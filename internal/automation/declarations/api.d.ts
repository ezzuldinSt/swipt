declare const accessibility: typeof import("@swipt/api/accessibility");
declare const input: typeof import("@swipt/api/input");
declare const device: typeof import("@swipt/api/device");
declare var screen: Screen;
declare interface Screen extends Readonly<typeof import("@swipt/api/screen")> {}
declare const image: typeof import("@swipt/api/image");
declare const dataDirectory: import("@swipt/api/file-system").DirectoryHandle;
declare const appIntents: typeof import("@swipt/api/app-intents");
declare const args: readonly string[];

declare function show(image: import("@swipt/api/image").RasterImage): void;

declare module "@swipt/api" {
  export interface CallOptions {
    readonly signal?: AbortSignal;
  }

  export interface ObservableState<T> {
    get(options?: CallOptions): Promise<T>;
    values(options?: CallOptions): AsyncIterable<T>;
  }

  export type Point = [x: number, y: number];
  export type Rect = [x: number, y: number, width: number, height: number];
  export type Quad = [
    topLeft: Point,
    topRight: Point,
    bottomRight: Point,
    bottomLeft: Point,
  ];

  export type ErrorName =
    "AbortError" | "TimeoutError" | "NotFoundError" | "UnavailableError";

  export type JSONValue =
    string | number | boolean | null | JSONValue[] | { [key: string]: JSONValue };

  export type Jsonify<T> = T extends string | number | boolean | null
    ? T
    : T extends undefined | symbol | ((...args: any[]) => unknown)
      ? never
      : T extends { toJSON(): infer J }
        ? Jsonify<J>
        : T extends readonly (infer U)[]
          ? Jsonify<U>[]
          : T extends object
            ? { [K in keyof T]: Jsonify<Exclude<T[K], undefined>> }
            : never;

  global {
    function sleep(ms: number, options?: CallOptions): Promise<void>;
  }
}

declare module "@swipt/api/swiplet" {
  interface StandardTypedV1<Input = unknown, Output = Input> {
    readonly "~standard": StandardTypedV1.Props<Input, Output>;
  }
  namespace StandardTypedV1 {
    interface Props<Input = unknown, Output = Input> {
      readonly version: 1;
      readonly vendor: string;
      readonly types?: Types<Input, Output> | undefined;
    }
    interface Types<Input = unknown, Output = Input> {
      readonly input: Input;
      readonly output: Output;
    }
    type InferInput<Schema extends StandardTypedV1> = NonNullable<Schema["~standard"]["types"]>["input"];
    type InferOutput<Schema extends StandardTypedV1> = NonNullable<Schema["~standard"]["types"]>["output"];
  }
  interface StandardSchemaV1<Input = unknown, Output = Input> {
    readonly "~standard": StandardSchemaV1.Props<Input, Output>;
  }
  namespace StandardSchemaV1 {
    interface Props<Input = unknown, Output = Input> extends StandardTypedV1.Props<Input, Output> {
      readonly validate: (value: unknown, options?: StandardSchemaV1.Options | undefined) => Result<Output> | Promise<Result<Output>>;
    }
    type Result<Output> = SuccessResult<Output> | FailureResult;
    interface SuccessResult<Output> {
      readonly value: Output;
      readonly issues?: undefined;
    }
    interface Options {
      readonly libraryOptions?: Record<string, unknown> | undefined;
    }
    interface FailureResult {
      readonly issues: ReadonlyArray<Issue>;
    }
    interface Issue {
      readonly message: string;
      readonly path?: ReadonlyArray<PropertyKey | PathSegment> | undefined;
    }
    interface PathSegment {
      readonly key: PropertyKey;
    }
    interface Types<Input = unknown, Output = Input> extends StandardTypedV1.Types<Input, Output> {
    }
    type InferInput<Schema extends StandardTypedV1> = StandardTypedV1.InferInput<Schema>;
    type InferOutput<Schema extends StandardTypedV1> = StandardTypedV1.InferOutput<Schema>;
  }
  interface StandardJSONSchemaV1<Input = unknown, Output = Input> {
    readonly "~standard": StandardJSONSchemaV1.Props<Input, Output>;
  }
  namespace StandardJSONSchemaV1 {
    interface Props<Input = unknown, Output = Input> extends StandardTypedV1.Props<Input, Output> {
      readonly jsonSchema: StandardJSONSchemaV1.Converter;
    }
    interface Converter {
      readonly input: (options: StandardJSONSchemaV1.Options) => Record<string, unknown>;
      readonly output: (options: StandardJSONSchemaV1.Options) => Record<string, unknown>;
    }
    type Target = "draft-2020-12" | "draft-07" | "openapi-3.0" | ({} & string);
    interface Options {
      readonly target: Target;
      readonly libraryOptions?: Record<string, unknown> | undefined;
    }
    interface Types<Input = unknown, Output = Input> extends StandardTypedV1.Types<Input, Output> {
    }
    type InferInput<Schema extends StandardTypedV1> = StandardTypedV1.InferInput<Schema>;
    type InferOutput<Schema extends StandardTypedV1> = StandardTypedV1.InferOutput<Schema>;
  }

  import type { Jsonify } from "@swipt/api";

  export type ActionCaller = "app" | "shortcuts" | "api";

  export type Schema<Input = unknown, Output = Input> = StandardSchemaV1<
    Input,
    Output
  > &
    StandardJSONSchemaV1<Input, Output>;

  const types: unique symbol;

  export interface Action<Input = void, Output = void> {
    readonly [types]: { input: Input; output: Output };
    readonly title: string;
    readonly description?: string;
    readonly expose: readonly "shortcuts"[];
  }

  export interface Extension<
    Actions extends Record<string, Action<any, any>> = {},
    HasRun extends boolean = boolean,
  > {
    readonly [types]: { hasRun: HasRun };
    readonly actions: Actions;
  }

  export interface RunContext {
    readonly signal: AbortSignal;
  }

  export interface ActionContext extends RunContext {
    readonly caller: ActionCaller;
  }

  type Received<S> = S extends Schema ? StandardSchemaV1.InferOutput<S> : void;
  type Sent<S> = S extends Schema ? StandardSchemaV1.InferInput<S> : void;
  type Returned<S, R> = S extends Schema ? StandardSchemaV1.InferInput<S> : R;
  type Delivered<S, R> = S extends Schema ? StandardSchemaV1.InferOutput<S> : R;

  export function defineAction<
    I extends Schema | undefined = undefined,
    O extends Schema | undefined = undefined,
    R = void,
  >(definition: {
    readonly title: string;
    readonly description?: string;
    readonly input?: I;
    readonly output?: O;
    readonly expose?: readonly "shortcuts"[];
    run(
      input: Received<I>,
      context: ActionContext,
    ): Returned<O, R> | Promise<Returned<O, R>>;
  }): Action<Sent<I>, Jsonify<Awaited<Delivered<O, R>>>>;

  export function defineExtension<
    Actions extends Record<string, Action<any, any>> = {},
  >(definition: {
    run(context: RunContext): Promise<void>;
    readonly actions?: Actions;
  }): Extension<Actions, true>;
  export function defineExtension<
    Actions extends Record<string, Action<any, any>> = {},
  >(definition: { readonly actions?: Actions }): Extension<Actions, false>;

  export type ServiceStatus = "starting" | "running" | "stopped" | "failed";

  type ActionCall<A> =
    A extends Action<infer Input, infer Output>
      ? [Input] extends [void]
        ? (
            input?: undefined,
            options?: { signal?: AbortSignal },
          ) => Promise<Output>
        : (input: Input, options?: { signal?: AbortSignal }) => Promise<Output>
      : never;

  export type ActionCalls<E extends Extension<any, any>> = {
    readonly [K in keyof E["actions"]]: ActionCall<E["actions"][K]>;
  };

  export interface ServiceHandle {
    readonly enabled: boolean;
    readonly status: ServiceStatus;
    readonly error: string | null;
    readonly since: Date | null;
    setEnabled(enabled: boolean): Promise<void>;
  }

  export function actions<
    E extends Extension<any, any>,
  >(): ActionCalls<E>;

  export function useService<
    E extends Extension<any, true>,
  >(): ServiceHandle;

  export function exit(): void;

  export {};
}

declare module "@swipt/api/file-system" {
  export const dataDirectory: DirectoryHandle;
  /**
   * Holds the files Swipt keeps for the Swiplet by default: screen recordings and their exports, and application icons.
   * The system may delete its contents when storage runs low; clearing the Swiplet's data or deleting the Swiplet removes it.
   */
  export const cacheDirectory: DirectoryHandle;
  export const assets: DirectoryHandle;

  export interface FileStat {
    readonly size: number;
    readonly modified: number;
  }

  export interface DirectoryHandle {
    readonly uri: string;
    readonly name: string;
    file(path: string): FileHandle;
    directory(path: string): DirectoryHandle;
    exists(): Promise<boolean>;
    create(): Promise<void>;
    list(): Promise<(FileHandle | DirectoryHandle)[]>;
    delete(): Promise<void>;
  }

  export interface FileHandle {
    readonly uri: string;
    readonly name: string;
    stat(): Promise<FileStat | null>;
    text(): Promise<string>;
    bytes(): Promise<Uint8Array>;
    stream(): ReadableStream<Uint8Array>;
    write(
      data:
        | string
        | ArrayBuffer
        | ArrayBufferView
        | Blob
        | ReadableStream<ArrayBuffer | ArrayBufferView>,
    ): Promise<void>;
    copy(to: FileHandle): Promise<void>;
    move(to: FileHandle): Promise<void>;
    delete(): Promise<void>;
  }
}

declare module "@swipt/api/accessibility" {
  import type { CallOptions, Point, Rect } from "@swipt/api";
  import type { RasterImage } from "@swipt/api/image";

  export type AccessibilityTrait =
    | "button"
    | "link"
    | "image"
    | "playsSound"
    | "keyboardKey"
    | "staticText"
    | "summaryElement"
    | "updatesFrequently"
    | "searchField"
    | "startsMediaSession"
    | "adjustable"
    | "allowsDirectInteraction"
    | "causesPageTurn"
    | "tabBar"
    | "header"
    | "webContent"
    | "textEntry";

  export type AccessibilityAction =
    | "press"
    | "increment"
    | "decrement"
    | "escape"
    | "menu"
    | "scrollUp"
    | "scrollDown"
    | "scrollLeft"
    | "scrollRight"
    | "scrollToVisible";

  export interface AccessibilityElement {
    readonly role: string;
    readonly class?: string;
    readonly traits: readonly AccessibilityTrait[];
    readonly label?: string;
    readonly value?: string;
    readonly identifier?: string;
    readonly placeholder?: string;
    readonly hint?: string;
    readonly url?: string;
    readonly frame?: Rect;
    readonly point?: Point;
    readonly partial?: true;
    readonly offScreen?: true;
    readonly inert?: true;
    readonly remote?: true;
    readonly disabled?: true;
    readonly focused?: true;
    readonly selected?: true;
    readonly expanded?: boolean;
    readonly secure?: true;
    readonly selection?: [location: number, length: number];
    readonly actions?: readonly string[];
    readonly children: readonly AccessibilityElement[];
    readonly parent?: AccessibilityElement;
    find(selector: AccessibilitySelector): AccessibilityElement | null;
    findAll(selector: AccessibilitySelector): AccessibilityElement[];
  }

  export interface AccessibilitySnapshot {
    readonly app: string;
    readonly time: number;
    readonly root: AccessibilityElement;
    readonly system?: AccessibilityElement;
    find(selector: AccessibilitySelector): AccessibilityElement | null;
    findAll(selector: AccessibilitySelector): AccessibilityElement[];
  }

  type TextField =
    | "role"
    | "class"
    | "label"
    | "value"
    | "identifier"
    | "placeholder"
    | "hint"
    | "url"
    | "app";
  type Flag =
    | "disabled"
    | "focused"
    | "selected"
    | "expanded"
    | "secure"
    | "partial"
    | "offScreen"
    | "inert"
    | "remote";

