import type { Point } from "@swipt/api";
import type {
  Observation,
  TapAttempt,
  TapEffect,
  TapResult,
} from "@swipt/api/accessibility";

import { invoke } from "./async";
import {
  AccessibilityElement,
  encode,
  isSelector,
  pick,
  refOf,
  snapshotOf,
  type RawSnapshot,
  type Selector,
} from "./elements";
import { errorFrom } from "./errors";
import { imageResult, run, type RasterImage } from "./image";
import { act, check, optionsOf, signalOf, type Options } from "./options";

const view = (options: unknown) => {
  const { scope = "frontApp", full = false } = optionsOf(options, [
    "scope",
    "full",
  ]);
  if (scope !== "frontApp" && scope !== "screen") {
    throw new TypeError("scope must be frontApp or screen");
  }
  if (typeof full !== "boolean") {
    throw new TypeError("full must be a boolean");
  }
  return { scope, full };
};

const selectorsOf = (selector: unknown): Selector[] => {
  const selectors = Array.isArray(selector) ? selector : [selector];
  if (selectors.length === 0) {
    throw new TypeError("find needs a selector");
  }
  for (const item of selectors) {
    if (!isSelector(item)) {
      throw new TypeError(
        "expected a selector: an object of element fields, or a function of an element",
      );
    }
  }
  return selectors as Selector[];
};

const limitOf = (value: unknown, name: string): number => {
  if (value === undefined) {
    return 0;
  }
  if (value === Infinity) {
    return Infinity;
  }
  if (typeof value !== "number" || !Number.isFinite(value) || value < 0) {
    throw new TypeError(
      `${name} must be a non-negative number of milliseconds, or Infinity`,
    );
  }
  return value;
};

const flagOf = (value: unknown, name: string, fallback: boolean): boolean => {
  if (value === undefined) {
    return fallback;
  }
  if (typeof value !== "boolean") {
    throw new TypeError(`${name} must be a boolean`);
  }
  return value;
};

interface Found {
  snapshot: RawSnapshot | null;
  seen: number;
}

interface Search {
  mode: "best" | "unique";
  timeout: number;
  pollLimit: number;
  signal?: AbortSignal;
  scroll?: boolean;
}

const pickFrom = (
  raw: RawSnapshot | null,
  selectors: Selector[],
  mode: "best" | "unique",
): AccessibilityElement | null => {
  if (raw === null) {
    return null;
  }
  const taken = snapshotOf(raw);
  for (const item of selectors) {
    const element = pick(taken, item, mode);
    if (element !== null) {
      return element;
    }
  }
  return null;
};

// locate reads the screen until one of the selectors matches, taking the
// match the ranking puts first; in "unique" mode a tie throws. With scroll, a
// read that matches nothing goes on to search the pages of the screen.
const locate = async (
  selectors: Selector[],
  { mode, timeout, pollLimit, signal, scroll = false }: Search,
): Promise<AccessibilityElement | null> => {
  const encoded = selectors.map((item) => encode(item).value);
  const started = Date.now();
  const deadline = started + timeout;
  const pollUntil = started + pollLimit;
  for (;;) {
    const found = await invoke<Found>("accessibility.find", [encoded], {
      signal,
    });
    const element = pickFrom(found.snapshot, selectors, mode);
    if (element !== null) {
      return element;
    }
    const now = Date.now();
    if (now >= deadline) {
      return scroll
        ? searchPages(selectors, encoded, mode, found.snapshot, signal)
        : null;
    }
    if (now < pollUntil) {
      continue;
    }
    await invoke(
      "accessibility.changed",
      [found.seen, deadline === Infinity ? null : deadline - now],
      { signal },
    );
  }
};

// The pages a search goes through each way before it gives up.
const pageLimit = 25;

const describeSelector = (selector: Selector): string => {
  if (typeof selector === "function") {
    return "a function";
  }
  const fields = Object.entries(selector).map(([key, value]) => {
    if (value instanceof RegExp) {
      return `${key}: ${String(value)}`;
    }
    if (value instanceof AccessibilityElement || isSelector(value)) {
      return `${key}: …`;
    }
    return `${key}: ${JSON.stringify(value)}`;
  });
  return `{ ${fields.join(", ")} }`;
};

