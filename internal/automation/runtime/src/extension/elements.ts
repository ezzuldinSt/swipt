import type { Point, Rect } from "@swipt/api";
import type {
  AccessibilityElement as AccessibilityElementInterface,
  AccessibilitySnapshot as AccessibilitySnapshotInterface,
  AccessibilityTrait,
} from "@swipt/api/accessibility";

import { errorFrom } from "./errors";
import { native } from "./native";

export interface Raw {
  ref: string;
  role: string;
  children?: Raw[];
  traits?: AccessibilityTrait[];
  [field: string]: unknown;
}

export interface RawSnapshot {
  id: string;
  app: string;
  time: number;
  root: Raw;
  system?: Raw;
}

const refs = new WeakMap<AccessibilityElement, string>();
const homes = new WeakMap<AccessibilityElement, AccessibilitySnapshot>();
const ids = new WeakMap<AccessibilitySnapshot, string>();
const matches = new WeakMap<
  AccessibilitySnapshot,
  Map<string, AccessibilityElement>
>();

export const refOf = (element: AccessibilityElement): string =>
  refs.get(element)!;

const clip = (text: string, limit: number) =>
  text.length > limit
    ? `${text.slice(0, limit - limit / 3)}…${text.slice(-limit / 3)}`
    : text;
const flags = [
  "selected",
  "focused",
  "disabled",
  "secure",
  "partial",
  "offScreen",
  "inert",
  "remote",
] as const;

export class AccessibilityElement implements AccessibilityElementInterface {
  declare readonly role: string;
  declare readonly class?: string;
  declare readonly traits: readonly AccessibilityTrait[];
  declare readonly label?: string;
  declare readonly value?: string;
  declare readonly identifier?: string;
  declare readonly placeholder?: string;
  declare readonly hint?: string;
  declare readonly url?: string;
  declare readonly frame?: Rect;
  declare readonly point?: Point;
  declare readonly partial?: true;
  declare readonly offScreen?: true;
  declare readonly inert?: true;
  declare readonly remote?: true;
  declare readonly disabled?: true;
  declare readonly focused?: true;
  declare readonly selected?: true;
  declare readonly expanded?: boolean;
  declare readonly secure?: true;
  declare readonly selection?: [location: number, length: number];
  declare readonly actions?: readonly string[];
  declare readonly children: readonly AccessibilityElement[];
  declare readonly parent?: AccessibilityElement;

  find(selector: Selector): AccessibilityElement | null {
    const home = homes.get(this);
    return home === undefined
      ? null
      : (search(home, refOf(this), selector, "one")[0] ?? null);
  }

  findAll(selector: Selector): AccessibilityElement[] {
    const home = homes.get(this);
    return home === undefined ? [] : search(home, refOf(this), selector, "all");
  }

  toString(): string {
    return line(this);
  }
}

export class AccessibilitySnapshot implements AccessibilitySnapshotInterface {
  declare readonly app: string;
  declare readonly time: number;
  declare readonly root: AccessibilityElement;
  declare readonly system?: AccessibilityElement;

  find(selector: Selector): AccessibilityElement | null {
    return search(this, "", selector, "one")[0] ?? null;
  }

  findAll(selector: Selector): AccessibilityElement[] {
    return search(this, "", selector, "all");
  }

  toString(): string {
    const app = `${this.app}\n${outline(this.root)}`;
    return this.system === undefined
      ? app
      : `${app}\ncom.apple.springboard, over the app\n${outline(this.system)}`;
  }
}

export function wrap(
  raw: Raw,
  parent?: AccessibilityElement,
  home?: AccessibilitySnapshot,
  registry?: Map<string, AccessibilityElement>,
): AccessibilityElement {
  const { ref, children = [], traits = [], ...fields } = raw;
  const element = Object.assign(
    Object.create(AccessibilityElement.prototype),
    fields,
    { traits },
  ) as AccessibilityElement;
  refs.set(element, ref);
  if (home !== undefined) {
    homes.set(element, home);
    registry?.set(ref, element);
  }
  if (parent !== undefined) {
    Object.defineProperty(element, "parent", { value: parent });
  }
  Object.defineProperty(element, "children", {
    value: children.map((child) => wrap(child, element, home, registry)),
    enumerable: true,
  });
  return Object.freeze(element);
}

export function snapshotOf(raw: RawSnapshot): AccessibilitySnapshot {
  const snapshot = Object.create(
    AccessibilitySnapshot.prototype,
  ) as AccessibilitySnapshot;
  ids.set(snapshot, raw.id);
  Object.assign(snapshot, {
    app: raw.app,
    time: raw.time,
    root: wrap(raw.root, undefined, snapshot),
  });
  if (raw.system !== undefined) {
    Object.assign(snapshot, { system: wrap(raw.system, undefined, snapshot) });
  }
  return Object.freeze(snapshot);
}