  export type AccessibilitySelector =
    | ({
        traits?: AccessibilityTrait[];
        within?: AccessibilitySelector | AccessibilityElement;
        parent?: AccessibilitySelector | AccessibilityElement;
        has?: AccessibilitySelector | AccessibilityElement;
        not?: AccessibilitySelector | AccessibilityElement;
        index?: number;
      } & { [field in TextField]?: string | RegExp } & {
        [field in Flag]?: boolean;
      })
    | ((element: AccessibilityElement) => boolean);

  export interface SnapshotOptions extends CallOptions {
    readonly scope?: "frontApp" | "screen";
    readonly full?: boolean;
  }

  export interface FindOptions extends CallOptions {
    /** Omitted or 0 reads once; a positive number or Infinity reads until a match, and answers null at the limit. */
    readonly timeoutMs?: number;
    /** Reads again as soon as a read ends. Without it, a read waits for the device's next screen push, which comes 0.75 s after a change at the soonest and not at all for a change that adds or removes no element. */
    readonly poll?: boolean;
    readonly pollTimeoutMs?: number;
    /** When the reads match nothing, scrolls the front app's largest scroll view a page at a time, down to its end and then up to its top, reading the screen after each page; throws NotFoundError naming the scroll view and the pages it went through, rather than answering null. */
    readonly scroll?: boolean;
  }

  export function snapshot(
    options?: SnapshotOptions,
  ): Promise<AccessibilitySnapshot>;
  /** Of several matches, answers the one an action most likely means, preferring in turn one that shows on the screen, one nothing covers, an enabled one, a control, and one at least twice the size of the others; among matches alike, the first in the tree. */
  export function find(
    selector: AccessibilitySelector | readonly AccessibilitySelector[],
    options?: FindOptions,
  ): Promise<AccessibilityElement | null>;
  export interface ElementOptions extends CallOptions {
    /** With a selector, also the time to wait for a match. */
    readonly timeoutMs?: number;
  }

  /** What an action settled on, with `observe`. */
  export interface Observation {
    /** False when the screen kept changing for 3 s after the action, and the frame or snapshot is of it moving. */
    readonly settled?: boolean;
    /** With `observe: "snapshot"`: the front app, read once the screen stopped changing for 300 ms. */
    readonly snapshot?: AccessibilitySnapshot;
    /** With `observe: "frame"`: the frame the screen settled on. */
    readonly frame?: RasterImage;
  }

  export interface ObserveOptions {
    /** After the action, waits for the screen to change and then to stay still for 300 ms (at most 3 s), and answers its snapshot or frame in the result, so one call acts and reads. */
    readonly observe?: "snapshot" | "frame";
  }

  /** "target": the screen changed at the element (or within 44 pt of a tapped point) after the touch; "elsewhere": it changed only away from it, or the device reported the element went, moved or another app came to the front; "none": nothing changed within 800 ms. A correct tap can change nothing visible, as on a control already in that state, so "none" is advice, not an error. */
  export type TapEffect = "target" | "elsewhere" | "none";

  export interface TapAttempt {
    /** "tap", "saferTap" (at the element's visible edge farthest from the app's bars), "scroll" (bringing it 80 pt clear of the bars) or "press" (its accessibility action). */
    readonly method: "tap" | "saferTap" | "scroll" | "press";
    readonly point?: Point;
    readonly effect?: TapEffect;
    readonly detail?: string;
  }

  export interface TapResult extends Observation {
    /** The element tapped, as "the element \"General\"". */
    readonly element?: string;
    /** Where the first touch went. */
    readonly point?: Point;
    /** What the tap changed; missing when it was not watched. */
    readonly effect?: TapEffect;
    /** How the effect showed, such as "the screen changed at it 214 ms after the touch". */
    readonly detail?: string;
    /** Every try, when the tap scrolled the element into view or tried again. */
    readonly attempts?: readonly TapAttempt[];
  }

  export interface TapElementOptions extends ElementOptions, ObserveOptions {
    /** Watches the screen and the element for up to 800 ms after the touch and reports the effect; on by default. False taps once and reports no effect, as before. */
    readonly verify?: boolean;
    /**
     * With verify, when nothing changed and the tap's point was in doubt, tries again in turn: at a safer point of the element, after scrolling it clear of the app's bars, and with its press action, stopping at the first that changes something; on by default.
     * The point is in doubt when the element was partial or off the screen, lay within 80 pt of the app's bars or by a cover, moved or animated before the tap, or the hit test answered another element.
     * A whole, clear and steady element is never tried again: its tap may rightly change nothing, or show it after 800 ms, and a second tap would act twice. Nor is an element already selected, or one whose label or identifier names an irreversible or outward action (Send, Pay, Buy, Delete, Remove, Reset, Confirm, Allow, Call, Post, Share, Sign Out and the like, in English).
     */
    readonly recover?: boolean;
    /** With a selector that matches nothing on the screen, searches the pages of the screen as find's `scroll` does. */
    readonly scroll?: boolean;
  }

  /** A selector picks its match as find does, and throws when several rank alike, listing them. An element off the screen or under the app's own bars, in a scroll view, is first scrolled 80 pt clear of the bars. */
  export function tap(
    element: AccessibilityElement | AccessibilitySelector,
    options?: TapElementOptions,
  ): Promise<TapResult>;
  export function setText(
    element: AccessibilityElement | AccessibilitySelector,
    text: string,
    options?: ElementOptions,
  ): Promise<void>;
  export function perform(
    element: AccessibilityElement | AccessibilitySelector,
    action: AccessibilityAction | (string & {}),
    options?: CallOptions,
  ): Promise<void>;
}

declare module "@swipt/api/input" {
  import type { CallOptions, Point } from "@swipt/api";
  import type { Observation, ObserveOptions, TapResult } from "@swipt/api/accessibility";

  export interface TapOptions extends CallOptions, ObserveOptions {
    readonly count?: number;
    /** Watches the screen for up to 800 ms after the touch and reports in `effect` whether it changed within 44 pt of the point or elsewhere; `observe` implies it. It never taps again. */
    readonly verify?: boolean;
  }

  export type KeyboardKey =
    | "return"
    | "backspace"
    | "forwardDelete"
    | "tab"
    | "escape"
    | "space"
    | "left"
    | "right"
    | "up"
    | "down"
    | "home"
    | "end"
    | "pageUp"
    | "pageDown"
    | `f${1 | 2 | 3 | 4 | 5 | 6 | 7 | 8 | 9 | 10 | 11 | 12}`;
  export type KeyboardModifier = "cmd" | "shift" | "ctrl" | "alt";
  export type KeyCombination =
    KeyboardKey | `${KeyboardModifier}+${KeyboardKey}` | (string & {});
  export type TouchSample = [x: number, y: number, ms: number];

  /** Answers the point, and with `verify` or `observe` the effect and its detail. */
  export function tap(
    point: Point,
    options?: TapOptions,
  ): Promise<TapResult>;
  /** Answers an empty object, or with `observe` what the scroll settled on. */
  export function swipe(
    from: Point,
    to: Point,
    options?: CallOptions & ObserveOptions & { durationMs?: number },
  ): Promise<Observation>;
  export function drag(
    from: Point,
    to: Point,
    options?: CallOptions &
      ObserveOptions & { durationMs?: number; pressMs?: number },
  ): Promise<Observation>;
  export function pinch(
    center: Point,
    scale: number,
    options?: CallOptions & { durationMs?: number },
  ): Promise<void>;
  export function touch(
    fingers: readonly (readonly TouchSample[])[],
    options?: CallOptions,
  ): Promise<void>;
  export function pressKey(
    combination: KeyCombination,
    options?: CallOptions,
  ): Promise<void>;
  export function pressHome(options?: CallOptions): Promise<void>;
  export function openAppSwitcher(options?: CallOptions): Promise<void>;
  export function pressPower(options?: CallOptions): Promise<void>;
  export function pressVolumeUp(options?: CallOptions): Promise<void>;
  export function pressVolumeDown(options?: CallOptions): Promise<void>;
}

declare module "@swipt/api/device" {
  import type { CallOptions, ObservableState } from "@swipt/api";
  import type { FileHandle } from "@swipt/api/file-system";

  export type DisplayState = "off" | "dimmed" | "inactiveOn" | "activeOn";

  export interface DeviceInfo {
    readonly name: string;
    readonly model: string;
    readonly os: string;
    readonly build: string;
    readonly language: string;
    readonly screen: {
      readonly width: number;
      readonly height: number;
      readonly scale: number;
      readonly orientation: "portrait" | "portraitUpsideDown" | "landscape";
    };
  }

  export interface InstalledApplication {
    readonly bundleId: string;
    readonly name: string;
    readonly version?: string;
    readonly build?: string;
  }

  export interface ApplicationStateEvent {
    readonly type: "launch" | "foreground" | "background";
    readonly bundleId: string;
    readonly pid: number;
    readonly time: number;
  }

  export interface SimulatedLocation {
    readonly latitude: number;
    readonly longitude: number;
  }

  export function info(options?: CallOptions): Promise<DeviceInfo>;
  export const foregroundApplication: ObservableState<string>;
  export const locked: ObservableState<boolean>;
  export const displayState: ObservableState<DisplayState>;
  export function unlock(options?: CallOptions): Promise<void>;

  export function applicationEvents(
    options?: CallOptions,
  ): AsyncIterable<ApplicationStateEvent>;
  export function installedApplications(
    options?: CallOptions,
  ): Promise<InstalledApplication[]>;
  /** Answers a PNG file in cacheDirectory. */
  export function applicationIcon(
    bundleId: string,
    options?: CallOptions,
  ): Promise<FileHandle>;
  export function launchApplication(
    bundleId: string,
    options?: CallOptions & { url?: string; restart?: boolean },
  ): Promise<void>;

  export function pasteboard(
    options?: CallOptions,
  ): Promise<string | null>;
  export function setPasteboard(
    text: string | null,
    options?: CallOptions,
  ): Promise<void>;

  /**
   * The WGS-84 location Swipt simulates, kept across restarts of Swipt until it is
   * set to null or the swiplet that set it is removed. iOS reports the real location
   * while Swipt is not connected to the device, and has no call that reads the
   * simulated location back, so get answers what Swipt last set.
   */
  export const simulatedLocation: ObservableState<SimulatedLocation | null> & {
    set(location: SimulatedLocation | null, options?: CallOptions): Promise<void>;
  };
}

