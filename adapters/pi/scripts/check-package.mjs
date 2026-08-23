import fs from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";

const root = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "..");

// The changelog and the notices file are authored once at the repository
// root and staged into the package here, because npm packs symlinks as
// nothing at all. Generated, not committed — same treatment as bin/.
fs.copyFileSync(
  path.resolve(root, "..", "..", "CHANGELOG.md"),
  path.join(root, "CHANGELOG.md"),
);
fs.copyFileSync(
  path.resolve(root, "..", "..", "NOTICES.md"),
  path.join(root, "NOTICES.md"),
);

const expected = [
  "linux-x64/autojournal",
  "linux-arm64/autojournal",
  "darwin-x64/autojournal",
  "darwin-arm64/autojournal",
];

for (const relative of expected) {
  const binary = path.join(root, "bin", relative);
  let stat;
  try {
    stat = fs.statSync(binary);
  } catch {
    throw new Error(`missing release binary: bin/${relative}`);
  }
  if (!stat.isFile()) throw new Error(`release binary is not a file: bin/${relative}`);
  if (process.platform !== "win32" && (stat.mode & 0o111) === 0) {
    throw new Error(`release binary is not executable: bin/${relative}`);
  }
}

for (const relative of ["index.ts", "README.md", "LICENSE", "CHANGELOG.md", "NOTICES.md"]) {
  if (!fs.existsSync(path.join(root, relative))) {
    throw new Error(`missing package file: ${relative}`);
  }
}

// One version, asserted in several places: package.json is the authority,
// and the adapter constant plus the packaged binary must agree with it.
const manifest = JSON.parse(fs.readFileSync(path.join(root, "package.json"), "utf8"));
const source = fs.readFileSync(path.join(root, "index.ts"), "utf8");
const adapterVersion = source.match(/^export const ADAPTER_VERSION = "([^"]+)";$/m)?.[1];
if (adapterVersion !== manifest.version) {
  throw new Error(
    `version drift: package.json ${manifest.version} vs ADAPTER_VERSION ${adapterVersion}`,
  );
}
// The changelog ships inside the package, so the entry for the version being
// packed has to name its release date before it leaves the machine. npm
// versions are immutable: an entry published as "unreleased" stays wrong
// forever, and correcting it costs an entire new version. That is exactly
// what happened to 1.0.2, and this gate is why it cannot happen again.
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

const { execFileSync } = await import("node:child_process");
const platformDir = `${process.platform}-${process.arch}`;
const nativeBinary = path.join(root, "bin", platformDir, "autojournal");
if (fs.existsSync(nativeBinary)) {
  const reported = execFileSync(nativeBinary, ["version"], { encoding: "utf8" });
  if (!reported.startsWith(`autojournal ${manifest.version} `)) {
    throw new Error(
      `version drift: package.json ${manifest.version} vs binary "${reported.trim()}"`,
    );
  }
}
