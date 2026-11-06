import type { Rect } from "@swipt/api";
import type {
  ImageDifference,
  RasterImage as RasterImageInterface,
} from "@swipt/api/image";
import type {
  RecordingSession as RecordingSessionInterface,
  ScreenChange,
  ScreenFindOptions,
  ScreenMatch,
  ScreenRecording as ScreenRecordingInterface,
  ScreenTarget,
  TimelineEvent,
  WaitOptions,
} from "@swipt/api/screen";

import { stream } from "./async";
import { DOMException } from "./errors";
import { AbortController } from "./events";
import {
  cacheDirectory,
  FileHandle,
  fileOf,
  handleOfPath,
  pathOf,
} from "./files";
import {
  adopt,
  adoptOptional,
  finite,
  freeze,
  graphicsNative,
  handleOf,
  imageResult,
  RasterImage,
  rectOf,
  run,
  timeoutOf,
  type ImageHandle,
} from "./image";
import { check, optionsOf, signalOf, type Options } from "./options";

interface RecordingHandle {
  readonly path: string;
  readonly start: number;
}

interface VideoHandle {
  readonly info: string;
  frame(time: number): ImageHandle;
}

interface RawChange {
  time: number;
  end: number;
  before: number;
  after: number;
  frames: Rect[];
  app: string | null;
}

interface VideoInfo {
  path: string;
  start: number | null;
  duration: number;
  width: number;
  height: number;
  scale: number;
  changes: RawChange[];
  events: TimelineEvent[];
}

const largestFrameCount = 1000;

const clip = (text: string, limit: number) => {
  const line = text.replace(/\s*\n\s*/g, " ");
  return line.length > limit ? `${line.slice(0, limit - 1)}…` : line;
};

const timelineEntry = (
  event: TimelineEvent,
): { text: string; detail?: string } | undefined => {
  switch (event.type) {
    case "operation": {
      const detail =
        [
          event.durationMs === undefined
            ? undefined
            : `${Math.round(event.durationMs)} ms`,
          event.error === undefined
            ? undefined
            : `failed: ${clip(event.error, 200)}`,
        ]
          .filter((part) => part !== undefined)
          .join(", ") || undefined;
      if (event.name === "trace.screen.find") {
        const message = event.message ?? "";
        const end = message.lastIndexOf("; ");
        return {
          text: `screen.find ${end < 0 ? message : message.slice(0, end)}`,
          detail,
        };
      }
      if (event.name.startsWith("trace.")) {
        return undefined;
      }
      return {
        text: `${event.name}(${clip((event.arguments ?? []).map((value) => JSON.stringify(value)).join(", "), 160)})`,
        detail,
      };
    }
    case "action":
      return {
        text: `action ${event.action} of ${event.swiplet} from ${event.caller}`,
      };
    case "foreground":
      return { text: `foreground ${event.app}` };
    case "log":
      return {
        text: `${event.level} ${event.swiplet}: ${clip(event.message, 200)}`,
      };
    case "feed":
      return undefined;
  }
};

const defaultFile = (folder: string, extension: string): FileHandle =>
  cacheDirectory().file(
    `${folder}/${new Date().toISOString().replace(/[:.]/g, "-")}.${extension}`,
  );

const thresholdOf = (value: unknown): number | undefined => {
  if (value === undefined) {
    return undefined;
  }
  if (
    !Number.isInteger(value) ||
    (value as number) < 1 ||
    (value as number) > 255
  ) {
    throw new RangeError("threshold must be an integer from 1 to 255");
  }
  return value as number;
};

const ignoreSizeOf = (value: unknown): number | undefined => {
  if (value === undefined) {
    return undefined;
  }
  if (typeof value !== "number" || !Number.isFinite(value) || value < 0) {
    throw new RangeError("ignoreSize must be a non-negative number of points");
  }
  return value;
};

const durationOf = (value: unknown, name: string, fallback: number): number => {
  if (value === undefined) {
    return fallback;
  }
  if (typeof value !== "number" || !Number.isFinite(value) || value <= 0) {
    throw new RangeError(`${name} must be a positive number of milliseconds`);
  }
  return value;
};

const recordings = new WeakMap<RecordingSession, RecordingHandle>();
const videos = new WeakMap<ScreenRecording, VideoHandle>();

const videoOf = (id: number): ScreenRecording => {
  const handle = graphicsNative().take(id);
  if (handle === null) {
    throw new DOMException("the recording was released", "InvalidStateError");
  }
  return new ScreenRecording(handle as VideoHandle);
};