declare module "@swipt/api/screen" {
  import type { CallOptions, ObservableState, Point, Rect } from "@swipt/api";
  import type { AccessibilityElement } from "@swipt/api/accessibility";
  import type { FileHandle } from "@swipt/api/file-system";
  import type { ImageDifference, RasterImage } from "@swipt/api/image";

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
    /** Where a tap reaches the target: an element's point, the text's center, or on the Home Screen the icon above its label. */
    readonly point: Point;
    /** From 0 to 1: how closely the text matches, less for text a hit test did not confirm and for the light-text pass. */
    readonly score: number;
    readonly source: LocateSource;
    /** The element came from accessibility, or a hit test at the point reached an element with this text. */
    readonly confirmed: boolean;
    readonly element?: AccessibilityElement;
  }

  /**
   * A lossless PNG of the screen from the device. Where the engine streams the screen (not on
   * macOS yet), a PNG taken earlier answers at once while the stream shows the screen unchanged
   * since it began, down to the last pixel; one is taken in the background once the screen has
   * stood still for half a second.
   */
  export function screenshot(options?: CallOptions): Promise<RasterImage>;
  export function find(
    target: ScreenTarget | readonly ScreenTarget[],
    options?: ScreenFindOptions,
  ): Promise<ScreenMatch | null>;
  /**
   * Finds text where a tap reaches it: an element of the front app whose label or value is the text (case, spaces and punctuation aside), else the text read off the live frame, matched loosely, read again with only near-white pixels kept when the first reading misses it.
   * Text read off the frame is confirmed with a hit test; on the Home Screen a label leads to its icon. Answers null when nothing scores the threshold. Tap the answer with input.tap(located.point, { verify: true }).
   */
  export function locate(
    target: string | RegExp,
    options?: LocateOptions,
  ): Promise<Located | null>;
  export const frames: ObservableState<RasterImage>;
  export function waitFor(
    predicate: (frame: RasterImage) => boolean | Promise<boolean>,
    options?: CallOptions & { readonly timeoutMs?: number },
  ): Promise<RasterImage>;
  /**
   * The first frame that differs from `from`, or else from the latest frame, in at least
   * 1/5000 of the region's pixels by `threshold` gray levels (24), compared at a quarter of their
   * size. It answers as soon as the changed frame is decoded.
   */
  export function waitForChange(
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
  export function waitForStable(
    options?: WaitOptions & { readonly durationMs?: number },
  ): Promise<RasterImage>;
  export function record(
    options?: CallOptions & {
      /** Omitted, the recording goes to a new file in cacheDirectory. */
      readonly file?: FileHandle | string;
    },
  ): Promise<RecordingSession>;
  export function openRecording(
    file: FileHandle | string,
    options?: CallOptions,
  ): Promise<ScreenRecording>;
}

declare module "@swipt/api/image" {
  import type { CallOptions, Point, Quad, Rect } from "@swipt/api";
  import type { FileHandle } from "@swipt/api/file-system";

  export type Color =
    `#${string}` | readonly [red: number, green: number, blue: number];
  export type ImageFormat = "png" | "jpeg" | "heic";

  export interface TemplateMatch {
    readonly frame: Rect;
    readonly center: Point;
    readonly score: number;
    readonly scale: number;
  }

  export interface TemplateMatchOptions extends CallOptions {
    readonly threshold?: number;
    readonly region?: Rect;
    readonly scales?: readonly number[];
  }

  export interface ColorCheck {
    readonly point: Point;
    readonly color: Color;
    readonly tolerance?: number;
  }

  export interface ConnectedComponent {
    readonly frame: Rect;
    readonly pixels: number;
    readonly centroid: Point;
  }

  export interface ImageStats {
    readonly mean: readonly number[];
    readonly min: readonly number[];
    readonly max: readonly number[];
    readonly deviation: readonly number[];
  }

  export interface TextRecognitionOptions extends CallOptions {
    readonly region?: Rect;
    readonly languages?: readonly string[];
    readonly detectLanguage?: boolean;
    readonly level?: "fast" | "accurate";
    readonly words?: readonly string[];
    readonly correction?: boolean;
    readonly minimumHeight?: number;
  }

  export interface RecognizedTextBox {
    readonly quad: Quad;
    readonly frame: Rect;
    readonly center: Point;
  }

  export interface RecognizedTextCandidate {
    readonly text: string;
    readonly confidence: number;
    boundingBox(start: number, end: number): RecognizedTextBox | null;
  }

  export interface RecognizedTextLine
    extends RecognizedTextCandidate, RecognizedTextBox {
    readonly candidates: readonly RecognizedTextCandidate[];
  }

  export interface ImageDifference {
    readonly ratio: number;
    readonly frames: readonly Rect[];
  }

  export interface DetectedBarcode {
    readonly payload: string | null;
    readonly symbology: string;
    readonly quad: Quad;
    readonly frame: Rect;
    readonly center: Point;
  }

  export interface RasterImage {
    readonly width: number;
    readonly height: number;
    readonly scale: number;
    readonly frame: Rect;
    readonly time: number | null;
    readonly timestamp: number | null;

    crop(rect: Rect): RasterImage;
    resize(scale: number): RasterImage;
    resize(size: {
      readonly width?: number;
      readonly height?: number;
    }): RasterImage;
    rotate(degrees: number): RasterImage;
    grayscale(): RasterImage;
    threshold(level?: number | "otsu"): RasterImage;
    mask(color: Color, options?: { readonly tolerance?: number }): RasterImage;
    mask(range: readonly [low: Color, high: Color]): RasterImage;
    invert(): RasterImage;
    dilate(radius?: number): RasterImage;
    erode(radius?: number): RasterImage;

    encode(
      format?: ImageFormat,
      options?: CallOptions & { readonly quality?: number },
    ): Promise<Uint8Array>;
    save(
      file: FileHandle | string,
      options?: CallOptions & {
        readonly format?: ImageFormat;
        readonly quality?: number;
      },
    ): Promise<FileHandle>;
    pixel(
      point: Point,
      options?: CallOptions,
    ): Promise<[red: number, green: number, blue: number, alpha: number]>;
    stats(
      options?: CallOptions & { readonly region?: Rect },
    ): Promise<ImageStats>;

    find(
      template: RasterImage,
      options?: TemplateMatchOptions,
    ): Promise<TemplateMatch | null>;
    findAll(
      template: RasterImage,
      options?: TemplateMatchOptions & {
        readonly limit?: number;
        readonly overlap?: number;
      },
    ): Promise<TemplateMatch[]>;
    matchColors(
      checks: readonly ColorCheck[],
      options?: CallOptions,
    ): Promise<boolean[]>;
    components(
      options?: CallOptions & { readonly minimumPixels?: number },
    ): Promise<ConnectedComponent[]>;
    recognizeText(
      options?: TextRecognitionOptions,
    ): Promise<RecognizedTextLine[]>;
    diff(
      other: RasterImage,
      options?: CallOptions & { readonly threshold?: number },
    ): Promise<ImageDifference>;
    distance(other: RasterImage, options?: CallOptions): Promise<number>;
    nearest(
      candidates: readonly RasterImage[],
      options?: CallOptions,
    ): Promise<{ readonly index: number; readonly distance: number }>;
    detectBarcodes(options?: CallOptions): Promise<DetectedBarcode[]>;
  }

  export function decode(
    data: Uint8Array | FileHandle | string,
    options?: CallOptions & { readonly scale?: number },
  ): Promise<RasterImage>;
}

declare module "@swipt/api/app-intents" {
  /**
   * This package is designed to provide the same headless interactions as Shortcuts or App Intents.
   *
   * When Shortcuts runs an action, iOS presents these interactions itself, except requestValue.
   * In every other case Swipt presents them. If another app is in front, Swipt takes a screenshot
   * of it, comes to the front with the interaction over that screenshot, and returns to that app
   * when the interaction ends. Cancelling an interaction rejects it with AbortError.
   *
   * It works in every environment, including the REPL, but should not be used in place of native
   * UI interactions in a swiplet.
   */
  import type { CallOptions } from "@swipt/api";

  export type IntentChoiceStyle = "default" | "destructive" | "cancel";

  export type ConfirmationActionName =
    | "add"
    | "addData"
    | "book"
    | "buy"
    | "call"
    | "checkIn"
    | "continue"
    | "create"
    | "do"
    | "download"
    | "filter"
    | "find"
    | "get"
    | "go"
    | "log"
    | "open"
    | "order"
    | "pay"
    | "play"
    | "playSound"
    | "post"
    | "request"
    | "run"
    | "search"
    | "send"
    | "set"
    | "share"
    | "start"
    | "startNavigation"
    | "toggle"
    | "turnOff"
    | "turnOn"
    | "view";

  export interface Person {
    readonly name: string;
    readonly handle?: { type: "phoneNumber" | "emailAddress"; value: string };
  }

  export interface Placemark {
    readonly name?: string;
    readonly coordinate?: [latitude: number, longitude: number];
    readonly address?: string;
  }

  /**
   * A choice of style cancel rejects with AbortError. Swiping the request away answers
   * the first choice, so the first choice is the one that is safe to take unasked.
   */
  export function requestChoice<
    T extends string | { title: string; style?: IntentChoiceStyle },
  >(choices: T[], prompt: string, options?: CallOptions): Promise<T>;
  /** The deny button rejects with AbortError. */
  export function requestConfirmation(
    prompt: string,
    options?: CallOptions & {
      actionName?:
        | ConfirmationActionName
        | { accept: string; deny: string; destructive?: boolean };
    },
  ): Promise<void>;
  export function requestValue(
    type: "text",
    prompt?: string,
    options?: CallOptions & {
      multiline?: boolean;
      /** Turns off autocapitalization, autocorrection, spell checking and smart punctuation. */
      literal?: boolean;
    },
  ): Promise<string>;
  export function requestValue(
    type: "integer",
    prompt?: string,
    options?: CallOptions & { stepper?: boolean; min?: number; max?: number },
  ): Promise<number>;
  export function requestValue(
    type: "number",
    prompt?: string,
    options?: CallOptions & { min?: number; max?: number },
  ): Promise<number>;
  export function requestValue(
    type: "date" | "time" | "dateTime",
    prompt?: string,
    options?: CallOptions,
  ): Promise<Date>;
  export function requestValue(
    type: "file",
    prompt?: string,
    options?: CallOptions,
  ): Promise<{ name: string; type: string; bytes: Uint8Array }>;
  export function requestValue(
    type: "person",
    prompt?: string,
    options?: CallOptions,
  ): Promise<Person>;
  export function requestValue(
    type: "placemark",
    prompt?: string,
    options?: CallOptions,
  ): Promise<Placemark>;
  export function requestDisambiguation(
    values: string[],
    prompt: string,
    options?: CallOptions,
  ): Promise<string>;
  export function requestValueConfirmation(
    value: string,
    prompt: string,
    options?: CallOptions,
  ): Promise<boolean>;
}

declare module "@swipt/api/localization" {
  export function preferredLocalizations(
    localizations: readonly string[],
  ): string[];
}

declare module "expo-sqlite" {
  export type SQLiteBindValue =
    string | number | boolean | bigint | null | Uint8Array;
  export type SQLiteBindParams =
    Record<string, SQLiteBindValue> | SQLiteBindValue[];
  export type SQLiteVariadicBindParams = SQLiteBindValue[];

  export interface SQLiteOpenOptions {
    enableChangeListener?: boolean;
    useNewConnection?: boolean;
    [key: string]: unknown;
  }

  export interface SQLiteRunResult {
    lastInsertRowId: number;
    changes: number;
  }

  export interface SQLiteExecuteSyncResult<T> extends IterableIterator<T> {
    readonly lastInsertRowId: number;
    readonly changes: number;
    getFirstSync(): T | null;
    getAllSync(): T[];
    resetSync(): void;
  }

  export interface SQLiteExecuteAsyncResult<T> extends AsyncIterableIterator<T> {
    readonly lastInsertRowId: number;
    readonly changes: number;
    getFirstAsync(): Promise<T | null>;
    getAllAsync(): Promise<T[]>;
    resetAsync(): Promise<void>;
  }

  export interface SQLiteStatement {
    executeSync<T>(params: SQLiteBindParams): SQLiteExecuteSyncResult<T>;
    executeSync<T>(
      ...params: SQLiteVariadicBindParams
    ): SQLiteExecuteSyncResult<T>;
    executeForRawResultSync<T extends unknown[]>(
      params: SQLiteBindParams,
    ): SQLiteExecuteSyncResult<T>;
    executeForRawResultSync<T extends unknown[]>(
      ...params: SQLiteVariadicBindParams
    ): SQLiteExecuteSyncResult<T>;
    executeAsync<T>(
      params: SQLiteBindParams,
    ): Promise<SQLiteExecuteAsyncResult<T>>;
    executeAsync<T>(
      ...params: SQLiteVariadicBindParams
    ): Promise<SQLiteExecuteAsyncResult<T>>;
    executeForRawResultAsync<T extends unknown[]>(
      params: SQLiteBindParams,
    ): Promise<SQLiteExecuteAsyncResult<T>>;
    executeForRawResultAsync<T extends unknown[]>(
      ...params: SQLiteVariadicBindParams
    ): Promise<SQLiteExecuteAsyncResult<T>>;
    getColumnNamesSync(): string[];
    getColumnNamesAsync(): Promise<string[]>;
    finalizeSync(): void;
    finalizeAsync(): Promise<void>;
  }

