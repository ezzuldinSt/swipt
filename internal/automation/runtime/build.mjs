import { execFileSync } from "node:child_process";
import fs from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";
import { build, transform } from "esbuild";
import { parse } from "meriyah";
import { hiddenSlots } from "./slots.mjs";

const shared = {
  bundle: true,
  format: "iife",
  target: "es2022",
  platform: "neutral",
  mainFields: ["module", "main"],
  minify: true,
  keepNames: true,
  legalComments: "inline",
};

const chunks = Object.fromEntries(
  /^export interface Chunks \{\n(.*?)\n\}$/ms
    .exec(fs.readFileSync("src/extension/chunks.ts", "utf8"))[1]
    .split("\n")
    .map((line) => {
      const [, name, module] =
        /^ {2}"?([\w-]+)"?: typeof import\("(.+)"\);$/.exec(line) ?? [];
      if (name === undefined) {
        throw new Error(
          `the Chunks member ${JSON.stringify(line.trim())} is not NAME: typeof import("MODULE")`,
        );
      }
      return [name, path.posix.join("src/extension", `${module}.ts`)];
    }),
);

const registry = (ids) => ({
  name: "registry",
  setup(build) {
    build.onResolve({ filter: /^swipt:modules$/ }, () => ({
      path: "modules",
      namespace: "swipt",
    }));
    build.onLoad({ filter: /.*/, namespace: "swipt" }, () => ({
      contents:
        `${ids.map((id, index) => `import * as m${index} from ${JSON.stringify(`./${id}`)};\n`).join("")}` +
        `export const modules = Object.freeze({ ${ids.map((id, index) => `${JSON.stringify(id)}: m${index}`).join(", ")} });\n`,
      resolveDir: process.cwd(),
      loader: "js",
    }));
  },
});

const internalSlots = {
  name: "internal-slots",
  setup(build) {
    build.onLoad(
      { filter: /[\\/]web-streams-polyfill[\\/]dist[\\/]ponyfill\.mjs$/ },
      (args) => ({
        contents: hiddenSlots(fs.readFileSync(args.path, "utf8")),
        loader: "js",
      }),
    );
  },
};

const runtime = {
  ...shared,
  format: "esm",
  entryPoints: ["src/extension/index.ts"],
  inject: ["src/extension/encoding.ts"],
  write: false,
  metafile: true,
};
const core = new Set(
  Object.keys(
    (
      await build({
        ...runtime,
        write: false,
        plugins: [internalSlots, registry([])],
      })
    ).metafile.inputs,
  ),
);

const imported = new Set();
const linked = {
  name: "linked",
  setup(build) {
    build.onResolve({ filter: /.*/ }, async (args) => {
      if (args.kind === "entry-point" || args.pluginData === linked) {
        return undefined;
      }
      const resolved = await build.resolve(args.path, {
        kind: args.kind,
        importer: args.importer,
        resolveDir: args.resolveDir,
        pluginData: linked,
      });
      // esbuild names its inputs with forward slashes on Windows too.
      const id = path
        .relative(process.cwd(), resolved.path)
        .split(path.sep)
        .join("/");
      if (resolved.errors.length > 0 || !core.has(id)) {
        return undefined;
      }
      imported.add(id);
      return { path: id, namespace: "core" };
    });
    build.onLoad({ filter: /.*/, namespace: "core" }, (args) => ({
      contents: `module.exports = __swiptCore[${JSON.stringify(args.path)}];`,
      loader: "js",
    }));
  },
};

const owners = new Map();
fs.rmSync("../chunks", { recursive: true, force: true });
fs.mkdirSync("../chunks");
for (const [name, entry] of Object.entries(chunks)) {
  if (core.has(entry)) {
    throw new Error(
      `runtime.js imports ${entry}, so it cannot be the chunk ${name}`,
    );
  }
  const result = await build({
    ...shared,
    globalName: "__swiptChunk",
    entryPoints: [entry],
    write: false,
    metafile: true,
    plugins: [linked],
  });
  for (const input of Object.keys(result.metafile.inputs).filter(
    (input) => !input.startsWith("core:"),
  )) {
    if (owners.has(input)) {
      throw new Error(
        `${input} is in both the chunks ${owners.get(input)} and ${name}`,
      );
    }
    owners.set(input, name);
  }
  const factory = /^(?:"use strict";)?var __swiptChunk=(.*?);?\n?$/s.exec(
    result.outputFiles[0].text,
  );
  if (factory === null) {
    throw new Error(
      `the chunk ${name} is not a single assignment to __swiptChunk`,
    );
  }
  fs.writeFileSync(
    `../chunks/${name}.js`,
    `(__swiptCore=>{"use strict";return ${factory[1]}})\n`,
  );
}

