import { invoke } from "./async";
import { observed, observeOf, tapResultOf, type RawTap } from "./accessibility";
import {
  act,
  check,
  optionsOf,
  point,
  signalOf,
  type Options,
} from "./options";

const button = (name: string) => (options?: Options) =>
  act("input.button", [name], check(options, []));

export const tap = async (at: unknown, options?: Options) => {
  const { observe, verify, ...rest } = optionsOf(options, [
    "count",
    "verify",
    "observe",
  ]);
  if (verify !== undefined && typeof verify !== "boolean") {
    throw new TypeError("verify must be a boolean");
  }
  const watched = observeOf(observe);
  const signal = signalOf(options);
  const raw = await invoke<RawTap | null>(
    "input.tap",
    [
      point(at, "the point"),
      { ...rest, verify: verify === true || watched !== undefined },
    ],
    { signal },
  );
  return observed(tapResultOf(raw), watched, signal);
};

const moved =
  (operation: string, allowed: string[]) =>
  async (from: unknown, to: unknown, options?: Options) => {
    const { observe, ...rest } = optionsOf(options, [...allowed, "observe"]);
    const watched = observeOf(observe);
    await act(operation, [point(from, "from"), point(to, "to"), rest], options);
    return observed({}, watched, signalOf(options));
  };

export const swipe = moved("input.swipe", ["durationMs"]);
export const drag = moved("input.drag", ["durationMs", "pressMs"]);
export const pinch = (center: unknown, scale: number, options?: Options) =>
  act(
    "input.pinch",
    [point(center, "the center"), scale, optionsOf(options, ["durationMs"])],
    options,
  );
export const touch = (fingers: unknown, options?: Options) =>
  act("input.touch", [fingers], check(options, []));
export const pressKey = (combination: string, options?: Options) => {
  if (typeof combination !== "string") {
    throw new TypeError("the key combination must be a string");
  }
  return act("input.pressKey", [combination], check(options, []));
};
export const pressHome = button("home");
export const openAppSwitcher = button("appSwitcher");
export const pressPower = button("power");
export const pressVolumeUp = button("volumeUp");
export const pressVolumeDown = button("volumeDown");