  export const SQLiteStatement: abstract new (
    ...args: never
  ) => SQLiteStatement;

  export interface SQLiteTaggedQuery<T = unknown> extends PromiseLike<
    T[] | SQLiteRunResult
  > {
    values(): Promise<unknown[][]>;
    first(): Promise<T | null>;
    each(): AsyncIterableIterator<T>;
    allSync(): T[] | SQLiteRunResult;
    valuesSync(): unknown[][];
    firstSync(): T | null;
    eachSync(): IterableIterator<T>;
  }

  export const SQLiteTaggedQuery: abstract new (
    ...args: never
  ) => SQLiteTaggedQuery;

  export interface SQLiteDatabase {
    readonly databasePath: string;
    readonly options: SQLiteOpenOptions;
    readonly databaseName: string;
    sql<T = unknown>(
      strings: TemplateStringsArray,
      ...values: SQLiteBindValue[]
    ): SQLiteTaggedQuery<T>;
    isInTransactionSync(): boolean;
    isInTransactionAsync(): Promise<boolean>;
    interruptSync(): void;
    closeSync(): void;
    closeAsync(): Promise<void>;
    execSync(source: string): void;
    execAsync(source: string): Promise<void>;
    prepareSync(source: string): SQLiteStatement;
    prepareAsync(source: string): Promise<SQLiteStatement>;
    runSync(source: string, params: SQLiteBindParams): SQLiteRunResult;
    runSync(source: string, ...params: SQLiteVariadicBindParams): SQLiteRunResult;
    runAsync(source: string, params: SQLiteBindParams): Promise<SQLiteRunResult>;
    runAsync(
      source: string,
      ...params: SQLiteVariadicBindParams
    ): Promise<SQLiteRunResult>;
    getFirstSync<T>(source: string, params: SQLiteBindParams): T | null;
    getFirstSync<T>(
      source: string,
      ...params: SQLiteVariadicBindParams
    ): T | null;
    getFirstAsync<T>(source: string, params: SQLiteBindParams): Promise<T | null>;
    getFirstAsync<T>(
      source: string,
      ...params: SQLiteVariadicBindParams
    ): Promise<T | null>;
    getAllSync<T>(source: string, params: SQLiteBindParams): T[];
    getAllSync<T>(source: string, ...params: SQLiteVariadicBindParams): T[];
    getAllAsync<T>(source: string, params: SQLiteBindParams): Promise<T[]>;
    getAllAsync<T>(
      source: string,
      ...params: SQLiteVariadicBindParams
    ): Promise<T[]>;
    getEachSync<T>(source: string, params: SQLiteBindParams): IterableIterator<T>;
    getEachSync<T>(
      source: string,
      ...params: SQLiteVariadicBindParams
    ): IterableIterator<T>;
    getEachAsync<T>(
      source: string,
      params: SQLiteBindParams,
    ): AsyncIterableIterator<T>;
    getEachAsync<T>(
      source: string,
      ...params: SQLiteVariadicBindParams
    ): AsyncIterableIterator<T>;
    withTransactionSync(task: () => void): void;
    withTransactionAsync(task: () => Promise<void>): Promise<void>;
    withExclusiveTransactionAsync(
      task: (transaction: SQLiteDatabase) => Promise<void>,
    ): Promise<void>;
    serializeSync(databaseName?: string): Uint8Array;
    serializeAsync(databaseName?: string): Promise<Uint8Array>;
    loadExtensionSync(libPath: string, entryPoint?: string): never;
    loadExtensionAsync(libPath: string, entryPoint?: string): Promise<never>;
    createSessionSync(databaseName?: string): never;
    createSessionAsync(databaseName?: string): Promise<never>;
    syncLibSQL(): Promise<never>;
  }

  export const SQLiteDatabase: abstract new (
    ...args: never
  ) => SQLiteDatabase;

  export interface SQLiteSession {}

  export const SQLiteSession: abstract new (
    ...args: never
  ) => SQLiteSession;

  export interface DatabaseChangeEvent {
    databaseName: string;
    databaseFilePath: string;
    tableName: string;
    rowId: number;
  }

  export interface SQLiteBackupOptions {
    sourceDatabase: SQLiteDatabase;
    sourceDatabaseName?: string;
    destDatabase: SQLiteDatabase;
    destDatabaseName?: string;
  }

  export interface SQLiteProviderAssetSource {
    assetId: number;
    forceOverwrite?: boolean;
  }

  export const defaultDatabaseDirectory: string;
  export const bundledExtensions: Readonly<
    Record<string, { libPath: string; entryPoint: string } | undefined>
  >;
  export function openDatabaseSync(
    databaseName: string,
    options?: SQLiteOpenOptions,
    directory?: string,
  ): SQLiteDatabase;
  export function openDatabaseAsync(
    databaseName: string,
    options?: SQLiteOpenOptions,
    directory?: string,
  ): Promise<SQLiteDatabase>;
  export function deserializeDatabaseSync(
    serializedData: Uint8Array,
    options?: SQLiteOpenOptions,
  ): SQLiteDatabase;
  export function deserializeDatabaseAsync(
    serializedData: Uint8Array,
    options?: SQLiteOpenOptions,
  ): Promise<SQLiteDatabase>;
  export function deleteDatabaseSync(
    databaseName: string,
    directory?: string,
  ): void;
  export function deleteDatabaseAsync(
    databaseName: string,
    directory?: string,
  ): Promise<void>;
  export function importDatabaseFromAssetAsync(
    databaseName: string,
    assetSource: SQLiteProviderAssetSource,
    directory?: string,
  ): Promise<void>;
  export function backupDatabaseSync(options: SQLiteBackupOptions): void;
  export function backupDatabaseAsync(
    options: SQLiteBackupOptions,
  ): Promise<void>;
  export function addDatabaseChangeListener(
    listener: (event: DatabaseChangeEvent) => void,
  ): { remove(): void };
  export function deepEqual(a: unknown, b: unknown): boolean;
  export function configureWeb(options?: unknown): void;
  export function SQLiteProvider(props: unknown): never;
  export function useSQLiteContext(): never;
}

declare module "expo-sqlite/kv-store" {
  export type SQLiteStorageSetItemUpdateFunction = (
    previous: string | null,
  ) => string;

  export class SQLiteStorage {
    constructor(databaseName: string);
    getItemSync(key: string): string | null;
    getItemAsync(key: string): Promise<string | null>;
    getItem(key: string): Promise<string | null>;
    setItemSync(
      key: string,
      value: string | SQLiteStorageSetItemUpdateFunction,
    ): void;
    setItemAsync(
      key: string,
      value: string | SQLiteStorageSetItemUpdateFunction,
    ): Promise<void>;
    setItem(
      key: string,
      value: string | SQLiteStorageSetItemUpdateFunction,
    ): Promise<void>;
    removeItemSync(key: string): boolean;
    removeItemAsync(key: string): Promise<boolean>;
    removeItem(key: string): Promise<void>;
    getAllKeysSync(): string[];
    getAllKeysAsync(): Promise<string[]>;
    getAllKeys(): Promise<string[]>;
    clearSync(): boolean;
    clearAsync(): Promise<boolean>;
    clear(): Promise<void>;
    getLengthSync(): number;
    getLengthAsync(): Promise<number>;
    getKeyByIndexSync(index: number): string | null;
    getKeyByIndexAsync(index: number): Promise<string | null>;
    mergeItem(key: string, value: string): Promise<void>;
    multiGet(keys: string[]): Promise<[string, string | null][]>;
    multiSet(pairs: [string, string][]): Promise<void>;
    multiRemove(keys: string[]): Promise<void>;
    multiMerge(pairs: [string, string][]): Promise<void>;
    closeSync(): void;
    closeAsync(): Promise<void>;
  }

  export const AsyncStorage: SQLiteStorage;
  export const Storage: SQLiteStorage;
  export default Storage;
}

declare module "expo-sqlite/localStorage/install" {
  global {
    interface Storage {
      readonly length: number;
      clear(): void;
      getItem(key: string): string | null;
      key(index: number): string | null;
      removeItem(key: string): void;
      setItem(key: string, value: string): void;
    }

    var localStorage: Storage;
  }

  export {};
}

declare namespace Web {
  type Fallback<T> = typeof globalThis extends { onmessage: any } ? {} : T;
  type FallbackValue<Name extends string, T> = typeof globalThis extends {
    onmessage: any;
  }
    ? typeof globalThis extends Record<Name, infer U>
      ? U
      : never
    : T;
  type WorkerFallback<T> = typeof globalThis extends { importScripts: any }
    ? {}
    : T;
  type WorkerFallbackValue<Name extends string, T> = typeof globalThis extends {
    importScripts: any;
  }
    ? FallbackValue<Name, T>
    : T;