const blockScript = (code) => {
  const program = parse(code, { module: true, next: true, ranges: true });
  const exported = program.body.at(-1);
  if (
    exported?.type !== "ExportNamedDeclaration" ||
    exported.declaration !== null ||
    exported.source !== null ||
    program.body
      .slice(0, -1)
      .some(
        (statement) =>
          statement.type === "ImportDeclaration" ||
          statement.type.startsWith("Export"),
      )
  ) {
    throw new Error("runtime.js does not end in its only export { ... }");
  }
  const members = exported.specifiers.map(
    (specifier) =>
      `${JSON.stringify(specifier.exported.name ?? specifier.exported.value)}: ${specifier.local.name}`,
  );
  let out = `${code.slice(0, exported.start)}({ ${members.join(", ")} });\n}\n`;
  for (const statement of program.body
    .filter(
      (statement) =>
        statement.type === "VariableDeclaration" && statement.kind === "var",
    )
    .reverse()) {
    out = `${out.slice(0, statement.start)}let${out.slice(statement.start + "var".length)}`;
  }
  out = `"use strict";\n{\n${out}`;
  parse(out, { next: true });
  return out;
};
const bundled = await build({
  ...runtime,
  plugins: [internalSlots, registry([...imported].sort())],
});
fs.writeFileSync("../runtime.js", blockScript(bundled.outputFiles[0].text));
const rewriter = await build({
  ...shared,
  entryPoints: ["src/extension/rewrite.ts"],
  globalName: "__swiptRewrite",
  write: false,
});
fs.writeFileSync("../rewrite.js", rewriter.outputFiles[0].text);

const read = (name) =>
  fs.readFileSync(
    new URL(`../../../api/${name}.d.ts`, import.meta.url),
    "utf8",
  );
const globalBlock = /^declare global \{\n([\s\S]*?)^\}\n/gm;
const standardSchema = fs
  .readFileSync(
    fileURLToPath(import.meta.resolve("@standard-schema/spec")).replace(
      /\.js$/,
      ".d.ts",
    ),
    "utf8",
  )
  .replace(/^[ \t]*\/\*[\s\S]*?\*\/\n/gm, "")
  .replace(/^export \{.*\};\n?/m, "")
  .replace(/^(?: {4})+/gm, (indent) => indent.slice(indent.length / 2))
  .replace(/^\s*\n/gm, "")
  .trimEnd();

execFileSync("bun", ["x", "tsc", "-p", "."], { stdio: "inherit" });

const table = parse(
  (
    await transform(fs.readFileSync("src/extension/extension.ts", "utf8"), {
      loader: "ts",
    })
  ).code,
  { module: true },
)
  .body.flatMap((node) =>
    node.type === "VariableDeclaration" ? node.declarations : [],
  )
  .find((declarator) => declarator.id.name === "moduleTable");
if (table === undefined) {
  throw new Error("src/extension/extension.ts has no module table moduleTable");
}
const typeFiles = new Set(
  fs
    .readdirSync("src/extension/types", { recursive: true })
    .filter((file) => file.endsWith(".d.ts"))
    .map((file) => file.split(path.sep).join("/").slice(0, -".d.ts".length)),
);
const ambientModule = (specifier, text) => {
  const body = text
    .replace(
      /^import type \{[^}]*\} from "@standard-schema\/spec";$/m,
      standardSchema,
    )
    .replace(
      /^import (type )?(.*) from "\.\/([a-z-]+)";$/gm,
      (_, type = "", names, module) =>
        `import ${type}${names} from "${module === "index" ? "@swipt/api" : `@swipt/api/${module}`}";`,
    )
    .replace(globalBlock, (global) => global.replace(/^declare /, ""))
    .replace(/^(export )?declare /gm, "$1")
    .replace(/\n{3,}/g, "\n\n");
  return `declare module "${specifier}" {\n${body.trim().replace(/^(?=.)/gm, "  ")}\n}`;
};
const modules = table.init.properties.map(({ key }) => {
  const specifier = key.value ?? key.name;
  if (specifier === "@swipt/api" || specifier.startsWith("@swipt/api/")) {
    return ambientModule(
      specifier,
      read(
        specifier === "@swipt/api"
          ? "index"
          : specifier.slice("@swipt/api/".length),
      ),
    );
  }
  if (!typeFiles.delete(specifier)) {
    throw new Error(`src/extension/types has no declarations of ${specifier}`);
  }
  return ambientModule(
    specifier,
    fs.readFileSync(`src/extension/types/${specifier}.d.ts`, "utf8"),
  );
});
if (typeFiles.size > 0) {
  throw new Error(
    `src/extension/types declares ${[...typeFiles].join(", ")}, which the module table lacks`,
  );
}
const repl = [...read("repl").matchAll(globalBlock)]
  .map(([, body]) =>
    body.replace(/^ {2}(?=\S)/gm, "declare ").replace(/^ {2}/gm, ""),
  )
  .join("")
  .trimEnd();
const web = fs.readFileSync("src/extension/web.d.ts", "utf8").trimEnd();
const declarations = fileURLToPath(
  new URL("../declarations/api.d.ts", import.meta.url),
);
fs.writeFileSync(declarations, `${[repl, ...modules, web].join("\n\n")}\n`);
for (const lib of ["es2023", "es2023,dom"]) {
  execFileSync(
    "bun",
    [
      "x",
      "tsc",
      "--ignoreConfig",
      "--noEmit",
      "--strict",
      "--noResolve",
      "--types",
      "",
      "--lib",
      lib,
      declarations,
    ],
    { stdio: "inherit" },
  );
}