export class RecordingSession implements RecordingSessionInterface {
  declare readonly file: FileHandle;
  declare readonly start: number;

  constructor(handle: RecordingHandle) {
    recordings.set(this, handle);
    Object.defineProperties(this, {
      file: { value: handleOfPath(handle.path), enumerable: true },
      start: { value: handle.start, enumerable: true },
    });
    Object.freeze(this);
  }

  async stop(options?: Options): Promise<ScreenRecording> {
    const result = await run<{ video: number }>(
      "recording.stop",
      {},
      [recordings.get(this)!],
      signalOf(check(options, [])),
    );
    return videoOf(result.video);
  }

  toString(): string {
    return `RecordingSession ${this.file.name} since ${new Date(this.start).toISOString()}`;
  }
}

export class ScreenRecording implements ScreenRecordingInterface {
  declare readonly file: FileHandle;
  declare readonly start: number | null;
  declare readonly duration: number;
  declare readonly width: number;
  declare readonly height: number;
  declare readonly scale: number;
  declare readonly changes: readonly ScreenChange[];
  declare readonly events: readonly TimelineEvent[];

  constructor(handle: VideoHandle) {
    videos.set(this, handle);
    const info: VideoInfo = JSON.parse(handle.info);
    const frameAt = (time: number) => new RasterImage(handle.frame(time));
    const changes = info.changes.map((change) => {
      let before: RasterImage | undefined;
      let after: RasterImage | undefined;
      return Object.freeze(
        Object.defineProperties(
          {
            time: change.time,
            end: change.end,
            frames: freeze(change.frames),
            app: change.app,
          },
          {
            before: {
              get: () => (before ??= frameAt(change.before)),
              enumerable: true,
            },
            after: {
              get: () => (after ??= frameAt(change.after)),
              enumerable: true,
            },
          },
        ),
      );
    });
    Object.defineProperties(this, {
      file: { value: handleOfPath(info.path), enumerable: true },
      start: { value: info.start, enumerable: true },
      duration: { value: info.duration, enumerable: true },
      width: { value: info.width, enumerable: true },
      height: { value: info.height, enumerable: true },
      scale: { value: info.scale, enumerable: true },
      changes: { value: Object.freeze(changes), enumerable: true },
      events: { value: freeze(info.events), enumerable: true },
    });
    Object.freeze(this);
  }