  interface WorkerNavigator {
    readonly userAgent: string;
    readonly language: string;
    readonly languages: ReadonlyArray<string>;
  }
  var WorkerNavigator: abstract new () => WorkerNavigator;
  var navigator: WorkerNavigator;
  interface AbortController {
    readonly signal: AbortSignal;
    abort(reason?: any): void;
  }
  var AbortController: {
    prototype: AbortController;
    new (): AbortController;
  };
  interface AbortSignal extends EventTarget {
    readonly aborted: boolean;
    onabort: ((this: AbortSignal, ev: Event) => any) | null;
    readonly reason: any;
    throwIfAborted(): void;
    addEventListener<K extends keyof AbortSignalEventMap>(
      type: K,
      listener: (this: AbortSignal, ev: AbortSignalEventMap[K]) => any,
      options?: boolean | AddEventListenerOptions,
    ): void;
    addEventListener(
      type: string,
      listener: EventListenerOrEventListenerObject,
      options?: boolean | AddEventListenerOptions,
    ): void;
    removeEventListener<K extends keyof AbortSignalEventMap>(
      type: K,
      listener: (this: AbortSignal, ev: AbortSignalEventMap[K]) => any,
      options?: boolean | EventListenerOptions,
    ): void;
    removeEventListener(
      type: string,
      listener: EventListenerOrEventListenerObject,
      options?: boolean | EventListenerOptions,
    ): void;
  }
  var AbortSignal: {
    prototype: AbortSignal;
    new (): AbortSignal;
    abort(reason?: any): AbortSignal;
    any(signals: AbortSignal[]): AbortSignal;
    timeout(milliseconds: number): AbortSignal;
  };
  interface Event {
    readonly bubbles: boolean;
    cancelBubble: boolean;
    readonly cancelable: boolean;
    readonly composed: boolean;
    readonly currentTarget: EventTarget | null;
    readonly defaultPrevented: boolean;
    readonly eventPhase: number;
    readonly isTrusted: boolean;
    returnValue: boolean;
    readonly srcElement: EventTarget | null;
    readonly target: EventTarget | null;
    readonly timeStamp: DOMHighResTimeStamp;
    readonly type: string;
    composedPath(): EventTarget[];
    initEvent(type: string, bubbles?: boolean, cancelable?: boolean): void;
    preventDefault(): void;
    stopImmediatePropagation(): void;
    stopPropagation(): void;
    readonly NONE: 0;
    readonly CAPTURING_PHASE: 1;
    readonly AT_TARGET: 2;
    readonly BUBBLING_PHASE: 3;
  }
  var Event: {
    prototype: Event;
    new (type: string, eventInitDict?: EventInit): Event;
    readonly NONE: 0;
    readonly CAPTURING_PHASE: 1;
    readonly AT_TARGET: 2;
    readonly BUBBLING_PHASE: 3;
  };
  interface EventTarget {
    addEventListener(
      type: string,
      callback: EventListenerOrEventListenerObject | null,
      options?: AddEventListenerOptions | boolean,
    ): void;
    dispatchEvent(event: Event): boolean;
    removeEventListener(
      type: string,
      callback: EventListenerOrEventListenerObject | null,
      options?: EventListenerOptions | boolean,
    ): void;
  }
  var EventTarget: {
    prototype: EventTarget;
    new (): EventTarget;
  };
  interface CustomEvent<T = any> extends Event {
    readonly detail: T | null;
    initCustomEvent(
      type: string,
      bubbles?: boolean,
      cancelable?: boolean,
      detail?: T,
    ): void;
  }
  var CustomEvent: {
    prototype: CustomEvent;
    new <T>(type: string, eventInitDict?: CustomEventInit<T>): CustomEvent<T>;
  };
  interface ErrorEvent extends Event {
    readonly colno: number;
    readonly error: any;
    readonly filename: string;
    readonly lineno: number;
    readonly message: string;
  }
  var ErrorEvent: {
    prototype: ErrorEvent;
    new (type: string, eventInitDict?: ErrorEventInit): ErrorEvent;
  };
  interface DOMException extends Error {
    readonly code: number;
    readonly message: string;
    readonly name: string;
    readonly INDEX_SIZE_ERR: 1;
    readonly DOMSTRING_SIZE_ERR: 2;
    readonly HIERARCHY_REQUEST_ERR: 3;
    readonly WRONG_DOCUMENT_ERR: 4;
    readonly INVALID_CHARACTER_ERR: 5;
    readonly NO_DATA_ALLOWED_ERR: 6;
    readonly NO_MODIFICATION_ALLOWED_ERR: 7;
    readonly NOT_FOUND_ERR: 8;
    readonly NOT_SUPPORTED_ERR: 9;
    readonly INUSE_ATTRIBUTE_ERR: 10;
    readonly INVALID_STATE_ERR: 11;
    readonly SYNTAX_ERR: 12;
    readonly INVALID_MODIFICATION_ERR: 13;
    readonly NAMESPACE_ERR: 14;
    readonly INVALID_ACCESS_ERR: 15;
    readonly VALIDATION_ERR: 16;
    readonly TYPE_MISMATCH_ERR: 17;
    readonly SECURITY_ERR: 18;
    readonly NETWORK_ERR: 19;
    readonly ABORT_ERR: 20;
    readonly URL_MISMATCH_ERR: 21;
    readonly QUOTA_EXCEEDED_ERR: 22;
    readonly TIMEOUT_ERR: 23;
    readonly INVALID_NODE_TYPE_ERR: 24;
    readonly DATA_CLONE_ERR: 25;
  }
  var DOMException: {
    prototype: DOMException;
    new (message?: string, name?: string): DOMException;
    readonly INDEX_SIZE_ERR: 1;
    readonly DOMSTRING_SIZE_ERR: 2;
    readonly HIERARCHY_REQUEST_ERR: 3;
    readonly WRONG_DOCUMENT_ERR: 4;
    readonly INVALID_CHARACTER_ERR: 5;
    readonly NO_DATA_ALLOWED_ERR: 6;
    readonly NO_MODIFICATION_ALLOWED_ERR: 7;
    readonly NOT_FOUND_ERR: 8;
    readonly NOT_SUPPORTED_ERR: 9;
    readonly INUSE_ATTRIBUTE_ERR: 10;
    readonly INVALID_STATE_ERR: 11;
    readonly SYNTAX_ERR: 12;
    readonly INVALID_MODIFICATION_ERR: 13;
    readonly NAMESPACE_ERR: 14;
    readonly INVALID_ACCESS_ERR: 15;
    readonly VALIDATION_ERR: 16;
    readonly TYPE_MISMATCH_ERR: 17;
    readonly SECURITY_ERR: 18;
    readonly NETWORK_ERR: 19;
    readonly ABORT_ERR: 20;
    readonly URL_MISMATCH_ERR: 21;
    readonly QUOTA_EXCEEDED_ERR: 22;
    readonly TIMEOUT_ERR: 23;
    readonly INVALID_NODE_TYPE_ERR: 24;
    readonly DATA_CLONE_ERR: 25;
  };
  interface QuotaExceededError extends DOMException {
    readonly quota: number | null;
    readonly requested: number | null;
  }
  interface QuotaExceededErrorOptions {
    quota?: number;
    requested?: number;
  }
  var QuotaExceededError: {
    prototype: QuotaExceededError;
    new (
      message?: string,
      options?: QuotaExceededErrorOptions,
    ): QuotaExceededError;
  } & Omit<typeof DOMException, "prototype">;
  interface TextEncoder extends TextEncoderCommon {
    encode(input?: string): Uint8Array;
    encodeInto(
      source: string,
      destination: Uint8Array,
    ): TextEncoderEncodeIntoResult;
  }
  var TextEncoder: {
    prototype: TextEncoder;
    new (): TextEncoder;
  };
  interface TextDecoder extends TextDecoderCommon {
    decode(
      input?: AllowSharedBufferSource,
      options?: TextDecodeOptions,
    ): string;
  }
  var TextDecoder: {
    prototype: TextDecoder;
    new (label?: string, options?: TextDecoderOptions): TextDecoder;
  };
  function atob(data: string): string;
  function btoa(data: string): string;
  function structuredClone<T = any>(
    value: T,
    options?: StructuredSerializeOptions,
  ): T;
  interface URL {
    hash: string;
    host: string;
    hostname: string;
    href: string;
    toString(): string;
    readonly origin: string;
    password: string;
    pathname: string;
    port: string;
    protocol: string;
    search: string;
    readonly searchParams: URLSearchParams;
    username: string;
    toJSON(): string;
  }
  var URL: {
    prototype: URL;
    new (url: string | URL, base?: string | URL): URL;
    canParse(url: string | URL, base?: string | URL): boolean;
    createObjectURL(obj: Blob): string;
    parse(url: string | URL, base?: string | URL): URL | null;
    revokeObjectURL(url: string): void;
  };
  interface URLSearchParams {
    readonly size: number;
    append(name: string, value: string): void;
    delete(name: string, value?: string): void;
    get(name: string): string | null;
    getAll(name: string): string[];
    has(name: string, value?: string): boolean;
    set(name: string, value: string): void;
    sort(): void;
    toString(): string;
    forEach(
      callbackfn: (value: string, key: string, parent: URLSearchParams) => void,
      thisArg?: any,
    ): void;
    [Symbol.iterator](): URLSearchParamsIterator<[string, string]>;
    entries(): URLSearchParamsIterator<[string, string]>;
    keys(): URLSearchParamsIterator<string>;
    values(): URLSearchParamsIterator<string>;
  }
  var URLSearchParams: {
    prototype: URLSearchParams;
    new (
      init?: string[][] | Record<string, string> | string | URLSearchParams,
    ): URLSearchParams;
  };
  interface Crypto {
    readonly subtle: SubtleCrypto;
    getRandomValues<T extends Exclude<BufferSource, ArrayBuffer>>(array: T): T;
    randomUUID(): `${string}-${string}-${string}-${string}-${string}`;
  }
  var Crypto: abstract new () => Crypto;
  interface CryptoKey {
    readonly algorithm: KeyAlgorithm;
    readonly extractable: boolean;
    readonly type: KeyType;
    readonly usages: KeyUsage[];
  }
  var CryptoKey: abstract new () => CryptoKey;
  interface SubtleCrypto {
    decrypt(
      algorithm:
        | AlgorithmIdentifier
        | RsaOaepParams
        | AesCtrParams
        | AesCbcParams
        | AesGcmParams,
      key: CryptoKey,
      data: BufferSource,
    ): Promise<ArrayBuffer>;
    deriveBits(
      algorithm:
        AlgorithmIdentifier | EcdhKeyDeriveParams | HkdfParams | Pbkdf2Params,
      baseKey: CryptoKey,
      length?: number | null,
    ): Promise<ArrayBuffer>;
    deriveKey(
      algorithm:
        AlgorithmIdentifier | EcdhKeyDeriveParams | HkdfParams | Pbkdf2Params,
      baseKey: CryptoKey,
      derivedKeyType:
        | AlgorithmIdentifier
        | AesDerivedKeyParams
        | HmacImportParams
        | HkdfParams
        | Pbkdf2Params,
      extractable: boolean,
      keyUsages: KeyUsage[],
    ): Promise<CryptoKey>;
    digest(
      algorithm: AlgorithmIdentifier,
      data: BufferSource,
    ): Promise<ArrayBuffer>;
    encrypt(
      algorithm:
        | AlgorithmIdentifier
        | RsaOaepParams
        | AesCtrParams
        | AesCbcParams
        | AesGcmParams,
      key: CryptoKey,
      data: BufferSource,
    ): Promise<ArrayBuffer>;
    exportKey(format: "jwk", key: CryptoKey): Promise<JsonWebKey>;
    exportKey(
      format: Exclude<KeyFormat, "jwk">,
      key: CryptoKey,
    ): Promise<ArrayBuffer>;
    exportKey(
      format: KeyFormat,
      key: CryptoKey,
    ): Promise<ArrayBuffer | JsonWebKey>;
    generateKey(
      algorithm: "Ed25519" | { name: "Ed25519" },
      extractable: boolean,
      keyUsages: ReadonlyArray<"sign" | "verify">,
    ): Promise<CryptoKeyPair>;
    generateKey(
      algorithm: "X25519" | { name: "X25519" },
      extractable: boolean,
      keyUsages: ReadonlyArray<"deriveBits" | "deriveKey">,
    ): Promise<CryptoKeyPair>;
    generateKey(
      algorithm: RsaHashedKeyGenParams | EcKeyGenParams,
      extractable: boolean,
      keyUsages: ReadonlyArray<KeyUsage>,
    ): Promise<CryptoKeyPair>;
    generateKey(
      algorithm: AesKeyGenParams | HmacKeyGenParams | Pbkdf2Params,
      extractable: boolean,
      keyUsages: ReadonlyArray<KeyUsage>,
    ): Promise<CryptoKey>;
    generateKey(
      algorithm: AlgorithmIdentifier,
      extractable: boolean,
      keyUsages: KeyUsage[],
    ): Promise<CryptoKeyPair | CryptoKey>;
    importKey(
      format: "jwk",
      keyData: JsonWebKey,
      algorithm:
        | AlgorithmIdentifier
        | RsaHashedImportParams
        | EcKeyImportParams
        | HmacImportParams
        | AesKeyAlgorithm,
      extractable: boolean,
      keyUsages: ReadonlyArray<KeyUsage>,
    ): Promise<CryptoKey>;
    importKey(
      format: Exclude<KeyFormat, "jwk">,
      keyData: BufferSource,
      algorithm:
        | AlgorithmIdentifier
        | RsaHashedImportParams
        | EcKeyImportParams
        | HmacImportParams
        | AesKeyAlgorithm,
      extractable: boolean,
      keyUsages: KeyUsage[],
    ): Promise<CryptoKey>;
    sign(
      algorithm: AlgorithmIdentifier | RsaPssParams | EcdsaParams,
      key: CryptoKey,
      data: BufferSource,
    ): Promise<ArrayBuffer>;
    unwrapKey(
      format: KeyFormat,
      wrappedKey: BufferSource,
      unwrappingKey: CryptoKey,
      unwrapAlgorithm:
        | AlgorithmIdentifier
        | RsaOaepParams
        | AesCtrParams
        | AesCbcParams
        | AesGcmParams,
      unwrappedKeyAlgorithm:
        | AlgorithmIdentifier
        | RsaHashedImportParams
        | EcKeyImportParams
        | HmacImportParams
        | AesKeyAlgorithm,
      extractable: boolean,
      keyUsages: KeyUsage[],
    ): Promise<CryptoKey>;
    verify(
      algorithm: AlgorithmIdentifier | RsaPssParams | EcdsaParams,
      key: CryptoKey,
      signature: BufferSource,
      data: BufferSource,
    ): Promise<boolean>;
    wrapKey(
      format: KeyFormat,
      key: CryptoKey,
      wrappingKey: CryptoKey,
      wrapAlgorithm:
        | AlgorithmIdentifier
        | RsaOaepParams
        | AesCtrParams
        | AesCbcParams
        | AesGcmParams,
    ): Promise<ArrayBuffer>;
    deriveKey(
      algorithm:
        AlgorithmIdentifier | EcdhKeyDeriveParams | HkdfParams | Pbkdf2Params,
      baseKey: CryptoKey,
      derivedKeyType:
        | AlgorithmIdentifier
        | AesDerivedKeyParams
        | HmacImportParams
        | HkdfParams
        | Pbkdf2Params,
      extractable: boolean,
      keyUsages: Iterable<KeyUsage>,
    ): Promise<CryptoKey>;
    generateKey(
      algorithm: AlgorithmIdentifier,
      extractable: boolean,
      keyUsages: Iterable<KeyUsage>,
    ): Promise<CryptoKeyPair | CryptoKey>;
    importKey(
      format: Exclude<KeyFormat, "jwk">,
      keyData: BufferSource,
      algorithm:
        | AlgorithmIdentifier
        | RsaHashedImportParams
        | EcKeyImportParams
        | HmacImportParams
        | AesKeyAlgorithm,
      extractable: boolean,
      keyUsages: Iterable<KeyUsage>,
    ): Promise<CryptoKey>;
    unwrapKey(
      format: KeyFormat,
      wrappedKey: BufferSource,
      unwrappingKey: CryptoKey,
      unwrapAlgorithm:
        | AlgorithmIdentifier
        | RsaOaepParams
        | AesCtrParams
        | AesCbcParams
        | AesGcmParams,
      unwrappedKeyAlgorithm:
        | AlgorithmIdentifier
        | RsaHashedImportParams
        | EcKeyImportParams
        | HmacImportParams
        | AesKeyAlgorithm,
      extractable: boolean,
      keyUsages: Iterable<KeyUsage>,
    ): Promise<CryptoKey>;
  }
  var SubtleCrypto: abstract new () => SubtleCrypto;
  var crypto: Crypto;
  interface Blob {
    readonly size: number;
    readonly type: string;
    arrayBuffer(): Promise<ArrayBuffer>;
    bytes(): Promise<Uint8Array>;
    slice(start?: number, end?: number, contentType?: string): Blob;
    stream(): ReadableStream<Uint8Array>;
    text(): Promise<string>;
    textStream(): ReadableStream<string>;
  }
  var Blob: {
    prototype: Blob;
    new (blobParts?: BlobPart[], options?: BlobPropertyBag): Blob;
  };
  interface File extends Blob {
    readonly lastModified: number;
    readonly name: string;
    readonly webkitRelativePath: string;
  }
  var File: {
    prototype: File;
    new (
      fileBits: BlobPart[],
      fileName: string,
      options?: FilePropertyBag,
    ): File;
  };
  interface FormData {
    append(name: string, value: string | Blob): void;
    append(name: string, value: string): void;
    append(name: string, blobValue: Blob, filename?: string): void;
    delete(name: string): void;
    get(name: string): FormDataEntryValue | null;
    getAll(name: string): FormDataEntryValue[];
    has(name: string): boolean;
    set(name: string, value: string | Blob): void;
    set(name: string, value: string): void;
    set(name: string, blobValue: Blob, filename?: string): void;
    forEach(
      callbackfn: (
        value: FormDataEntryValue,
        key: string,
        parent: FormData,
      ) => void,
      thisArg?: any,
    ): void;
    [Symbol.iterator](): FormDataIterator<[string, FormDataEntryValue]>;
    entries(): FormDataIterator<[string, FormDataEntryValue]>;
    keys(): FormDataIterator<string>;
    values(): FormDataIterator<FormDataEntryValue>;
  }
  var FormData: {
    prototype: FormData;
    new (): FormData;
  };
  interface ReadableStream<R = any> {
    readonly locked: boolean;
    cancel(reason?: any): Promise<void>;
    getReader(options: { mode: "byob" }): ReadableStreamBYOBReader;
    getReader(): ReadableStreamDefaultReader<R>;
    getReader(
      options?: ReadableStreamGetReaderOptions,
    ): ReadableStreamReader<R>;
    pipeThrough<T>(
      transform: ReadableWritablePair<T, R>,
      options?: StreamPipeOptions,
    ): ReadableStream<T>;
    pipeTo(
      destination: WritableStream<R>,
      options?: StreamPipeOptions,
    ): Promise<void>;
    tee(): [ReadableStream<R>, ReadableStream<R>];
    [Symbol.asyncIterator](
      options?: ReadableStreamIteratorOptions,
    ): ReadableStreamAsyncIterator<R>;
    values(
      options?: ReadableStreamIteratorOptions,
    ): ReadableStreamAsyncIterator<R>;
  }
  var ReadableStream: {
    prototype: ReadableStream;
    new (
      underlyingSource: UnderlyingByteSource,
      strategy?: { highWaterMark?: number },
    ): ReadableStream<Uint8Array>;
    new <R = any>(
      underlyingSource: UnderlyingDefaultSource<R>,
      strategy?: QueuingStrategy<R>,
    ): ReadableStream<R>;
    new <R = any>(
      underlyingSource?: UnderlyingSource<R>,
      strategy?: QueuingStrategy<R>,
    ): ReadableStream<R>;
  };
  interface ReadableStreamDefaultReader<
    R = any,
  > extends ReadableStreamGenericReader {
    read(): Promise<ReadableStreamReadResult<R>>;
    releaseLock(): void;
  }
  var ReadableStreamDefaultReader: {
    prototype: ReadableStreamDefaultReader;
    new <R = any>(stream: ReadableStream<R>): ReadableStreamDefaultReader<R>;
  };
  interface ReadableStreamBYOBReader extends ReadableStreamGenericReader {
    read<T extends Exclude<BufferSource, ArrayBuffer>>(
      view: T,
      options?: ReadableStreamBYOBReaderReadOptions,
    ): Promise<ReadableStreamReadResult<T>>;
    releaseLock(): void;
  }
  var ReadableStreamBYOBReader: {
    prototype: ReadableStreamBYOBReader;
    new (stream: ReadableStream<Uint8Array>): ReadableStreamBYOBReader;
  };
  interface ReadableStreamBYOBRequest {
    readonly view: ArrayBufferView | null;
    respond(bytesWritten: number): void;
    respondWithNewView(view: ArrayBufferView): void;
  }
  var ReadableStreamBYOBRequest: abstract new () => ReadableStreamBYOBRequest;
  interface ReadableStreamDefaultController<R = any> {
    readonly desiredSize: number | null;
    close(): void;
    enqueue(chunk: R): void;
    error(e?: any): void;
  }
  var ReadableStreamDefaultController: abstract new () => ReadableStreamDefaultController;
  interface ReadableByteStreamController {
    readonly byobRequest: ReadableStreamBYOBRequest | null;
    readonly desiredSize: number | null;
    close(): void;
    enqueue(chunk: ArrayBufferView): void;
    error(e?: any): void;
  }
  var ReadableByteStreamController: abstract new () => ReadableByteStreamController;
  interface WritableStream<W = any> {
    readonly locked: boolean;
    abort(reason?: any): Promise<void>;
    close(): Promise<void>;
    getWriter(): WritableStreamDefaultWriter<W>;
  }
  var WritableStream: {
    prototype: WritableStream;
    new <W = any>(
      underlyingSink?: UnderlyingSink<W>,
      strategy?: QueuingStrategy<W>,
    ): WritableStream<W>;
  };
  interface WritableStreamDefaultWriter<W = any> {
    readonly closed: Promise<undefined>;
    readonly desiredSize: number | null;
    readonly ready: Promise<void>;
    abort(reason?: any): Promise<void>;
    close(): Promise<void>;
    releaseLock(): void;
    write(chunk?: W): Promise<void>;
  }
  var WritableStreamDefaultWriter: {
    prototype: WritableStreamDefaultWriter;
    new <W = any>(stream: WritableStream<W>): WritableStreamDefaultWriter<W>;
  };
  interface WritableStreamDefaultController {
    readonly signal: AbortSignal;
    error(e?: any): void;
  }
  var WritableStreamDefaultController: abstract new () => WritableStreamDefaultController;
  interface TransformStream<I = any, O = any> {
    readonly readable: ReadableStream<O>;
    readonly writable: WritableStream<I>;
  }
  var TransformStream: {
    prototype: TransformStream;
    new <I = any, O = any>(
      transformer?: Transformer<I, O>,
      writableStrategy?: QueuingStrategy<I>,
      readableStrategy?: QueuingStrategy<O>,
    ): TransformStream<I, O>;
  };
  interface TransformStreamDefaultController<O = any> {
    readonly desiredSize: number | null;
    enqueue(chunk?: O): void;
    error(reason?: any): void;
    terminate(): void;
  }
  var TransformStreamDefaultController: abstract new () => TransformStreamDefaultController;
  interface ByteLengthQueuingStrategy extends QueuingStrategy<ArrayBufferView> {
    readonly highWaterMark: number;
    readonly size: QueuingStrategySize<ArrayBufferView>;
  }
  var ByteLengthQueuingStrategy: {
    prototype: ByteLengthQueuingStrategy;
    new (init: QueuingStrategyInit): ByteLengthQueuingStrategy;
  };
  interface CountQueuingStrategy extends QueuingStrategy {
    readonly highWaterMark: number;
    readonly size: QueuingStrategySize;
  }
  var CountQueuingStrategy: {
    prototype: CountQueuingStrategy;
    new (init: QueuingStrategyInit): CountQueuingStrategy;
  };
  interface TextEncoderStream
    extends GenericTransformStream, TextEncoderCommon {
    readonly readable: ReadableStream<Uint8Array>;
    readonly writable: WritableStream<string>;
  }
  var TextEncoderStream: {
    prototype: TextEncoderStream;
    new (): TextEncoderStream;
  };
  interface TextDecoderStream
    extends GenericTransformStream, TextDecoderCommon {
    readonly readable: ReadableStream<string>;
    readonly writable: WritableStream<BufferSource>;
  }
  var TextDecoderStream: {
    prototype: TextDecoderStream;
    new (label?: string, options?: TextDecoderOptions): TextDecoderStream;
  };
  interface CompressionStream extends GenericTransformStream {
    readonly readable: ReadableStream<Uint8Array>;
    readonly writable: WritableStream<BufferSource>;
  }
  var CompressionStream: {
    prototype: CompressionStream;
    new (format: CompressionFormat): CompressionStream;
  };
  interface DecompressionStream extends GenericTransformStream {
    readonly readable: ReadableStream<Uint8Array>;
    readonly writable: WritableStream<BufferSource>;
  }
  var DecompressionStream: {
    prototype: DecompressionStream;
    new (format: CompressionFormat): DecompressionStream;
  };
  var performance: Performance;
  interface Performance extends EventTarget {
    readonly timeOrigin: DOMHighResTimeStamp;
    clearMarks(markName?: string): void;
    clearMeasures(measureName?: string): void;
    getEntries(): PerformanceEntryList;
    getEntriesByName(name: string, type?: string): PerformanceEntryList;
    getEntriesByType(type: string): PerformanceEntryList;
    mark(
      markName: string,
      markOptions?: PerformanceMarkOptions,
    ): PerformanceMark;
    measure(
      measureName: string,
      startOrMeasureOptions?: string | PerformanceMeasureOptions,
      endMark?: string,
    ): PerformanceMeasure;
    now(): DOMHighResTimeStamp;
    toJSON(): any;
    addEventListener(
      type: string,
      listener: EventListenerOrEventListenerObject,
      options?: boolean | AddEventListenerOptions,
    ): void;
    removeEventListener(
      type: string,
      listener: EventListenerOrEventListenerObject,
      options?: boolean | EventListenerOptions,
    ): void;
  }
  var Performance: abstract new () => Performance;
  interface PerformanceEntry {
    readonly duration: DOMHighResTimeStamp;
    readonly entryType: string;
    readonly name: string;
    readonly startTime: DOMHighResTimeStamp;
    toJSON(): any;
  }
  var PerformanceEntry: abstract new () => PerformanceEntry;
  interface PerformanceMark extends PerformanceEntry {
    readonly detail: any;
  }
  var PerformanceMark: {
    prototype: PerformanceMark;
    new (
      markName: string,
      markOptions?: PerformanceMarkOptions,
    ): PerformanceMark;
  };
  interface PerformanceMeasure extends PerformanceEntry {
    readonly detail: any;
  }
  var PerformanceMeasure: abstract new () => PerformanceMeasure;
  interface PerformanceObserver {
    disconnect(): void;
    observe(options?: PerformanceObserverInit): void;
    takeRecords(): PerformanceEntryList;
  }
  var PerformanceObserver: {
    prototype: PerformanceObserver;
    new (callback: PerformanceObserverCallback): PerformanceObserver;
    readonly supportedEntryTypes: ReadonlyArray<string>;
  };
  interface PerformanceObserverEntryList {
    getEntries(): PerformanceEntryList;
    getEntriesByName(name: string, type?: string): PerformanceEntryList;
    getEntriesByType(type: string): PerformanceEntryList;
  }
  var PerformanceObserverEntryList: abstract new () => PerformanceObserverEntryList;
  function queueMicrotask(callback: VoidFunction): void;
  var console: Console;
  function setTimeout(
    handler: string | ((...args: any[]) => void),
    timeout?: number,
    ...arguments: any[]
  ): number;
  function clearTimeout(id?: number): void;
  function setInterval(
    handler: string | ((...args: any[]) => void),
    timeout?: number,
    ...arguments: any[]
  ): number;
  function clearInterval(id?: number): void;
  interface AbortSignalEventMap {
    abort: Event;
  }
  interface AddEventListenerOptions extends EventListenerOptions {
    once?: boolean;
    passive?: boolean;
    signal?: AbortSignal;
  }
  type EventListenerOrEventListenerObject = EventListener | EventListenerObject;
  interface EventListenerOptions {
    capture?: boolean;
  }
  type DOMHighResTimeStamp = number;
  interface EventInit {
    bubbles?: boolean;
    cancelable?: boolean;
    composed?: boolean;
  }
  interface CustomEventInit<T = any> extends EventInit {
    detail?: T;
  }
  interface ErrorEventInit extends EventInit {
    colno?: number;
    error?: any;
    filename?: string;
    lineno?: number;
    message?: string;
  }
  interface TextEncoderCommon {
    readonly encoding: string;
  }
  interface TextEncoderEncodeIntoResult {
    read: number;
    written: number;
  }
  interface TextDecoderCommon {
    readonly encoding: string;
    readonly fatal: boolean;
    readonly ignoreBOM: boolean;
  }
  type AllowSharedBufferSource = ArrayBufferLike | ArrayBufferView;
  interface TextDecodeOptions {
    stream?: boolean;
  }
  interface TextDecoderOptions {
    fatal?: boolean;
    ignoreBOM?: boolean;
  }
  interface StructuredSerializeOptions {
    transfer?: Iterable<
      ArrayBuffer | ReadableStream | WritableStream | TransformStream
    >;
  }
  interface URLSearchParamsIterator<T> extends IteratorObject<
    T,
    BuiltinIteratorReturn,
    unknown
  > {
    [Symbol.iterator](): URLSearchParamsIterator<T>;
  }
  type BufferSource = ArrayBufferView | ArrayBuffer;
  interface KeyAlgorithm {
    name: string;
  }
  type KeyType = "private" | "public" | "secret";
  type KeyUsage =
    | "decrypt"
    | "deriveBits"
    | "deriveKey"
    | "encrypt"
    | "sign"
    | "unwrapKey"
    | "verify"
    | "wrapKey";
  type AlgorithmIdentifier = Algorithm | string;
  interface RsaOaepParams extends Algorithm {
    label?: BufferSource;
  }
  interface AesCtrParams extends Algorithm {
    counter: BufferSource;
    length: number;
  }
  interface AesCbcParams extends Algorithm {
    iv: BufferSource;
  }
  interface AesGcmParams extends Algorithm {
    additionalData?: BufferSource;
    iv: BufferSource;
    tagLength?: number;
  }
  interface EcdhKeyDeriveParams extends Algorithm {
    public: CryptoKey;
  }
  interface HkdfParams extends Algorithm {
    hash: HashAlgorithmIdentifier;
    info: BufferSource;
    salt: BufferSource;
  }
  interface Pbkdf2Params extends Algorithm {
    hash: HashAlgorithmIdentifier;
    iterations: number;
    salt: BufferSource;
  }
  interface AesDerivedKeyParams extends Algorithm {
    length: number;
  }
  interface HmacImportParams extends Algorithm {
    hash: HashAlgorithmIdentifier;
    length?: number;
  }
  interface JsonWebKey {
    alg?: string;
    crv?: string;
    d?: string;
    dp?: string;
    dq?: string;
    e?: string;
    ext?: boolean;
    k?: string;
    key_ops?: string[];
    kty?: string;
    n?: string;
    oth?: RsaOtherPrimesInfo[];
    p?: string;
    q?: string;
    qi?: string;
    use?: string;
    x?: string;
    y?: string;
  }
  type KeyFormat = "jwk" | "pkcs8" | "raw" | "spki";
  interface CryptoKeyPair {
    privateKey: CryptoKey;
    publicKey: CryptoKey;
  }
  interface RsaHashedKeyGenParams extends RsaKeyGenParams {
    hash: HashAlgorithmIdentifier;
  }
  interface EcKeyGenParams extends Algorithm {
    namedCurve: NamedCurve;
  }
  interface AesKeyGenParams extends Algorithm {
    length: number;
  }
  interface HmacKeyGenParams extends Algorithm {
    hash: HashAlgorithmIdentifier;
    length?: number;
  }
  interface RsaHashedImportParams extends Algorithm {
    hash: HashAlgorithmIdentifier;
  }
  interface EcKeyImportParams extends Algorithm {
    namedCurve: NamedCurve;
  }
  interface AesKeyAlgorithm extends KeyAlgorithm {
    length: number;
  }
  interface RsaPssParams extends Algorithm {
    saltLength: number;
  }
  interface EcdsaParams extends Algorithm {
    hash: HashAlgorithmIdentifier;
  }
  type BlobPart = BufferSource | Blob | string;
  interface BlobPropertyBag {
    endings?: EndingType;
    type?: string;
  }
  interface FilePropertyBag extends BlobPropertyBag {
    lastModified?: number;
  }
  type FormDataEntryValue = File | string;
  interface FormDataIterator<T> extends IteratorObject<
    T,
    BuiltinIteratorReturn,
    unknown
  > {
    [Symbol.iterator](): FormDataIterator<T>;
  }
  interface ReadableStreamGetReaderOptions {
    mode?: ReadableStreamReaderMode;
  }
  type ReadableStreamReader<T> =
    ReadableStreamDefaultReader<T> | ReadableStreamBYOBReader;
  interface ReadableWritablePair<R = any, W = any> {
    readable: ReadableStream<R>;
    writable: WritableStream<W>;
  }
  interface StreamPipeOptions {
    preventAbort?: boolean;
    preventCancel?: boolean;
    preventClose?: boolean;
    signal?: AbortSignal;
  }
  interface UnderlyingByteSource {
    autoAllocateChunkSize?: number;
    cancel?: UnderlyingSourceCancelCallback;
    pull?: (
      controller: ReadableByteStreamController,
    ) => void | PromiseLike<void>;
    start?: (controller: ReadableByteStreamController) => any;
    type: "bytes";
  }
  interface UnderlyingDefaultSource<R = any> {
    cancel?: UnderlyingSourceCancelCallback;
    pull?: (
      controller: ReadableStreamDefaultController<R>,
    ) => void | PromiseLike<void>;
    start?: (controller: ReadableStreamDefaultController<R>) => any;
    type?: undefined;
  }
  interface QueuingStrategy<T = any> {
    highWaterMark?: number;
    size?: QueuingStrategySize<T>;
  }
  interface UnderlyingSource<R = any> {
    autoAllocateChunkSize?: number;
    cancel?: UnderlyingSourceCancelCallback;
    pull?: UnderlyingSourcePullCallback<R>;
    start?: UnderlyingSourceStartCallback<R>;
    type?: ReadableStreamType;
  }
  interface ReadableStreamIteratorOptions {
    preventCancel?: boolean;
  }
  interface ReadableStreamAsyncIterator<T> extends AsyncIteratorObject<
    T,
    BuiltinIteratorReturn,
    unknown
  > {
    [Symbol.asyncIterator](): ReadableStreamAsyncIterator<T>;
  }
  interface ReadableStreamGenericReader {
    readonly closed: Promise<undefined>;
    cancel(reason?: any): Promise<void>;
  }
  type ReadableStreamReadResult<T> =
    ReadableStreamReadValueResult<T> | ReadableStreamReadDoneResult<T>;
  interface ReadableStreamBYOBReaderReadOptions {
    min?: number;
  }
  interface UnderlyingSink<W = any> {
    abort?: UnderlyingSinkAbortCallback;
    close?: UnderlyingSinkCloseCallback;
    start?: UnderlyingSinkStartCallback;
    type?: undefined;
    write?: UnderlyingSinkWriteCallback<W>;
  }
  interface Transformer<I = any, O = any> {
    flush?: TransformerFlushCallback<O>;
    readableType?: undefined;
    start?: TransformerStartCallback<O>;
    transform?: TransformerTransformCallback<I, O>;
    writableType?: undefined;
  }
  interface QueuingStrategySize<T = any> {
    (chunk: T): number;
  }
  interface QueuingStrategyInit {
    highWaterMark: number;
  }
  interface GenericTransformStream {
    readonly readable: ReadableStream;
    readonly writable: WritableStream;
  }
  type CompressionFormat = "brotli" | "deflate" | "deflate-raw" | "gzip";
  type PerformanceEntryList = PerformanceEntry[];
  interface PerformanceMarkOptions {
    detail?: any;
    startTime?: DOMHighResTimeStamp;
  }
  interface PerformanceMeasureOptions {
    detail?: any;
    duration?: DOMHighResTimeStamp;
    end?: string | DOMHighResTimeStamp;
    start?: string | DOMHighResTimeStamp;
  }
  interface PerformanceObserverInit {
    buffered?: boolean;
    entryTypes?: string[];
    type?: string;
  }
  interface PerformanceObserverCallback {
    (
      entries: PerformanceObserverEntryList,
      observer: PerformanceObserver,
    ): void;
  }
  interface VoidFunction {
    (): void;
  }
  interface Console {
    assert(condition?: boolean, ...data: any[]): void;
    clear(): void;
    count(label?: string): void;
    countReset(label?: string): void;
    debug(...data: any[]): void;
    dir(item?: any, options?: any): void;
    dirxml(...data: any[]): void;
    error(...data: any[]): void;
    group(...data: any[]): void;
    groupCollapsed(...data: any[]): void;
    groupEnd(): void;
    info(...data: any[]): void;
    log(...data: any[]): void;
    table(tabularData?: any, properties?: string[]): void;
    time(label?: string): void;
    timeEnd(label?: string): void;
    timeLog(label?: string, ...data: any[]): void;
    trace(...data: any[]): void;
    warn(...data: any[]): void;
  }
  interface EventListener {
    (evt: Event): void;
  }
  interface EventListenerObject {
    handleEvent(object: Event): void;
  }
  interface Algorithm {
    name: string;
  }
  type HashAlgorithmIdentifier = AlgorithmIdentifier;
  interface RsaOtherPrimesInfo {
    d?: string;
    r?: string;
    t?: string;
  }
  interface RsaKeyGenParams extends Algorithm {
    modulusLength: number;
    publicExponent: BigInteger;
  }
  type NamedCurve = string;
  type EndingType = "native" | "transparent";
  type ReadableStreamReaderMode = "byob";
  interface UnderlyingSourceCancelCallback {
    (reason?: any): void | PromiseLike<void>;
  }
  interface UnderlyingSourcePullCallback<R> {
    (controller: ReadableStreamController<R>): void | PromiseLike<void>;
  }
  interface UnderlyingSourceStartCallback<R> {
    (controller: ReadableStreamController<R>): any;
  }
  type ReadableStreamType = "bytes";
  interface ReadableStreamReadValueResult<T> {
    done: false;
    value: T;
  }
  interface ReadableStreamReadDoneResult<T> {
    done: true;
    value: T | undefined;
  }
  interface UnderlyingSinkAbortCallback {
    (reason?: any): void | PromiseLike<void>;
  }
  interface UnderlyingSinkCloseCallback {
    (): void | PromiseLike<void>;
  }
  interface UnderlyingSinkStartCallback {
    (controller: WritableStreamDefaultController): any;
  }
  interface UnderlyingSinkWriteCallback<W> {
    (
      chunk: W,
      controller: WritableStreamDefaultController,
    ): void | PromiseLike<void>;
  }
  interface TransformerFlushCallback<O> {
    (controller: TransformStreamDefaultController<O>): void | PromiseLike<void>;
  }
  interface TransformerStartCallback<O> {
    (controller: TransformStreamDefaultController<O>): any;
  }
  interface TransformerTransformCallback<I, O> {
    (
      chunk: I,
      controller: TransformStreamDefaultController<O>,
    ): void | PromiseLike<void>;
  }
  type BigInteger = Uint8Array;
  type ReadableStreamController<T> =
    ReadableStreamDefaultController<T> | ReadableByteStreamController;
}