// searchPages scrolls the largest scroll view of the screen a page at a time,
// down to its end and then up to its top, reading the screen after each page,
// and throws NotFoundError naming the scroll view and the pages it went
// through when nothing matches.
const searchPages = async (
  selectors: Selector[],
  encoded: unknown[],
  mode: "best" | "unique",
  first: RawSnapshot | null,
  signal?: AbortSignal,
): Promise<AccessibilityElement> => {
  const wanted = selectors.map(describeSelector).join(" or ");
  if (first === null) {
    throw errorFrom({
      name: "NotFoundError",
      message: `no element matches ${wanted}, and the screen could not be read to scroll it`,
    });
  }
  let raw = first;
  let container = "the screen";
  const went: string[] = [];
  for (const direction of ["down", "up"]) {
    let pages = 0;
    let ended = false;
    while (pages < pageLimit) {
      let paged: { moved: boolean; container: string };
      try {
        paged = await invoke(
          "accessibility.scrollPage",
          [raw.root.ref, direction],
          {
            signal,
          },
        );
      } catch (error) {
        if ((error as Error).name !== "NotFoundError") {
          throw error;
        }
        throw errorFrom({
          name: "NotFoundError",
          message: `no element matches ${wanted}, and ${(error as Error).message}`,
        });
      }
      container = paged.container;
      if (!paged.moved) {
        ended = true;
        break;
      }
      pages++;
      const found = await invoke<Found>("accessibility.find", [encoded], {
        signal,
      });
      const element = pickFrom(found.snapshot, selectors, mode);
      if (element !== null) {
        return element;
      }
      if (found.snapshot === null) {
        throw errorFrom({
          name: "NotFoundError",
          message: `no element matches ${wanted}; after ${pages} ${pages === 1 ? "page" : "pages"} ${direction} of ${container} another application came to the front`,
        });
      }
      raw = found.snapshot;
    }
    const end = direction === "down" ? "to its end" : "to its top";
    went.push(
      `${pages} ${pages === 1 ? "page" : "pages"} ${direction}${ended ? ` ${end}` : ` (the most a search scrolls)`}`,
    );
  }
  throw errorFrom({
    name: "NotFoundError",
    message: `no element matches ${wanted} in ${container}, scrolled ${went.join(" and then ")}`,
  });
};

const elementOf = (element: unknown, operation: string): string | Selector => {
  if (element instanceof AccessibilityElement) {
    return refOf(element);
  }
  if (!isSelector(element)) {
    throw new TypeError(
      `${operation} takes an element of a snapshot or a selector`,
    );
  }
  return element;
};

// on runs an action on an element, or on the one element a selector means,
// found within the time limit, with the time left.
const on = <T>(
  element: string | Selector,
  operation: string,
  limit: number,
  options: unknown,
  scroll: boolean,
  run: (ref: string, limit: number) => Promise<T>,
): Promise<T> => {
  if (typeof element === "string") {
    return run(element, limit);
  }
  const started = Date.now();
  return locate([element], {
    mode: "unique",
    timeout: limit,
    pollLimit: -Infinity,
    signal: signalOf(options),
    scroll,
  }).then((found) => {
    if (found === null) {
      throw errorFrom({
        name: "NotFoundError",
        message: `${operation}: no element matches the selector${limit > 0 ? ` within ${limit} ms` : ""}`,
      });
    }
    return run(refOf(found), Math.max(limit - (Date.now() - started), 0));
  });
};

export const snapshot = async (options?: Options) =>
  snapshotOf(
    await invoke<RawSnapshot>("accessibility.snapshot", [view(options)], {
      signal: signalOf(options),
    }),
  );

export const find = async (
  selector: unknown,
  options?: Options,
): Promise<AccessibilityElement | null> => {
  const selectors = selectorsOf(selector);
  const {
    timeoutMs,
    poll = false,
    pollTimeoutMs,
    scroll,
  } = optionsOf(options, ["timeoutMs", "poll", "pollTimeoutMs", "scroll"]);
  const timeout = limitOf(timeoutMs, "timeoutMs");
  if (typeof poll !== "boolean") {
    throw new TypeError("poll must be a boolean");
  }
  if (pollTimeoutMs !== undefined && !poll) {
    throw new TypeError("pollTimeoutMs applies with poll");
  }
  const pollLimit =
    pollTimeoutMs === undefined
      ? Infinity
      : limitOf(pollTimeoutMs, "pollTimeoutMs");
  return locate(selectors, {
    mode: "best",
    timeout,
    pollLimit: poll ? pollLimit : -Infinity,
    signal: signalOf(options),
    scroll: flagOf(scroll, "scroll", false),
  });
};

