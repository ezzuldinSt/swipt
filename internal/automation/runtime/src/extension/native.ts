import type { Raw } from "./elements";
import type { Position } from "./format";

export interface Native {
  readonly swiplet: string;
  readonly languages: string;
  readonly dataDirectory: string;
  readonly assetsDirectory: string;
  readonly cacheDirectory: string;
  readonly extensionHead: string;
  readonly timeOrigin: string;
  now(): number;
  start(id: string, name: string, args: string): void;
  cancel(id: string): void;
  failure(id: string): string;
  value(id: string): unknown;
  subscribe(id: string, kind: string, args: string): void;
  unsubscribe(id: string): void;
  print(level: string, text: string): void;
  put(data: Uint8Array): string;
  take(key: string): Uint8Array;
  random(length: number): Uint8Array;
  digest(algorithm: string, data: Uint8Array): Uint8Array;
  finish(failure: string, output: string, id?: string): void;
  show(image: Uint8Array): void;
  match(
    snapshot: string,
    scope: string,
    json: string,
    mode: "one" | "all" | "best" | "unique",
    call: (kind: string, id: string, value: string) => string,
  ): { elements?: Raw[]; failure?: string; thrown?: boolean };
  preferredLocalizations(localizations: string): string[];
  locate(line: number, column: number): Position | undefined;
  sqlite(request: string): unknown;
  encodeText(text: string): Uint8Array;
  decodeText(data: Uint8Array, fatal: number): string | number;
  release(id: string, reason?: string): void;
  compression(
    format: string,
    mode: "compress" | "decompress",
  ): [writer: string, reader: string];
  openFile(root: string, path: string): string;
  publishObjectURL?(url: string, data: Uint8Array | null): void;
  chunk(name: string): (core: object) => unknown;
  setTimeout(
    callback: (...args: unknown[]) => void,
    timeout: number,
    ...args: unknown[]
  ): number;
  setInterval(
    callback: (...args: unknown[]) => void,
    timeout: number,
    ...args: unknown[]
  ): number;
  clearTimer(id: number): void;
  readonly graphics?: GraphicsNative;
}

export interface GraphicsNative {
  prepare(method: string, args: string, objects: readonly object[]): number;
  take(id: number): object | null;
  bytes(id: number): Uint8Array | null;
}

export let native: Native;

export function bind(value: Native): void {
  native = value;
}