export const adopt = (
  home: AccessibilitySnapshot,
  raw: Raw,
): AccessibilityElement => {
  let found = matches.get(home);
  if (found === undefined) {
    found = new Map();
    matches.set(home, found);
  }
  return found.get(raw.ref) ?? wrap(raw, undefined, home, found);
};

export const line = (element: AccessibilityElement): string => {
  const parts = [element.role];
  const record = element as unknown as Record<string, unknown>;
  if (element.class !== undefined) {
    parts.push(`class=${JSON.stringify(clip(element.class, 90))}`);
  }
  if (element.label !== undefined) {
    parts.push(JSON.stringify(clip(element.label, 300)));
  }
  if (element.value !== undefined) {
    parts.push(`value=${JSON.stringify(clip(element.value, 300))}`);
  }
  if (element.placeholder !== undefined) {
    parts.push(`placeholder=${JSON.stringify(element.placeholder)}`);
  }
  if (element.identifier !== undefined) {
    parts.push(`id=${JSON.stringify(clip(element.identifier, 120))}`);
  }
  if (element.url !== undefined) {
    parts.push(`url=${JSON.stringify(clip(element.url, 100))}`);
  }
  if (element.frame !== undefined) {
    parts.push(`[${element.frame.map(Math.round).join(",")}]`);
  }
  for (const flag of flags) {
    if (record[flag]) {
      parts.push(flag);
    }
  }
  if (element.expanded !== undefined) {
    parts.push(element.expanded ? "expanded" : "collapsed");
  }
  if (element.actions !== undefined) {
    parts.push(`actions=${JSON.stringify(element.actions)}`);
  }
  return parts.join(" ");
};

const outline = (root: AccessibilityElement): string => {
  const lines: string[] = [];
  const visit = (element: AccessibilityElement, depth: number) => {
    lines.push("  ".repeat(depth) + line(element));
    if (element.role === "keyboard") {
      return;
    }
    for (const child of element.children) {
      visit(child, depth + 1);
    }
  };
  visit(root, 0);
  return lines.join("\n");
};

export type Selector =
  Record<string, unknown> | ((element: AccessibilityElement) => boolean);

const relations = ["within", "parent", "has", "not"];

export const isSelector = (value: unknown): value is Selector =>
  typeof value === "function" ||
  (value !== null &&
    typeof value === "object" &&
    !Array.isArray(value) &&
    !(value instanceof AccessibilityElement));

export interface Encoded {
  value: unknown;
  holes: ((value: string, home?: AccessibilitySnapshot) => boolean)[];
}

export const encode = (selector: Selector): Encoded => {
  const holes: Encoded["holes"] = [];
  const convert = (value: unknown): unknown => {
    if (typeof value === "function") {
      holes.push((argument, home) => {
        const element =
          home === undefined
            ? undefined
            : wrap(JSON.parse(argument) as Raw, undefined, home, undefined);
        return (
          element !== undefined &&
          Boolean(
            (value as (element: AccessibilityElement) => unknown)(element),
          )
        );
      });
      return { $predicate: holes.length - 1 };
    }
    if (!isSelector(value)) {
      throw new TypeError(
        "expected a selector: an object of element fields, or a function of an element",
      );
    }
    const out: Record<string, unknown> = {};
    for (const [key, field] of Object.entries(value)) {
      if (field instanceof RegExp) {
        holes.push((text) => text.search(field) >= 0);
        out[key] = {
          $regex: field.source,
          $flags: field.flags,
          $id: holes.length - 1,
        };
      } else if (
        relations.includes(key) &&
        field instanceof AccessibilityElement
      ) {
        out[key] = { $element: refOf(field) };
      } else if (relations.includes(key) && isSelector(field)) {
        out[key] = convert(field);
      } else {
        out[key] = field === undefined ? null : field;
      }
    }
    return out;
  };
  return { value: convert(selector), holes };
};

const search = (
  home: AccessibilitySnapshot,
  scope: string,
  selector: Selector,
  mode: "one" | "all" | "best" | "unique",
): AccessibilityElement[] => {
  const encoded = encode(selector);
  let thrown: { error: unknown } | undefined;
  const answer = native.match(
    ids.get(home)!,
    scope,
    JSON.stringify(encoded.value),
    mode,
    (_kind, id, value) => {
      try {
        return encoded.holes[Number(id)](value, home) ? "true" : "false";
      } catch (error) {
        thrown = { error };
        return "throw";
      }
    },
  );
  if (thrown !== undefined) {
    throw thrown.error;
  }
  if (answer.failure !== undefined) {
    throw errorFrom(JSON.parse(answer.failure));
  }
  return (answer.elements ?? []).map((raw) => adopt(home, raw));
};

// pick finds the match an action most likely means: "best" answers the first
// of those the ranking cannot tell apart, and "unique" throws listing them.
export const pick = (
  home: AccessibilitySnapshot,
  selector: Selector,
  mode: "best" | "unique",
): AccessibilityElement | null => search(home, "", selector, mode)[0] ?? null;