interface AbortController extends Web.Fallback<Web.AbortController> {}
declare var AbortController: Web.FallbackValue<
  "AbortController",
  typeof Web.AbortController
>;
interface AbortSignal extends Web.Fallback<Web.AbortSignal> {}
declare var AbortSignal: Web.FallbackValue<
  "AbortSignal",
  typeof Web.AbortSignal
>;
interface Event extends Web.Fallback<Web.Event> {}
declare var Event: Web.FallbackValue<"Event", typeof Web.Event>;
interface EventTarget extends Web.Fallback<Web.EventTarget> {}
declare var EventTarget: Web.FallbackValue<
  "EventTarget",
  typeof Web.EventTarget
>;
interface CustomEvent<T = any> extends Web.Fallback<Web.CustomEvent<T>> {}
declare var CustomEvent: Web.FallbackValue<
  "CustomEvent",
  typeof Web.CustomEvent
>;
interface ErrorEvent extends Web.Fallback<Web.ErrorEvent> {}
declare var ErrorEvent: Web.FallbackValue<"ErrorEvent", typeof Web.ErrorEvent>;
interface DOMException extends Web.Fallback<Web.DOMException> {}
declare var DOMException: Web.FallbackValue<
  "DOMException",
  typeof Web.DOMException
>;
// not in lib.dom yet
interface QuotaExceededError extends Web.QuotaExceededError {}
declare var QuotaExceededError: typeof Web.QuotaExceededError;
interface TextEncoder extends Web.Fallback<Web.TextEncoder> {}
declare var TextEncoder: Web.FallbackValue<
  "TextEncoder",
  typeof Web.TextEncoder