  #time(value: unknown, name: string): number {
    return Math.min(Math.max(finite(value, name), 0), this.duration);
  }

  frame(time: number): RasterImage {
    return new RasterImage(
      videos.get(this)!.frame(this.#time(time, "the time")),
    );
  }

  frames(
    times:
      | readonly number[]
      | {
          readonly everyMs: number;
          readonly from?: number;
          readonly to?: number;
        },
  ): RasterImage[] {
    let chosen: number[];
    if (Array.isArray(times)) {
      chosen = times.map((time, index) =>
        this.#time(time, `time ${index + 1}`),
      );
    } else {
      const { everyMs, from, to } = check(times, ["everyMs", "from", "to"]) as {
        everyMs?: unknown;
        from?: unknown;
        to?: unknown;
      };
      const step = durationOf(everyMs, "everyMs", 1000);
      const first = from === undefined ? 0 : this.#time(from, "from");
      const last = to === undefined ? this.duration : this.#time(to, "to");
      if (last < first) {
        throw new RangeError("to must not be before from");
      }
      chosen = [];
      for (
        let time = first;
        time <= last && chosen.length <= largestFrameCount;
        time += step
      ) {
        chosen.push(time);
      }
    }
    if (chosen.length > largestFrameCount) {
      throw new RangeError(`at most ${largestFrameCount} frames at a time`);
    }
    return chosen.map((time) => this.frame(time));
  }

  async grid(
    options?: Options & {
      readonly times?: readonly number[];
      readonly columns?: number;
      readonly width?: number;
    },
  ): Promise<RasterImage> {
    const { times, columns, width } = optionsOf(options, [
      "times",
      "columns",
      "width",
    ]);
    const args: Record<string, unknown> = {};
    if (times !== undefined) {
      if (!Array.isArray(times) || times.length === 0 || times.length > 200) {
        throw new TypeError("times must be an array of 1 to 200 times");
      }
      args.times = times.map((time, index) =>
        this.#time(time, `time ${index + 1}`),
      );
    }
    if (columns !== undefined) {
      if (
        !Number.isInteger(columns) ||
        (columns as number) < 1 ||
        (columns as number) > 16
      ) {
        throw new RangeError("columns must be an integer from 1 to 16");
      }
      args.columns = columns;
    }
    if (width !== undefined) {
      if (
        !Number.isInteger(width) ||
        (width as number) < 32 ||
        (width as number) > 1024
      ) {
        throw new RangeError("width must be an integer from 32 to 1024 pixels");
      }
      args.width = width;
    }
    return imageResult(
      run("video.grid", args, [videos.get(this)!], signalOf(options)),
    );
  }

  async diff(
    from: number,
    to: number,
    options?: Options & { readonly threshold?: number },
  ) {
    const { threshold } = optionsOf(options, ["threshold"]);
    const args = {
      from: this.#time(from, "from"),
      to: this.#time(to, "to"),
      threshold: thresholdOf(threshold),
    };
    return freeze(
      await run<ImageDifference>(
        "video.diff",
        args,
        [videos.get(this)!],
        signalOf(options),
      ),
    );
  }

  async find(
    target: ScreenTarget | readonly ScreenTarget[],
    options?: Omit<ScreenFindOptions, "timeoutMs"> & {
      readonly from?: number;
      readonly to?: number;
    },
  ): Promise<(ScreenMatch & { readonly time: number }) | null> {
    const values = optionsOf(options, [...matchKeys, "from", "to"]);
    const { args, images } = matchArgs(target, values);
    if (values.from !== undefined) {
      args.from = this.#time(values.from, "from");
    }
    if (values.to !== undefined) {
      args.to = this.#time(values.to, "to");
    }
    return matchOf(
      await run<(RawMatch & { time: number }) | null>(
        "video.find",
        args,
        [videos.get(this)!, ...images],
        signalOf(options),
      ),
    );
  }

  async export(
    options?: Options & {
      readonly file?: unknown;
      readonly from?: number;
      readonly to?: number;
      readonly region?: Rect;
      readonly scale?: number;
      readonly format?: "mov" | "mp4" | "gif";
      readonly frameRate?: number;
    },
  ): Promise<FileHandle> {
    const {
      file,
      from,
      to,
      region,
      scale,
      format = "mp4",
      frameRate,
    } = optionsOf(options, [
      "file",
      "from",
      "to",
      "region",
      "scale",
      "format",
      "frameRate",
    ]);
    if (format !== "mov" && format !== "mp4" && format !== "gif") {
      throw new TypeError("the format must be mov, mp4 or gif");
    }
    const args: Record<string, unknown> = {
      format,
      path: pathOf(
        file === undefined
          ? defaultFile("Exports", format)
          : fileOf(file, "file"),
      ),
    };
    if (from !== undefined) {
      args.from = this.#time(from, "from");
    }
    if (to !== undefined) {
      args.to = this.#time(to, "to");
    }
    if (
      from !== undefined &&
      to !== undefined &&
      finite(to, "to") <= finite(from, "from")
    ) {
      throw new RangeError("to must be after from");
    }
    if ((args.to ?? this.duration) === (args.from ?? 0)) {
      throw new RangeError(
        `the range lies outside the recording, which runs from 0 to ${this.duration} ms`,
      );
    }
    if (region !== undefined) {
      args.region = rectOf(region, "region");
    }
    if (scale !== undefined) {
      if (typeof scale !== "number" || !(scale > 0 && scale <= 4)) {
        throw new RangeError("scale must be above 0 and at most 4");
      }
      args.scale = scale;
    }
    if (frameRate !== undefined) {
      if (
        typeof frameRate !== "number" ||
        !(frameRate > 0 && frameRate <= 120)
      ) {
        throw new RangeError("frameRate must be above 0 and at most 120");
      }
      args.frameRate = frameRate;
    }
    const result = await run<{ path: string }>(
      "video.export",
      args,
      [videos.get(this)!],
      signalOf(options),
    );
    return handleOfPath(result.path);
  }

  toString(): string {
    const entries: {
      time: number;
      end: number;
      text: string;
      detail?: string;
    }[] = [];
    for (const event of this.events) {
      const entry = timelineEntry(event);
      if (entry !== undefined) {
        entries.push({
          time: event.time,
          end:
            event.time +
            (event.type === "operation" ? (event.durationMs ?? 0) : 0),
          ...entry,
        });
      }
    }
    for (const change of this.changes as unknown as readonly RawChange[]) {
      const shown = change.frames
        .slice(0, 3)
        .map((frame) => `[${frame.map(Math.round).join(", ")}]`)
        .join(" ");
      const more =
        change.frames.length > 3 ? ` and ${change.frames.length - 3} more` : "";
      const app = change.app === null ? "" : ` in ${change.app}`;
      entries.push({
        time: change.time,
        end: change.end,
        text: `screen changed at ${shown}${more}${app}`,
        detail: `until ${Math.round(change.end)} ms`,
      });
    }
    entries.sort((a, b) => a.time - b.time);
    entries.push({
      time: this.duration,
      end: this.duration,
      text: "recording ends",
    });
    const folded: {
      time: number;
      end: number;
      text: string;
      detail?: string;
      count: number;
    }[] = [];
    for (const entry of entries) {
      const previous = folded.at(-1);
      if (previous !== undefined && previous.text === entry.text) {
        previous.end = Math.max(previous.end, entry.end);
        previous.count += 1;
      } else {
        folded.push({ ...entry, count: 1 });
      }
    }
    const width = Math.max(
      ...folded.map((entry) => String(Math.round(entry.time)).length),
    );
    return folded
      .map((entry) => {
        const suffix =
          entry.count > 1
            ? `, ${entry.count} times until ${Math.round(entry.end)} ms`
            : entry.detail === undefined
              ? ""
              : `, ${entry.detail}`;
        return `${String(Math.round(entry.time)).padStart(width)} ms  ${entry.text}${suffix}`;
      })
      .join("\n");
  }
}

