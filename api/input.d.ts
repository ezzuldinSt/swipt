import type { CallOptions, Point } from "./index";
import type { Observation, ObserveOptions, TapResult } from "./accessibility";

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
export declare function tap(
  point: Point,
  options?: TapOptions,
): Promise<TapResult>;
/** Answers an empty object, or with `observe` what the scroll settled on. */
export declare function swipe(
  from: Point,
  to: Point,
  options?: CallOptions & ObserveOptions & { durationMs?: number },
): Promise<Observation>;
export declare function drag(
  from: Point,
  to: Point,
  options?: CallOptions &
    ObserveOptions & { durationMs?: number; pressMs?: number },
): Promise<Observation>;
export declare function pinch(
  center: Point,
  scale: number,
  options?: CallOptions & { durationMs?: number },
): Promise<void>;
export declare function touch(
  fingers: readonly (readonly TouchSample[])[],
  options?: CallOptions,
): Promise<void>;
export declare function pressKey(
  combination: KeyCombination,
  options?: CallOptions,
): Promise<void>;
export declare function pressHome(options?: CallOptions): Promise<void>;
export declare function openAppSwitcher(options?: CallOptions): Promise<void>;
export declare function pressPower(options?: CallOptions): Promise<void>;
export declare function pressVolumeUp(options?: CallOptions): Promise<void>;
export declare function pressVolumeDown(options?: CallOptions): Promise<void>;