>;
interface TextDecoder extends Web.Fallback<Web.TextDecoder> {}
declare var TextDecoder: Web.FallbackValue<
  "TextDecoder",
  typeof Web.TextDecoder
>;
interface URL extends Web.Fallback<Web.URL> {}
declare var URL: Web.FallbackValue<"URL", typeof Web.URL>;
interface URLSearchParams extends Web.Fallback<Web.URLSearchParams> {}
declare var URLSearchParams: Web.FallbackValue<
  "URLSearchParams",
  typeof Web.URLSearchParams
>;
interface Crypto extends Web.Fallback<Web.Crypto> {}
declare var Crypto: Web.FallbackValue<"Crypto", typeof Web.Crypto>;
interface CryptoKey extends Web.Fallback<Web.CryptoKey> {}
declare var CryptoKey: Web.FallbackValue<"CryptoKey", typeof Web.CryptoKey>;
interface SubtleCrypto extends Web.Fallback<Web.SubtleCrypto> {}
declare var SubtleCrypto: Web.FallbackValue<
  "SubtleCrypto",
  typeof Web.SubtleCrypto
>;
declare var crypto: Crypto;
interface Blob extends Web.Fallback<Web.Blob> {}
declare var Blob: Web.FallbackValue<"Blob", typeof Web.Blob>;
interface File extends Web.Fallback<Web.File> {}
declare var File: Web.FallbackValue<"File", typeof Web.File>;
interface FormData extends Web.Fallback<Web.FormData> {}
declare var FormData: Web.FallbackValue<"FormData", typeof Web.FormData>;
interface ReadableStream<R = any> extends Web.Fallback<Web.ReadableStream<R>> {}
declare var ReadableStream: Web.FallbackValue<
  "ReadableStream",
  typeof Web.ReadableStream
