import type { CallOptions, Point, Rect } from "./index";
import type { RasterImage } from "./image";

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

export declare function snapshot(
  options?: SnapshotOptions,
): Promise<AccessibilitySnapshot>;
/** Of several matches, answers the one an action most likely means, preferring in turn one that shows on the screen, one nothing covers, an enabled one, a control, and one at least twice the size of the others; among matches alike, the first in the tree. */
export declare function find(
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

/**
 * "target": the screen changed at the element (or within 44 pt of a tapped point) after the touch, and was still changed 350 ms later, or the element's value or selection changed; "elsewhere": it changed only away from it and stayed so, or the element went, or another app came to the front.
 * Changes of at most four spots of 12 by 12 pt, as a ticking clock hand or a blinking caret make, changes in places that changed on their own before the touch, and the device's screen-change notifications alone do not count.
 * "moved": the element moved away from the touch as it went, as a list does that loads after a launch, so the touch most likely reached another element, which the detail names when the hit test can; the tap is not tried again.
 * "none": nothing changed within 800 ms, or the change went back, as a press highlight or an icon's bounce does. A correct tap can change nothing visible, as on a control already in that state, so "none" is advice, not an error.
 * "unknown": the stream sent no frame the device captured after the window (or after the change could have held) within 1.5 s more, as a stalled link does, and the device reported nothing; the tap is not tried again.
 * Frames count by when the device captured them, so a change captured before the touch reached the device and rendered, which came late, does not count.
 */
export type TapEffect = "target" | "elsewhere" | "moved" | "none" | "unknown";

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
   * A whole, clear and steady element is never tried again: its tap may rightly change nothing, or show it after 800 ms, and a second tap would act twice. Nor is any element when the live screen cannot be watched. Nor is an element already selected, or one whose label or identifier names an irreversible or outward action (Send, Pay, Buy, Delete, Remove, Reset, Confirm, Allow, Call, Post, Share, Sign Out and the like, in English).
   */
  readonly recover?: boolean;
  /** With a selector that matches nothing on the screen, searches the pages of the screen as find's `scroll` does. */
  readonly scroll?: boolean;
}

/** A selector picks its match as find does, and throws when several rank alike, listing them. An element off the screen or under the app's own bars, in a scroll view, is first scrolled 80 pt clear of the bars. An element of an app that came to the front less than 1.5 s ago, or that moved since its read, is tapped only once two reads of its frame 100 ms apart agree, waiting up to 1 s; one that moved is read again first. */
export declare function tap(
  element: AccessibilityElement | AccessibilitySelector,
  options?: TapElementOptions,
): Promise<TapResult>;
export declare function setText(
  element: AccessibilityElement | AccessibilitySelector,
  text: string,
  options?: ElementOptions,
): Promise<void>;
export declare function perform(
  element: AccessibilityElement | AccessibilitySelector,
  action: AccessibilityAction | (string & {}),
  options?: CallOptions,
): Promise<void>;
