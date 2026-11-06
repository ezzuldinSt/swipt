import { find, perform, setText, snapshot, tap } from "./accessibility";
import * as deviceModule from "./device";
import { decode, handleOf, RasterImage } from "./image";
import * as inputModule from "./input";
import { locate } from "./locate";
import {
  find as findOnScreen,
  frames,
  openRecording,
  record,
  screenshot,
  waitFor,
  waitForChange,
  waitForStable,
} from "./screen";

export const accessibility = Object.freeze({
  snapshot,
  find,
  tap,
  setText,
  perform,
});
export const input = Object.freeze({ ...inputModule });
export const device = Object.freeze({ ...deviceModule });
export const screen = Object.freeze({
  screenshot,
  find: findOnScreen,
  locate,
  frames,
  waitFor,
  waitForChange,
  waitForStable,
  record,
  openRecording,
});
export const image = Object.freeze({ decode });
export { handleOf, RasterImage };
