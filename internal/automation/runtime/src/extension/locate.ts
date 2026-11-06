import type { Point, Rect } from "@swipt/api";
import type { RecognizedTextLine } from "@swipt/api/image";
import type { Located, LocateSource } from "@swipt/api/screen";

import { find, hitTest, type HitElement } from "./accessibility";
import { invoke } from "./async";
import type { AccessibilityElement } from "./elements";
import { imageResult, rectOf, run, type RasterImage } from "./image";
import { optionsOf, signalOf, type Options } from "./options";

interface Token {
  text: string;
  start: number;
  end: number;
}

const tokens = (text: string): Token[] =>
  [...text.matchAll(/[\p{L}\p{N}]+/gu)].map((match) => ({
    text: match[0].toLowerCase(),
    start: match.index,
    end: match.index + match[0].length,
  }));

const distance = (first: string, second: string): number => {
  const a = [...first];
  const b = [...second];
  let previous = Array.from({ length: b.length + 1 }, (_, index) => index);
  for (let i = 1; i <= a.length; i++) {
    const current = [i];
    for (let j = 1; j <= b.length; j++) {
      current[j] = Math.min(
        previous[j] + 1,
        current[j - 1] + 1,
        previous[j - 1] + (a[i - 1] === b[j - 1] ? 0 : 1),
      );
    }
    previous = current;
  }
  return previous[b.length];
};

const similarity = (first: string, second: string): number => {
  if (first === second) {
    return 1;
  }
  const longest = Math.max([...first].length, [...second].length);
  return longest === 0 ? 0 : 1 - distance(first, second) / longest;
};

interface Run {
  score: number;
  start: number;
  end: number;
}

// bestRun scores the runs of a text's words against the target's, one word
// fewer to one word more than it has, also with the spaces taken out, as OCR
// splits "YouTube" into "YouTu be"; a run short of the whole text scores a
// little less than the whole text alike.
const bestRun = (target: Token[], text: string): Run | null => {
  const words = tokens(text);
  const wanted = target.map((word) => word.text).join(" ");
  const joined = wanted.replace(/ /g, "");
  let best: Run | null = null;
  for (
    let size = Math.max(1, target.length - 1);
    size <= target.length + 1;
    size++
  ) {
    for (let first = 0; first + size <= words.length; first++) {
      const span = words.slice(first, first + size);
      const spanText = span.map((word) => word.text).join(" ");
      let score = Math.max(
        similarity(spanText, wanted),
        similarity(spanText.replace(/ /g, ""), joined) * 0.98,
      );
      if (size < words.length) {
        score *= 0.98;
      }
      if (best === null || score > best.score) {
        best = { score, start: span[0].start, end: span[span.length - 1].end };
      }
    }
  }
  return best;
};

const textScore = (target: string | RegExp, text: string | undefined) => {
  if (text === undefined || text === "") {
    return 0;
  }
  if (target instanceof RegExp) {
    return text.search(target) >= 0 ? 1 : 0;
  }
  return bestRun(tokens(target), text)?.score ?? 0;
};

const escaped = (text: string) => text.replace(/[.*+?^${}()|[\]\\]/g, "\\$&");
const gap = "[^\\p{L}\\p{N}]";

// The accessibility selectors, best first: a label of the target's words
// alone, a value of them alone, then a label holding them.
const selectorsOf = (target: string | RegExp) => {
  if (target instanceof RegExp) {
    return [{ label: target }, { value: target }];
  }
  const words = tokens(target).map((word) => escaped(word.text));
  const body = words.join(`${gap}+`);
  const alone = new RegExp(`^${gap}*${body}${gap}*$`, "iu");
  return [
    { label: alone },
    { value: alone },
    { label: new RegExp(`(?:^|${gap})${body}(?:${gap}|$)`, "iu") },
  ];
};

const accessibilityScores = [1, 0.95, 0.9];

const springboard = "com.apple.springboard";

// A Home Screen icon's label is not where a tap opens it: tapping the label
// "Clock" at its OCR center did nothing, while 45 pt above it, on the icon,
// opened Clock (iPhone18,2, iOS 27.0.1 24A446).
const iconLift = 45;
const iconLabelHeight = 20;

// Text OCR found counts this much less than an element's, unless a hit test
// confirms it; a light-text pass counts a little less again.
const unconfirmed = 0.9;
const lightText = 0.95;

interface Candidate {
  line: RecognizedTextLine;
  run: Run;
  source: LocateSource;
}

