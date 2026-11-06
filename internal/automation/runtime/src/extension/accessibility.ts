import { invoke } from "./async";
import { errorFrom } from "./errors";
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
import { act, check, optionsOf, signalOf, type Options } from "./options";

const depthLimit = 56;

const depthOf = (depth: unknown): number | undefined => {
  if (depth === undefined) {
    return undefined;
  }
  if (typeof depth !== "number" || Number.isNaN(depth)) {
    throw new TypeError("depth must be a number");
  }
  if (Number.isFinite(depth) && !Number.isInteger(depth)) {
    throw new TypeError("depth must be an integer");
  }
  if ((depth as number) < 0 || (depth as number) > depthLimit) {
    throw new RangeError(`depth must be from 0 to ${depthLimit}`);
  }
  return depth as number;
};

const view = (options: unknown) => {
  const {
    scope,
    within,
    full = false,
    depth,
  } = optionsOf(options, ["scope", "within", "full", "depth"]);
  if (scope !== undefined && scope !== "frontApp" && scope !== "screen") {
    throw new TypeError("scope must be frontApp or screen");
  }
  if (typeof full !== "boolean") {
    throw new TypeError("full must be a boolean");
  }
  if (within === undefined) {
    return { scope: scope ?? "frontApp", full, depth: depthOf(depth) };
  }
  if (scope !== undefined) {
    throw new TypeError("scope and within are exclusive");
  }
  if (!(within instanceof AccessibilityElement)) {
    throw new TypeError("within takes an element of a snapshot");
  }
  return {
    within: refOf(within),
    full,
    depth: depthOf(depth),
  };
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

interface Found {
  snapshot: RawSnapshot | null;
  seen: number;
  failure?: string;
}

interface Search {
  mode: "best" | "unique";
  timeout: number;
  pollLimit: number;
  signal?: AbortSignal;
  depth?: number;
}

// locate reads the screen until one of the selectors matches, taking the
// match the ranking puts first; in "unique" mode a tie throws.
const locate = async (
  selectors: Selector[],
  { mode, timeout, pollLimit, signal, depth }: Search,
): Promise<AccessibilityElement | null> => {
  const encoded = selectors.map((item) => encode(item).value);
  const read = { depth };
  const started = Date.now();
  const deadline = started + timeout;
  const pollUntil = started + pollLimit;
  for (;;) {
    const found = await invoke<Found>("accessibility.find", [encoded, read], {
      signal,
    });
    if (found.snapshot !== null) {
      const taken = snapshotOf(found.snapshot);
      for (const item of selectors) {
        const element = pick(taken, item, mode);
        if (element !== null) {
          return element;
        }
      }
    }
    const now = Date.now();
    if (now >= deadline) {
      if (found.failure !== undefined) {
        throw errorFrom(JSON.parse(found.failure));
      }
      return null;
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
const on = (
  element: string | Selector,
  operation: string,
  limit: number,
  options: unknown,
  run: (ref: string, limit: number) => Promise<void>,
): Promise<void> => {
  if (typeof element === "string") {
    return run(element, limit);
  }
  const started = Date.now();
  return locate([element], {
    mode: "unique",
    timeout: limit,
    pollLimit: -Infinity,
    signal: signalOf(options),
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
    depth,
  } = optionsOf(options, ["timeoutMs", "poll", "pollTimeoutMs", "depth"]);
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
    depth: depthOf(depth),
  });
};

export const setText = (element: unknown, text: unknown, options?: Options) => {
  const target = elementOf(element, "accessibility.setText");
  if (typeof text !== "string") {
    throw new TypeError("the text must be a string");
  }
  const { timeoutMs } = optionsOf(options, ["timeoutMs"]);
  const limit = limitOf(timeoutMs, "timeoutMs");
  return on(target, "accessibility.setText", limit, options, (ref, left) =>
    act(
      "accessibility.setText",
      [ref, text, left === Infinity ? null : left],
      options,
    ),
  );
};

export const tap = (element: unknown, options?: Options) => {
  const target = elementOf(element, "accessibility.tap");
  const { timeoutMs } = optionsOf(options, ["timeoutMs"]);
  const limit = limitOf(timeoutMs, "timeoutMs");
  return on(target, "accessibility.tap", limit, options, (ref, left) =>
    act("accessibility.tap", [ref, left === Infinity ? null : left], options),
  );
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
  return on(target, "accessibility.perform", 0, checked, (ref) =>
    act("accessibility.perform", [ref, action], checked),
  );
};
