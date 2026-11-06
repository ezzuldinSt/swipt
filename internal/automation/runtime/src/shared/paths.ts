// A Windows path, C:\dir or \\server\share, as the host's filepath gives it.
const windowsPath = /^(?:[A-Za-z]:[\\/]|\\\\)/;

const drivePath = /^[A-Za-z]:\//;

export const joinRelative = (base: string, path: string): string => {
  if (typeof path !== "string") {
    throw new TypeError("the path must be a string");
  }
  if (path.startsWith("/") || windowsPath.test(path)) {
    throw new TypeError(
      `${JSON.stringify(path)} is absolute; give a path inside the directory`,
    );
  }
  const segments = base === "" ? [] : base.split("/");
  for (const part of path.split("/")) {
    if (part === "" || part === ".") {
      continue;
    }
    if (part === "..") {
      if (segments.length === 0) {
        throw new TypeError(`${JSON.stringify(path)} leaves the directory`);
      }
      segments.pop();
      continue;
    }
    segments.push(part);
  }
  return segments.join("/");
};

export const nameOf = (path: string): string => path.split("/").at(-1) ?? "";

// slashed spells a Windows path with forward slashes; any other path is kept.
export const slashed = (absolute: string): string =>
  windowsPath.test(absolute) ? absolute.replaceAll("\\", "/") : absolute;

// A drive path takes a slash before its letter, as file:///C:/dir (RFC 8089
// appendix E.2).
export const directoryURI = (absolute: string): string => {
  const path = slashed(absolute);
  return `file://${drivePath.test(path) ? "/" : ""}${encodeURI(path).replace(/[?#]/g, encodeURIComponent)}`;
};

// absoluteOf reads a file URI or an absolute path back into the spelling of
// slashed; Windows paths compare without case.
export const absoluteOf = (
  value: string,
): { path: string; folded: boolean } => {
  let path = value.startsWith("file://")
    ? decodeURIComponent(value.slice("file://".length))
    : value;
  if (/^\/[A-Za-z]:\//.test(path)) {
    path = path.slice(1);
  }
  path = slashed(path);
  return { path, folded: drivePath.test(path) || path.startsWith("//") };
};

export const fileURI = (
  root: string,
  path: string,
  directory: boolean,
): string => {
  const base = root.replace(/\/+$/, "");
  const encoded =
    path === "" ? "" : `/${path.split("/").map(encodeURIComponent).join("/")}`;
  return `${base}${encoded}${directory ? "/" : ""}`;
};
