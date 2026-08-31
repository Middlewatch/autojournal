// Packaging self-check, run by npm prepack. The published package is this
// repository's root: the TypeScript engine, the extension entry, and the
// CLI, with no bundled binaries and no runtime dependencies.
import fs from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";

const root = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "..");

const requiredFiles = [
  "index.ts",
  "cli.ts",
  "bin/autojournal",
  "README.md",
  "LICENSE",
  "CHANGELOG.md",
  "NOTICES.md",
  "src/contracts.ts",
  "src/identity.ts",
  "src/render.ts",
  "src/episode.ts",
  "src/corpus.ts",
  "src/store.ts",
  "src/index.ts",
  "src/retrieval.ts",
  "src/search.ts",
  "src/aliases.ts",
  "src/ops.ts",
  "src/ops-alias.ts",
  "src/config.ts",
  "src/paths.ts",
  "src/json.ts",
];

for (const relative of requiredFiles) {
  if (!fs.existsSync(path.join(root, relative))) {
    throw new Error(`missing package file: ${relative}`);
  }
}

// With --pack-json <file> (a saved `npm pack --dry-run --json` listing),
// also assert the tarball layout itself: every required file ships, and
// no test, fixture, or workflow tree leaks into the package.
const packArg = process.argv.indexOf("--pack-json");
if (packArg !== -1) {
  const listing = JSON.parse(fs.readFileSync(process.argv[packArg + 1], "utf8"));
  const shipped = new Set(listing[0].files.map((f) => f.path));
  for (const relative of requiredFiles) {
    if (!shipped.has(relative)) throw new Error(`npm pack omits required file: ${relative}`);
  }
  const forbidden = [...shipped].filter((p) => /^(test|testdata|docs|artifacts|node_modules|\.github)\//.test(p));
  if (forbidden.length > 0) {
    throw new Error(`npm pack leaks non-package trees: ${forbidden.join(", ")}`);
  }
}

// One version, asserted in three places: package.json is the authority,
// and the extension constant plus the CLI constant must agree with it.
const manifest = JSON.parse(fs.readFileSync(path.join(root, "package.json"), "utf8"));
const extension = fs.readFileSync(path.join(root, "index.ts"), "utf8");
const adapterVersion = extension.match(/^export const ADAPTER_VERSION = "([^"]+)";$/m)?.[1];
if (adapterVersion !== manifest.version) {
  throw new Error(`version drift: package.json ${manifest.version} vs ADAPTER_VERSION ${adapterVersion}`);
}
const cli = fs.readFileSync(path.join(root, "cli.ts"), "utf8");
const cliVersion = cli.match(/^export const CLI_VERSION = "([^"]+)";$/m)?.[1];
if (cliVersion !== manifest.version) {
  throw new Error(`version drift: package.json ${manifest.version} vs CLI_VERSION ${cliVersion}`);
}

// The engine ships with zero runtime dependencies, and that is a product
// property rather than an accident.
if (manifest.dependencies !== undefined && Object.keys(manifest.dependencies).length > 0) {
  throw new Error(`the package must have no runtime dependencies; found: ${Object.keys(manifest.dependencies).join(", ")}`);
}

// The changelog ships inside the package, so the entry for the version
// being packed has to name its release date before it leaves the machine.
// npm versions are immutable: an entry published as "unreleased" stays
// wrong forever, and correcting it costs an entire new version.
const changelog = fs.readFileSync(path.join(root, "CHANGELOG.md"), "utf8");
const escaped = manifest.version.replace(/\./g, "\\.");
const entry = changelog.match(new RegExp(`^##\\s+${escaped}\\s*[—–-]\\s*(.+)$`, "m"));
if (!entry) {
  throw new Error(`CHANGELOG.md has no entry for ${manifest.version}`);
}
if (/unreleased|unpublished|tbd|pending/i.test(entry[1])) {
  throw new Error(
    `CHANGELOG.md still marks ${manifest.version} as "${entry[1].trim()}" — ` +
      `date the entry before publishing; npm cannot correct it afterward`,
  );
}