>;
interface ReadableStreamDefaultReader<R = any> extends Web.Fallback<
  Web.ReadableStreamDefaultReader<R>
> {}
declare var ReadableStreamDefaultReader: Web.FallbackValue<
  "ReadableStreamDefaultReader",
  typeof Web.ReadableStreamDefaultReader
>;
interface ReadableStreamBYOBReader extends Web.Fallback<Web.ReadableStreamBYOBReader> {}
declare var ReadableStreamBYOBReader: Web.FallbackValue<
  "ReadableStreamBYOBReader",
  typeof Web.ReadableStreamBYOBReader
>;
interface ReadableStreamBYOBRequest extends Web.Fallback<Web.ReadableStreamBYOBRequest> {}
declare var ReadableStreamBYOBRequest: Web.FallbackValue<
  "ReadableStreamBYOBRequest",
  typeof Web.ReadableStreamBYOBRequest
>;
interface ReadableStreamDefaultController<R = any> extends Web.Fallback<
  Web.ReadableStreamDefaultController<R>
> {}
declare var ReadableStreamDefaultController: Web.FallbackValue<
  "ReadableStreamDefaultController",
  typeof Web.ReadableStreamDefaultController
>;
interface ReadableByteStreamController extends Web.Fallback<Web.ReadableByteStreamController> {}
declare var ReadableByteStreamController: Web.FallbackValue<
  "ReadableByteStreamController",
  typeof Web.ReadableByteStreamController
>;
interface WritableStream<W = any> extends Web.Fallback<Web.WritableStream<W>> {}
declare var WritableStream: Web.FallbackValue<
  "WritableStream",
  typeof Web.WritableStream
>;
interface WritableStreamDefaultWriter<W = any> extends Web.Fallback<
  Web.WritableStreamDefaultWriter<W>
> {}
declare var WritableStreamDefaultWriter: Web.FallbackValue<
  "WritableStreamDefaultWriter",
  typeof Web.WritableStreamDefaultWriter
>;
interface WritableStreamDefaultController extends Web.Fallback<Web.WritableStreamDefaultController> {}
declare var WritableStreamDefaultController: Web.FallbackValue<
  "WritableStreamDefaultController",
  typeof Web.WritableStreamDefaultController
>;
interface TransformStream<I = any, O = any> extends Web.Fallback<
  Web.TransformStream<I, O>
> {}
declare var TransformStream: Web.FallbackValue<
  "TransformStream",
  typeof Web.TransformStream
>;
interface TransformStreamDefaultController<O = any> extends Web.Fallback<
  Web.TransformStreamDefaultController<O>
> {}
declare var TransformStreamDefaultController: Web.FallbackValue<
  "TransformStreamDefaultController",
  typeof Web.TransformStreamDefaultController
>;
interface ByteLengthQueuingStrategy extends Web.Fallback<Web.ByteLengthQueuingStrategy> {}
declare var ByteLengthQueuingStrategy: Web.FallbackValue<
  "ByteLengthQueuingStrategy",
  typeof Web.ByteLengthQueuingStrategy
>;
interface CountQueuingStrategy extends Web.Fallback<Web.CountQueuingStrategy> {}
declare var CountQueuingStrategy: Web.FallbackValue<
  "CountQueuingStrategy",
  typeof Web.CountQueuingStrategy
>;
interface TextEncoderStream extends Web.Fallback<Web.TextEncoderStream> {}
declare var TextEncoderStream: Web.FallbackValue<
  "TextEncoderStream",
  typeof Web.TextEncoderStream
>;
interface TextDecoderStream extends Web.Fallback<Web.TextDecoderStream> {}
declare var TextDecoderStream: Web.FallbackValue<
  "TextDecoderStream",
  typeof Web.TextDecoderStream
>;
interface CompressionStream extends Web.Fallback<Web.CompressionStream> {}
declare var CompressionStream: Web.FallbackValue<
  "CompressionStream",
  typeof Web.CompressionStream
>;
interface DecompressionStream extends Web.Fallback<Web.DecompressionStream> {}
declare var DecompressionStream: Web.FallbackValue<
  "DecompressionStream",
  typeof Web.DecompressionStream
>;
interface Performance extends Web.Fallback<Web.Performance> {}
declare var performance: Performance;
declare var Performance: Web.FallbackValue<
  "Performance",
  typeof Web.Performance
>;
interface PerformanceEntry extends Web.Fallback<Web.PerformanceEntry> {}
declare var PerformanceEntry: Web.FallbackValue<
  "PerformanceEntry",
  typeof Web.PerformanceEntry
>;
interface PerformanceMark extends Web.Fallback<Web.PerformanceMark> {}
declare var PerformanceMark: Web.FallbackValue<
  "PerformanceMark",
  typeof Web.PerformanceMark
>;
interface PerformanceMeasure extends Web.Fallback<Web.PerformanceMeasure> {}
declare var PerformanceMeasure: Web.FallbackValue<
  "PerformanceMeasure",
  typeof Web.PerformanceMeasure
>;
interface PerformanceObserver extends Web.Fallback<Web.PerformanceObserver> {}
declare var PerformanceObserver: Web.FallbackValue<
  "PerformanceObserver",
  typeof Web.PerformanceObserver
>;
interface PerformanceObserverEntryList extends Web.Fallback<Web.PerformanceObserverEntryList> {}
declare var PerformanceObserverEntryList: Web.FallbackValue<
  "PerformanceObserverEntryList",
  typeof Web.PerformanceObserverEntryList
>;
interface Console extends Web.Fallback<Web.Console> {}
declare var console: Console;
interface WorkerNavigator extends Web.WorkerFallback<Web.WorkerNavigator> {}
declare var WorkerNavigator: Web.WorkerFallbackValue<
  "WorkerNavigator",
  typeof Web.WorkerNavigator
>;
interface Navigator extends Web.Fallback<Web.WorkerNavigator> {}
declare var navigator: Navigator;
declare function atob(data: string): string;
declare function btoa(data: string): string;
interface StructuredSerializeOptions extends Web.Fallback<Web.StructuredSerializeOptions> {}
declare function structuredClone<T = any>(
  value: T,
  options?: StructuredSerializeOptions,
): T;
declare function queueMicrotask(callback: () => void): void;
declare function setTimeout(
  handler: string | ((...args: any[]) => void),
  timeout?: number,
  ...args: any[]
): number;
declare function clearTimeout(id?: number): void;
declare function setInterval(
  handler: string | ((...args: any[]) => void),
  timeout?: number,
  ...args: any[]
): number;
declare function clearInterval(id?: number): void;