const waitArgs = (options: unknown, extra: string[]) => {
  const { region, timeoutMs, threshold, ignoreSize, ...rest } = optionsOf(
    options,
    ["region", "timeoutMs", "threshold", "ignoreSize", ...extra],
  );
  return {
    rest,
    args: {
      region: region === undefined ? undefined : rectOf(region, "region"),
      timeoutMs: timeoutOf(timeoutMs),
      threshold: thresholdOf(threshold),
      ignoreSize: ignoreSizeOf(ignoreSize),
    } as Record<string, unknown>,
  };
};

export const frames = Object.freeze({
  get: (options?: Options) =>
    imageResult(run("screen.get", {}, [], signalOf(check(options, [])))),
  values: (options?: Options) =>
    stream<RasterImage>(
      "screen.watch",
      () => [graphicsNative().prepare("screen.watch", "{}", [])],
      signalOf(check(options, [])),
      "latest",
      adoptOptional,
    ),
});

const limited = (signal: AbortSignal | undefined, timeout: number | null) => {
  const controller = new AbortController();
  const abort = () => controller.abort(signal!.reason);
  if (signal?.aborted) {
    controller.abort(signal.reason);
  }
  signal?.addEventListener("abort", abort, { once: true });
  const timer =
    timeout === null
      ? undefined
      : setTimeout(
          () =>
            controller.abort(
              new DOMException(
                `no frame matched within ${timeout} ms`,
                "TimeoutError",
              ),
            ),
          timeout,
        );
  return {
    signal: controller.signal,
    done: () => {
      clearTimeout(timer);
      signal?.removeEventListener("abort", abort);
    },
  };
};

export const screenshot = (options?: Options) =>
  imageResult(run("screenshot", {}, [], signalOf(check(options, []))));

type RawMatch = Omit<ScreenMatch, "image"> & { image: number };

const findLimit = (value: unknown): number | null => {
  if (value === undefined || value === 0) {
    return 0;
  }
  if (value === Infinity) {
    return null;
  }
  if (typeof value !== "number" || !Number.isFinite(value) || value < 0) {
    throw new TypeError(
      "timeoutMs must be a non-negative number of milliseconds, or Infinity",
    );
  }
  return value;
};

const matchArgs = (
  target: ScreenTarget | readonly ScreenTarget[],
  options: Record<string, unknown>,
) => {
  const { region, threshold, scales, languages, minimumHeight, binarize } =
    options;
  const list = Array.isArray(target) ? target : [target];
  if (list.length === 0) {
    throw new TypeError("find needs a target");
  }
  const images: object[] = [];
  const targets = list.map((item, index) => {
    if (item instanceof RasterImage) {
      images.push(handleOf(item));
      return { kind: "image" };
    }
    if (item instanceof RegExp) {
      return { kind: "text", pattern: item.source, flags: item.flags };
    }
    if (typeof item === "string" && item !== "") {
      return {
        kind: "text",
        pattern: item.replace(/[.*+?^${}()|[\]\\]/g, "\\$&"),
        flags: "",
      };
    }
    throw new TypeError(
      `target ${index + 1} must be a RasterImage, a RegExp or a non-empty string`,
    );
  });
  const args: Record<string, unknown> = { targets };
  if (region !== undefined) {
    args.region = rectOf(region, "region");
  }
  if (threshold !== undefined) {
    if (typeof threshold !== "number" || !(threshold > 0 && threshold <= 1)) {
      throw new RangeError("threshold must be above 0 and at most 1");
    }
    args.threshold = threshold;
  }
  if (scales !== undefined) {
    if (
      !Array.isArray(scales) ||
      scales.length === 0 ||
      !scales.every((scale) => typeof scale === "number" && scale > 0)
    ) {
      throw new TypeError("scales must be an array of positive numbers");
    }
    args.scales = scales;
  }
  if (languages !== undefined) {
    if (
      !Array.isArray(languages) ||
      !languages.every((language) => typeof language === "string")
    ) {
      throw new TypeError("languages must be an array of strings");
    }
    args.languages = languages;
  }
  if (minimumHeight !== undefined) {
    if (typeof minimumHeight !== "number" || !(minimumHeight > 0)) {
      throw new RangeError("minimumHeight must be a positive number of points");
    }
    args.minimumHeight = minimumHeight;
  }
  if (binarize !== undefined) {
    if (
      !Number.isInteger(binarize) ||
      (binarize as number) < 1 ||
      (binarize as number) > 254
    ) {
      throw new RangeError("binarize must be an integer from 1 to 254");
    }
    args.binarize = binarize;
  }
  return { args, images };
};

