import {
  absoluteOf,
  directoryURI,
  fileURI,
  joinRelative,
  nameOf,
  slashed,
} from "../shared/paths";
import { invoke } from "./async";
import { Blob } from "./blob";
import { TextDecoder, TextEncoder } from "./encoding";
import { native } from "./native";
import {
  chunkBytes,
  nativeReadable,
  nativeWritable,
  ReadableStream,
  takeNative,
} from "./streams";

type Root = "data" | "assets" | "cache";

type BufferSource = ArrayBuffer | ArrayBufferView;

interface Location {
  readonly root: Root;
  readonly path: string;
}

interface Stat {
  kind: "file" | "directory";
  size: number;
  modified: number;
}

const token = Symbol("location");
const locations = new WeakMap<FileHandle | DirectoryHandle, Location>();

const rootPaths: Record<Root, () => string> = {
  data: () => native.dataDirectory,
  assets: () => native.assetsDirectory,
  cache: () => native.cacheDirectory,
};

const rootPath = (root: Root): string => rootPaths[root]().replace(/\/+$/, "");

const at = (handle: FileHandle | DirectoryHandle): Location =>
  locations.get(handle)!;

export const fileLocation = (handle: unknown): Location | undefined =>
  handle instanceof FileHandle ? at(handle) : undefined;

export const pathOf = (handle: FileHandle | DirectoryHandle): string => {
  const location = at(handle);
  return location.path === ""
    ? rootPath(location.root)
    : `${rootPath(location.root)}/${location.path}`;
};

const uriOf = (location: Location, directory: boolean): string =>
  fileURI(directoryURI(rootPath(location.root)), location.path, directory);

const call = <T>(name: string, ...args: unknown[]): Promise<T> =>
  invoke<T>(`fs.${name}`, args);

const child = (parent: Location, path: string): Location => ({
  root: parent.root,
  path: joinRelative(parent.path, path),
});

const bytesOf = async (data: unknown): Promise<Uint8Array> => {
  if (typeof data === "string") {
    return new TextEncoder().encode(data);
  }
  if (data instanceof Blob) {
    return data.bytes();
  }
  if (ArrayBuffer.isView(data) || data instanceof ArrayBuffer) {
    return chunkBytes(data);
  }
  throw new TypeError(
    "the data must be a string, an ArrayBuffer, an ArrayBufferView, a Blob or a ReadableStream",
  );
};

const guard = (key: symbol) => {
  if (key !== token) {
    throw new TypeError(
      "file handles come from dataDirectory, cacheDirectory, assets or from Swipt",
    );
  }
};

export class DirectoryHandle {
  constructor(key: symbol, location: Location) {
    guard(key);
    locations.set(this, location);
    Object.freeze(this);
  }

  get uri(): string {
    return uriOf(at(this), true);
  }

  get name(): string {
    return nameOf(pathOf(this));
  }

  file(path: string): FileHandle {
    return new FileHandle(token, child(at(this), path));
  }

  directory(path: string): DirectoryHandle {
    return new DirectoryHandle(token, child(at(this), path));
  }

  async exists(): Promise<boolean> {
    const { root, path } = at(this);
    return (await call<Stat | null>("stat", root, path))?.kind === "directory";
  }

  async create(): Promise<void> {
    const { root, path } = at(this);
    await call("mkdir", root, path);
  }

  async list(): Promise<(FileHandle | DirectoryHandle)[]> {
    const location = at(this);
    const entries = await call<{ name: string; directory: boolean }[]>(
      "list",
      location.root,
      location.path,
    );
    return entries.map((entry) =>
      entry.directory
        ? new DirectoryHandle(token, child(location, entry.name))
        : new FileHandle(token, child(location, entry.name)),
    );
  }

  async delete(): Promise<void> {
    const { root, path } = at(this);
    await call("remove", root, path);
  }

  toString(): string {
    return `Directory ${this.uri}`;
  }
}

export class FileHandle {
  constructor(key: symbol, location: Location) {
    guard(key);
    locations.set(this, location);
    Object.freeze(this);
  }

  get uri(): string {
    return uriOf(at(this), false);
  }

  get name(): string {
    return nameOf(at(this).path);
  }

  async stat(): Promise<{ size: number; modified: number } | null> {
    const { root, path } = at(this);
    const info = await call<Stat | null>("stat", root, path);
    return info?.kind === "file"
      ? { size: info.size, modified: info.modified }
      : null;
  }

  async bytes(): Promise<Uint8Array> {
    const { root, path } = at(this);
    return call<Uint8Array>("read", root, path);
  }

  async text(): Promise<string> {
    return new TextDecoder().decode(await this.bytes());
  }

  stream(): ReadableStream<Uint8Array> {
    const { root, path } = at(this);
    return nativeReadable(native.openFile(root, path));
  }

  async write(
    data: string | BufferSource | Blob | ReadableStream<BufferSource>,
  ): Promise<void> {
    const { root, path } = at(this);
    if (!(data instanceof ReadableStream)) {
      await call("write", root, path, native.put(await bytesOf(data)));
      return;
    }
    if (data.locked) {
      throw new TypeError("the stream is locked");
    }
    const writer = await call<string>("writer", root, path);
    const source = takeNative(data);
    if (source === null) {
      await data.pipeTo(nativeWritable(writer));
    } else {
      await invoke("stream.pipe", [source, writer]);
    }
  }

  async copy(to: FileHandle): Promise<void> {
    const from = at(this);
    const target = at(handleOf(to, "the destination"));
    await call("copy", from.root, from.path, target.root, target.path);
  }

  async move(to: FileHandle): Promise<void> {
    const from = at(this);
    const target = at(handleOf(to, "the destination"));
    await call("move", from.root, from.path, target.root, target.path);
  }

  async delete(): Promise<void> {
    const { root, path } = at(this);
    await call("remove", root, path);
  }

  toString(): string {
    return `File ${this.uri}`;
  }
}

const handleOf = (value: unknown, name: string): FileHandle => {
  if (!(value instanceof FileHandle)) {
    throw new TypeError(`${name} must be a FileHandle`);
  }
  return value;
};

let data: DirectoryHandle | undefined;
let assetsRoot: DirectoryHandle | undefined;
let cache: DirectoryHandle | undefined;

export const dataDirectory = (): DirectoryHandle =>
  (data ??= new DirectoryHandle(token, { root: "data", path: "" }));

export const assets = (): DirectoryHandle =>
  (assetsRoot ??= new DirectoryHandle(token, { root: "assets", path: "" }));

export const cacheDirectory = (): DirectoryHandle =>
  (cache ??= new DirectoryHandle(token, { root: "cache", path: "" }));

export const fileOf = (value: unknown, name: string): FileHandle => {
  if (value instanceof FileHandle) {
    return value;
  }
  if (typeof value === "string") {
    return dataDirectory().file(value);
  }
  throw new TypeError(
    `${name} must be a FileHandle or a path in the data directory`,
  );
};

export const handleOfPath = (value: string): FileHandle => {
  const absolute = absoluteOf(value);
  for (const root of ["data", "assets", "cache"] as const) {
    const prefix = `${slashed(rootPath(root))}/`;
    const head = absolute.path.slice(0, prefix.length);
    if (
      absolute.folded
        ? head.toLowerCase() === prefix.toLowerCase()
        : head === prefix
    ) {
      return new FileHandle(token, {
        root,
        path: absolute.path.slice(prefix.length).replace(/\/+$/, ""),
      });
    }
  }
  throw new TypeError(
    `${JSON.stringify(value)} is outside the directories Swipt provides`,
  );
};