const bestLine = (
  target: string | RegExp,
  lines: readonly RecognizedTextLine[],
  source: LocateSource,
): Candidate | null => {
  let best: Candidate | null = null;
  for (const line of lines) {
    let found: Run | null;
    if (target instanceof RegExp) {
      const match = new RegExp(
        target.source,
        target.flags.replace(/[gy]/g, ""),
      ).exec(line.text);
      found =
        match === null
          ? null
          : {
              score: 1,
              start: match.index,
              end: match.index + match[0].length,
            };
    } else {
      found = bestRun(tokens(target), line.text);
    }
    if (found !== null && (best === null || found.score > best.run.score)) {
      best = { line, run: found, source };
    }
  }
  return best;
};

const hitScore = (target: string | RegExp, hit: HitElement | null) =>
  hit === null
    ? 0
    : Math.max(textScore(target, hit.label), textScore(target, hit.value));

export const locate = async (
  target: unknown,
  options?: Options,
): Promise<Located | null> => {
  if (
    !(target instanceof RegExp) &&
    (typeof target !== "string" || tokens(target).length === 0)
  ) {
    throw new TypeError(
      "the target must be a RegExp or a string with letters or digits",
    );
  }
  const {
    threshold = 0.75,
    region,
    languages,
  } = optionsOf(options, ["threshold", "region", "languages"]);
  if (typeof threshold !== "number" || !(threshold > 0 && threshold <= 1)) {
    throw new RangeError("threshold must be above 0 and at most 1");
  }
  const area = region === undefined ? undefined : rectOf(region, "region");
  const signal = signalOf(options);
  const inside = (at: Point | undefined) =>
    at !== undefined &&
    (area === undefined ||
      (at[0] >= area[0] &&
        at[0] <= area[0] + area[2] &&
        at[1] >= area[1] &&
        at[1] <= area[1] + area[3]));

  const selectors = selectorsOf(target);
  const element: AccessibilityElement | null = await find(selectors, {
    signal,
  });
  if (
    element !== null &&
    inside(element.point) &&
    element.frame !== undefined
  ) {
    const index = Math.max(
      selectors.findIndex((selector) => {
        const [field, pattern] = Object.entries(selector)[0] as [
          "label" | "value",
          RegExp,
        ];
        const text = element[field];
        return text !== undefined && text.search(pattern) >= 0;
      }),
      0,
    );
    return Object.freeze({
      text: (index === 1 ? element.value : element.label) ?? "",
      frame: element.frame,
      point: element.point!,
      score: accessibilityScores[index] ?? 1,
      source: "accessibility",
      confirmed: true,
      element,
    });
  }

  const frame: RasterImage = await imageResult(
    run("screen.get", {}, [], signal),
  );
  const read = (image: RasterImage) =>
    image.recognizeText({
      signal,
      ...(area === undefined ? {} : { region: area }),
      ...(languages === undefined ? {} : { languages: languages as string[] }),
    });
  let best = bestLine(target, await read(frame), "text");
  if (best === null || best.run.score < 0.9) {
    const light = bestLine(
      target,
      await read(
        frame
          .mask([
            [200, 200, 200],
            [255, 255, 255],
          ])
          .invert(),
      ),
      "lightText",
    );
    if (
      light !== null &&
      (best === null || light.run.score * lightText > best.run.score)
    ) {
      best = light;
    }
  }
  if (best === null) {
    return null;
  }
  const { line, run: span, source } = best;
  const box = line.boundingBox(span.start, span.end) ?? line;
  const center = box.center;
  const front = await invoke<string>("device.state", ["foreground"], {
    signal,
  }).catch(() => "");
  const icon: Point = [center[0], center[1] - iconLift];
  const home = front === springboard && icon[1] > 0;
  const hits = await hitTest(home ? [center, icon] : [center], signal).catch(
    () => [] as (HitElement | null)[],
  );
  let point = center;
  let confirmed = false;
  if (home && hitScore(target, hits[1] ?? null) >= threshold) {
    point = icon;
    confirmed = true;
  } else if (hitScore(target, hits[0] ?? null) >= threshold) {
    confirmed = true;
  } else if (
    home &&
    box.frame[3] <= iconLabelHeight &&
    tokens(line.text).length <= 3
  ) {
    point = icon;
  }
  let score = span.score * (confirmed ? 1 : unconfirmed);
  if (source === "lightText") {
    score *= lightText;
  }
  if (score < threshold) {
    return null;
  }
  return Object.freeze({
    text: line.text.slice(span.start, span.end),
    frame: box.frame as Rect,
    point,
    score: Math.round(score * 1000) / 1000,
    source,
    confirmed,
  });
};