const matchKeys = [
  "region",
  "threshold",
  "scales",
  "languages",
  "minimumHeight",
  "binarize",
];

const matchOf = <T extends { image: number }>(result: T | null) => {
  if (result === null) {
    return null;
  }
  const { image, ...match } = result;
  return freeze({ ...match, image: adopt(image) });
};

export const find = async (
  target: ScreenTarget | readonly ScreenTarget[],
  options?: ScreenFindOptions,
): Promise<ScreenMatch | null> => {
  const values = optionsOf(options, [...matchKeys, "timeoutMs"]);
  const { args, images } = matchArgs(target, values);
  args.timeoutMs = findLimit(values.timeoutMs);
  return matchOf(
    await run<RawMatch | null>("screen.find", args, images, signalOf(options)),
  );
};

export const waitFor = async (
  predicate: (frame: RasterImage) => boolean | Promise<boolean>,
  options?: Options & { readonly timeoutMs?: number },
): Promise<RasterImage> => {
  if (typeof predicate !== "function") {
    throw new TypeError("the predicate must be a function");
  }
  const { timeoutMs } = optionsOf(options, ["timeoutMs"]);
  const limit = limited(signalOf(options), timeoutOf(timeoutMs));
  try {
    for await (const frame of frames.values({ signal: limit.signal })) {
      if (await predicate(frame)) {
        return frame;
      }
    }
  } finally {
    limit.done();
  }
  throw new DOMException("the screen stream ended", "UnavailableError");
};

export const waitForChange = (
  options?: WaitOptions & { readonly from?: RasterImageInterface },
) => {
  const { rest, args } = waitArgs(options, ["from"]);
  const objects = rest.from === undefined ? [] : [handleOf(rest.from, "from")];
  return imageResult(
    run("screen.waitForChange", args, objects, signalOf(options)),
  );
};

export const waitForStable = (
  options?: WaitOptions & { readonly durationMs?: number },
) => {
  const { rest, args } = waitArgs(options, ["durationMs"]);
  // Without one, each engine waits its own quiet time: 500 ms on macOS, 100 ms elsewhere,
  // where every frame is compared with the quiet stretch's first.
  args.durationMs =
    rest.durationMs === undefined
      ? undefined
      : durationOf(rest.durationMs, "durationMs", 0);
  return imageResult(run("screen.waitForStable", args, [], signalOf(options)));
};

export const record = async (
  options?: Options & { readonly file?: unknown },
): Promise<RecordingSession> => {
  const { file } = optionsOf(options, ["file"]);
  const target =
    file === undefined
      ? defaultFile("Recordings", "mov")
      : fileOf(file, "file");
  const path = pathOf(target);
  if (!path.toLowerCase().endsWith(".mov")) {
    throw new TypeError("a recording is a .mov file");
  }
  const result = await run<{ recording: number }>(
    "record",
    { path },
    [],
    signalOf(options),
  );
  const handle = graphicsNative().take(result.recording);
  if (handle === null) {
    throw new DOMException("the recording was released", "InvalidStateError");
  }
  return new RecordingSession(handle as RecordingHandle);
};

export const openRecording = async (
  file: unknown,
  options?: Options,
): Promise<ScreenRecording> => {
  const result = await run<{ video: number }>(
    "video.open",
    { path: pathOf(fileOf(file, "the file")) },
    [],
    signalOf(check(options, [])),
  );
  return videoOf(result.video);
};
