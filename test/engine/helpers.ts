// Shared fixture plumbing for the engine suites: repo-relative fixture
// paths, the Go fuzz-corpus file format (the pinned regression seeds were
// minted by `go test -fuzz` and stay in that encoding), and a deterministic
// PRNG for the property suites.

import * as fs from "node:fs";
import * as path from "node:path";
import { fileURLToPath } from "node:url";

export const REPO_ROOT = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "..", "..");
export const TESTDATA = path.join(REPO_ROOT, "testdata");
export const GOLDEN_DIR = path.join(TESTDATA, "golden");
export const PAYLOADS_DIR = path.join(TESTDATA, "payloads");
export const FUZZ_SEED_DIR = path.join(REPO_ROOT, "testdata", "fuzz");

export function readFixture(...parts: string[]): Buffer {
  return fs.readFileSync(path.join(TESTDATA, ...parts));
}

/**
 * Decodes one `go test fuzz v1` corpus file into the byte string its
 * `[]byte("...")` line carries. The quoting is Go's strconv syntax:
 * `\xNN` and octal escapes denote raw bytes; `\uNNNN`/`\UNNNNNNNN` and
 * literal characters denote UTF-8-encoded code points.
 */
export function decodeGoFuzzCorpus(file: string): Buffer {
  const text = fs.readFileSync(file, "utf8");
  const lines = text.split("\n");
  if (lines[0] !== "go test fuzz v1") throw new Error(`${file}: not a go fuzz corpus file`);
  const m = /^\[\]byte\("([\s\S]*)"\)$/.exec(lines.slice(1).join("\n").trimEnd());
  if (m === null) throw new Error(`${file}: no []byte line`);
  return decodeGoQuoted(m[1]);
}

function decodeGoQuoted(s: string): Buffer {
  const out: number[] = [];
  const pushCodePoint = (cp: number) => {
    for (const b of Buffer.from(String.fromCodePoint(cp), "utf8")) out.push(b);
  };
  for (let i = 0; i < s.length; ) {
    const c = s[i];
    if (c !== "\\") {
      const cp = s.codePointAt(i)!;
      pushCodePoint(cp);
      i += String.fromCodePoint(cp).length;
      continue;
    }
    const e = s[i + 1];
    i += 2;
    switch (e) {
      case "a":
        out.push(0x07);
        break;
      case "b":
        out.push(0x08);
        break;
      case "f":
        out.push(0x0c);
        break;
      case "n":
        out.push(0x0a);
        break;
      case "r":
        out.push(0x0d);
        break;
      case "t":
        out.push(0x09);
        break;
      case "v":
        out.push(0x0b);
        break;
      case "\\":
        out.push(0x5c);
        break;
      case '"':
        out.push(0x22);
        break;
      case "'":
        out.push(0x27);
        break;
      case "x":
        out.push(parseInt(s.slice(i, i + 2), 16));
        i += 2;
        break;
      case "u":
        pushCodePoint(parseInt(s.slice(i, i + 4), 16));
        i += 4;
        break;
      case "U":
        pushCodePoint(parseInt(s.slice(i, i + 8), 16));
        i += 8;
        break;
      default:
        if (e >= "0" && e <= "7") {
          out.push(parseInt(e + s.slice(i, i + 2), 8));
          i += 2;
          break;
        }
        throw new Error(`unknown escape \\${e}`);
    }
  }
  return Buffer.from(out);
}

/** Every seed for one fuzz target: named regression seeds plus a fixture glob. */
export function fuzzSeeds(target: string, fixtureDir: string, suffix: string): Buffer[] {
  const seeds: Buffer[] = [];
  const seedDir = path.join(FUZZ_SEED_DIR, target);
  if (fs.existsSync(seedDir)) {
    for (const name of fs.readdirSync(seedDir).sort()) {
      seeds.push(decodeGoFuzzCorpus(path.join(seedDir, name)));
    }
  }
  for (const name of fs.readdirSync(fixtureDir).sort()) {
    if (name.endsWith(suffix)) seeds.push(fs.readFileSync(path.join(fixtureDir, name)));
  }
  if (seeds.length === 0) throw new Error(`seed corpus missing for ${target}`);
  return seeds;
}

/** SplitMix64: deterministic, seedable, good-enough dispersion for mutation. */
export class Prng {
  private state: bigint;
  constructor(seed: bigint) {
    this.state = seed;
  }
  next(): bigint {
    this.state = (this.state + 0x9e3779b97f4a7c15n) & 0xffffffffffffffffn;
    let z = this.state;
    z = ((z ^ (z >> 30n)) * 0xbf58476d1ce4e5b9n) & 0xffffffffffffffffn;
    z = ((z ^ (z >> 27n)) * 0x94d049bb133111ebn) & 0xffffffffffffffffn;
    return z ^ (z >> 31n);
  }
  int(bound: number): number {
    return Number(this.next() % BigInt(bound));
  }
}

/**
 * One mutation step over a seed corpus: pick a seed, apply 1-4 random
 * byte-level edits (flip, insert, delete, splice from another seed).
 */
export function mutate(prng: Prng, seeds: Buffer[]): Buffer {
  const base = seeds[prng.int(seeds.length)];
  let bytes = Array.from(base);
  const edits = 1 + prng.int(4);
  for (let e = 0; e < edits; e++) {
    switch (prng.int(4)) {
      case 0: {
        if (bytes.length === 0) break;
        const i = prng.int(bytes.length);
        bytes[i] = prng.int(256);
        break;
      }
      case 1: {
        const i = prng.int(bytes.length + 1);
        bytes.splice(i, 0, prng.int(256));
        break;
      }
      case 2: {
        if (bytes.length === 0) break;
        bytes.splice(prng.int(bytes.length), 1 + prng.int(4));
        break;
      }
      case 3: {
        const other = seeds[prng.int(seeds.length)];
        if (other.length === 0) break;
        const from = prng.int(other.length);
        const len = 1 + prng.int(Math.min(16, other.length - from));
        const i = prng.int(bytes.length + 1);
        bytes = bytes.slice(0, i).concat(Array.from(other.subarray(from, from + len)), bytes.slice(i));
        break;
      }
    }
  }
  return Buffer.from(bytes);
}

/** Property iteration budget: bounded in the gate, raised by the long-run job. */
export function propertyIterations(defaultCount: number): number {
  const env = process.env.AUTOJOURNAL_PROPERTY_ITERS;
  if (env === undefined || env === "") return defaultCount;
  const n = Number(env);
  if (!Number.isInteger(n) || n <= 0) throw new Error(`bad AUTOJOURNAL_PROPERTY_ITERS: ${env}`);
  return n;
}

/** An Environ backed by a fixed map; keys absent from the map are unset. */
export function mapEnviron(pairs: Record<string, string>): (key: string) => string | undefined {
  return (key) => pairs[key];
}