export const setText = (element: unknown, text: unknown, options?: Options) => {
  const target = elementOf(element, "accessibility.setText");
  if (typeof text !== "string") {
    throw new TypeError("the text must be a string");
  }
  const { timeoutMs } = optionsOf(options, ["timeoutMs"]);
  const limit = limitOf(timeoutMs, "timeoutMs");
  return on(
    target,
    "accessibility.setText",
    limit,
    options,
    false,
    (ref, left) =>
      act(
        "accessibility.setText",
        [ref, text, left === Infinity ? null : left],
        options,
      ),
  );
};

export type Observe = "snapshot" | "frame";

export const observeOf = (value: unknown): Observe | undefined => {
  if (value !== undefined && value !== "snapshot" && value !== "frame") {
    throw new TypeError('observe must be "snapshot" or "frame"');
  }
  return value;
};

// After an action, the screen counts as settled once it has not changed for
// settleQuiet, and observing gives up waiting for that after settleLimit.
const settleQuiet = 300;
const settleLimit = 3000;

// observed waits for the screen to settle after an action, and adds the frame
// it settled on or a snapshot read then.
export const observed = async <T extends object>(
  result: T,
  observe: Observe | undefined,
  signal?: AbortSignal,
): Promise<T & Observation> => {
  if (observe === undefined) {
    return Object.freeze(result);
  }
  let frame: RasterImage;
  let settled = true;
  try {
    frame = await imageResult(
      run(
        "screen.waitForStable",
        { durationMs: settleQuiet, timeoutMs: settleLimit },
        [],
        signal,
      ),
    );
  } catch (error) {
    if ((error as Error).name !== "TimeoutError") {
      throw error;
    }
    settled = false;
    frame = await imageResult(run("screen.get", {}, [], signal));
  }
  if (observe === "frame") {
    return Object.freeze({ ...result, settled, frame });
  }
  return Object.freeze({
    ...result,
    settled,
    snapshot: await snapshot({ signal }),
  });
};

export interface RawTap {
  element?: string;
  point?: Point;
  effect?: TapEffect;
  detail?: string;
  attempts?: TapAttempt[];
}

export const tapResultOf = (raw: RawTap | null): TapResult => {
  const out: Record<string, unknown> = {};
  for (const [key, value] of Object.entries(raw ?? {})) {
    if (value !== undefined && value !== null) {
      out[key] =
        key === "attempts"
          ? Object.freeze(
              (value as TapAttempt[]).map((attempt) => Object.freeze(attempt)),
            )
          : value;
    }
  }
  return out as TapResult;
};

export const tap = async (element: unknown, options?: Options) => {
  const target = elementOf(element, "accessibility.tap");
  const { timeoutMs, verify, recover, observe, scroll } = optionsOf(options, [
    "timeoutMs",
    "verify",
    "recover",
    "observe",
    "scroll",
  ]);
  const limit = limitOf(timeoutMs, "timeoutMs");
  const checked = flagOf(verify, "verify", true);
  const recovering = flagOf(recover, "recover", checked);
  if (recovering && !checked) {
    throw new TypeError("recover applies with verify");
  }
  const watched = observeOf(observe);
  const signal = signalOf(options);
  const raw = await on(
    target,
    "accessibility.tap",
    limit,
    options,
    flagOf(scroll, "scroll", false),
    (ref, left) =>
      invoke<RawTap | null>(
        "accessibility.tap",
        [
          ref,
          left === Infinity ? null : left,
          { verify: checked, recover: recovering },
        ],
        { signal },
      ),
  );
  return observed(tapResultOf(raw), watched, signal);
};

export const perform = (
  element: unknown,
  action: unknown,
  options?: Options,
) => {
  const target = elementOf(element, "accessibility.perform");
  if (typeof action !== "string") {
    throw new TypeError("the action must be a string");
  }
  const checked = check(options, []);
  return on(target, "accessibility.perform", 0, checked, false, (ref) =>
    act("accessibility.perform", [ref, action], checked),
  );
};

export interface HitElement {
  label?: string;
  value?: string;
  identifier?: string;
  role: string;
  frame?: [number, number, number, number];
  point?: Point;
}

// hitTest names the element a tap at each point would reach.
export const hitTest = (points: Point[], signal?: AbortSignal) =>
  invoke<(HitElement | null)[]>("accessibility.hitTest", [points], { signal });
