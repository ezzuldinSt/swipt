import type { CallOptions, ObservableState, Point, Rect } from "./index";
import type { AccessibilityElement } from "./accessibility";
import type { FileHandle } from "./file-system";
import type { ImageDifference, RasterImage } from "./image";

export interface WaitOptions extends CallOptions {
  readonly region?: Rect;
  readonly timeoutMs?: number;
  readonly threshold?: number;
  /**
   * A change whose changed pixels lie in at most four spots, each changing no more than an
   * ignoreSize by ignoreSize square of points, does not count. waitForStable ignores 12 by
   * default, so a blinking caret, a clock hand or a digit of the status bar does not keep the
   * screen moving; waitForChange counts every change by default. 0 counts every change. The
   * macOS engine does not read it yet.
   */
  readonly ignoreSize?: number;
}

export interface ScreenChange {
  readonly time: number;
  readonly end: number;
  readonly frames: readonly Rect[];
  readonly app: string | null;
  readonly before: RasterImage;
  readonly after: RasterImage;
}

export type TimelineEvent =
  | {
      readonly time: number;
      readonly type: "operation";
      readonly name: string;
      readonly arguments?: readonly unknown[];
      readonly durationMs?: number;
      readonly error?: string;
      readonly message?: string;
      readonly swiplet?: string;
    }
  | {
      readonly time: number;
      readonly type: "action";
      readonly swiplet: string;
      readonly action: string;
      readonly caller: "app" | "shortcuts" | "api";
    }
  | { readonly time: number; readonly type: "foreground"; readonly app: string }
  | {
      readonly time: number;
      readonly type: "log";
      readonly swiplet: string;
      readonly level: "error" | "warn" | "info" | "debug";
      readonly message: string;
    }
  | {
      readonly time: number;
      readonly type: "feed";
      readonly frames: number;
      readonly decodedMs: number;
    };

export interface ScreenRecording {
  readonly file: FileHandle;
  readonly start: number | null;
  readonly duration: number;
  readonly width: number;
  readonly height: number;
  readonly scale: number;
  readonly changes: readonly ScreenChange[];
  readonly events: readonly TimelineEvent[];

  frame(time: number): RasterImage;
  frames(
    times:
      | readonly number[]
      | {
          readonly everyMs: number;
          readonly from?: number;
          readonly to?: number;
        },
  ): RasterImage[];
  grid(
    options?: CallOptions & {
      readonly times?: readonly number[];
      readonly columns?: number;
      readonly width?: number;
    },
  ): Promise<RasterImage>;
  diff(
    from: number,
    to: number,
    options?: CallOptions & { readonly threshold?: number },
  ): Promise<ImageDifference>;
  find(
    target: ScreenTarget | readonly ScreenTarget[],
    options?: Omit<ScreenFindOptions, "timeoutMs"> & {
      readonly from?: number;
      readonly to?: number;
    },
  ): Promise<(ScreenMatch & { readonly time: number }) | null>;
  export(
    options?: CallOptions & {
      /** Omitted, the export goes to a new file in cacheDirectory. */
      readonly file?: FileHandle | string;
      readonly from?: number;
      readonly to?: number;
      readonly region?: Rect;
      readonly scale?: number;
      readonly format?: "mov" | "mp4" | "gif";
      readonly frameRate?: number;
    },
  ): Promise<FileHandle>;
}

export interface RecordingSession {
  readonly file: FileHandle;
  readonly start: number;
  stop(options?: CallOptions): Promise<ScreenRecording>;
}

export type ScreenTarget = RasterImage | RegExp | string;

export interface ScreenFindOptions extends CallOptions {
  readonly region?: Rect;
  /** Omitted or 0 reads the latest frame once; a positive number or Infinity keeps reading frames until a match, and answers null at the limit. */
  readonly timeoutMs?: number;
  readonly threshold?: number;
  readonly scales?: readonly number[];
  readonly languages?: readonly string[];
  readonly minimumHeight?: number;
  /** Compares templates and the screen as black and white, split at this gray level, so a mark keeps matching over changing backgrounds. */
  readonly binarize?: number;
}

export interface ScreenMatch {
  readonly frame: Rect;
  readonly center: Point;
  readonly score: number;
  readonly text: string | null;
  readonly target: number;
  readonly image: RasterImage;
}

/** "accessibility": an element's label or value; "text": text read off the live frame; "lightText": text read off the frame with only its near-white pixels kept, as white labels on wallpapers. */
export type LocateSource = "accessibility" | "text" | "lightText";

export interface LocateOptions extends CallOptions {
  /** The lowest score answered; 0.75 when omitted. */
  readonly threshold?: number;
  readonly region?: Rect;
  readonly languages?: readonly string[];
}

export interface Located {
  /** The text that matched. */
  readonly text: string;
  readonly frame: Rect;
  /** Where a tap reaches the target: an element's point, or for text read off the frame the point of the element it belongs to (on the Home Screen and in the Dock the icon above its label), else the text's center. */
  readonly point: Point;
  /** From 0 to 1: how closely the text matches, less for text a hit test did not confirm and for the light-text pass. */
  readonly score: number;
  readonly source: LocateSource;
  /** The element came from accessibility, the text lies on or right under an element, or a hit test at the point reached an element with this text. */
  readonly confirmed: boolean;
  /** The element found by accessibility, or the one text read off the frame belongs to, such as the icon over a label. */
  readonly element?: AccessibilityElement;
}

/**
 * A lossless PNG of the screen from the device. Where the engine streams the screen (not on
 * macOS yet), a PNG taken earlier answers at once while the stream shows the screen unchanged
 * since it began, down to the last pixel; one is taken in the background once the screen has
 * stood still for half a second.
 */
export declare function screenshot(options?: CallOptions): Promise<RasterImage>;
export declare function find(
  target: ScreenTarget | readonly ScreenTarget[],
  options?: ScreenFindOptions,
): Promise<ScreenMatch | null>;
/**
 * Finds text where a tap reaches it: an element of the front app whose label or value is the text (case, spaces and punctuation aside), else the text read off the live frame, matched loosely, read again with only near-white pixels kept when the first reading misses it.
 * Text read off the frame is confirmed with a hit test; on the Home Screen a label leads to its icon. Answers null when nothing scores the threshold. Tap the answer with input.tap(located.point, { verify: true }).
 */
export declare function locate(
  target: string | RegExp,
  options?: LocateOptions,
): Promise<Located | null>;
export declare const frames: ObservableState<RasterImage>;
export declare function waitFor(
  predicate: (frame: RasterImage) => boolean | Promise<boolean>,
  options?: CallOptions & { readonly timeoutMs?: number },
): Promise<RasterImage>;
/**
 * The first frame that differs from `from`, or else from the latest frame, in at least
 * 1/5000 of the region's pixels by `threshold` gray levels (24), compared at a quarter of their
 * size. It answers as soon as the changed frame is decoded.
 */
export declare function waitForChange(
  options?: WaitOptions & { readonly from?: RasterImage },
): Promise<RasterImage>;
/**
 * The latest frame once the screen has not changed for `durationMs`: 100 ms by default (500 ms
 * on macOS), timed by the frames' arrival and over three frames at least. Each frame is compared
 * with the first of the quiet stretch, so a slow fade or the tail of a scroll counts. Small
 * changes that `ignoreSize` lets pass in three frames running lengthen the stretch by 200 ms,
 * and right after input the screen gets 400 ms to answer before an unchanged one counts as
 * settled.
 */
export declare function waitForStable(
  options?: WaitOptions & { readonly durationMs?: number },
): Promise<RasterImage>;
export declare function record(
  options?: CallOptions & {
    /** Omitted, the recording goes to a new file in cacheDirectory. */
    readonly file?: FileHandle | string;
  },
): Promise<RecordingSession>;
export declare function openRecording(
  file: FileHandle | string,
  options?: CallOptions,
): Promise<ScreenRecording>;
