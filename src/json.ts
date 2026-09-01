// Strict ordered JSON parsing shared by the payload and config boundaries.
//
// JSON.parse cannot reject duplicate object keys, cannot preserve key order
// for the config rewrite, and cannot hand back raw number literals for the
// frozen numeric coercions — all three are load-bearing contract behaviors
// inherited from the Go engine. This parser produces an ordered document
// model: objects keep their pair order, numbers keep their literal text, and
// duplicate keys, trailing garbage, and malformed escapes reject the whole
// document.
//
// Unpaired \uXXXX surrogate escapes decode to U+FFFD, matching Go's
// encoding/json, so identity and digest derivation agree with the corpus the
// Go engine wrote.

export type JsonValue =
  | { readonly kind: "null" }
  | { readonly kind: "bool"; readonly value: boolean }
  | { readonly kind: "string"; readonly value: string }
  | { readonly kind: "number"; readonly literal: string }
  | { readonly kind: "object"; readonly entries: JsonEntry[] }
  | { readonly kind: "array"; readonly items: JsonValue[] };

export interface JsonEntry {
  readonly key: string;
  value: JsonValue;
}

export function objectGet(entries: readonly JsonEntry[], key: string): JsonValue | undefined {
  for (const e of entries) {
    if (e.key === key) return e.value;
  }
  return undefined;
}

export function objectHas(entries: readonly JsonEntry[], key: string): boolean {
  return objectGet(entries, key) !== undefined;
}

// objectSet replaces in place when the key exists and appends otherwise.
// Replace-in-place keeps an existing key from migrating to the end of the
// config file on every rewrite; the frozen byte order depends on it.
export function objectSet(entries: JsonEntry[], key: string, value: JsonValue): void {
  for (const e of entries) {
    if (e.key === key) {
      e.value = value;
      return;
    }
  }
  entries.push({ key, value });
}

export function objectRemove(entries: JsonEntry[], key: string): void {
  const i = entries.findIndex((e) => e.key === key);
  if (i >= 0) entries.splice(i, 1);
}

// parseOrderedJson decodes one complete JSON document, rejecting duplicate
// keys and trailing garbage. Returns null on any malformation.
export function parseOrderedJson(text: string): JsonValue | null {
  const p = new Parser(text);
  const v = p.parseValue();
  if (v === null) return null;
  p.skipWs();
  if (p.pos !== text.length) return null;
  return v;
}

class Parser {
  pos = 0;
  private readonly text: string;
  constructor(text: string) {
    this.text = text;
  }

  skipWs(): void {
    while (this.pos < this.text.length) {
      const c = this.text[this.pos];
      if (c === " " || c === "\t" || c === "\n" || c === "\r") this.pos++;
      else break;
    }
  }

  parseValue(): JsonValue | null {
    this.skipWs();
    if (this.pos >= this.text.length) return null;
    const c = this.text[this.pos];
    switch (c) {
      case "{":
        return this.parseObject();
      case "[":
        return this.parseArray();
      case '"': {
        const s = this.parseString();
        return s === null ? null : { kind: "string", value: s };
      }
      case "t":
        return this.parseLiteral("true") ? { kind: "bool", value: true } : null;
      case "f":
        return this.parseLiteral("false") ? { kind: "bool", value: false } : null;
      case "n":
        return this.parseLiteral("null") ? { kind: "null" } : null;
      default:
        return this.parseNumber();
    }
  }

  parseLiteral(word: string): boolean {
    if (this.text.startsWith(word, this.pos)) {
      this.pos += word.length;
      return true;
    }
    return false;
  }

  parseObject(): JsonValue | null {
    this.pos++;
    const entries: JsonEntry[] = [];
    const seen = new Set<string>();
    this.skipWs();
    if (this.text[this.pos] === "}") {
      this.pos++;
      return { kind: "object", entries };
    }
    for (;;) {
      this.skipWs();
      if (this.text[this.pos] !== '"') return null;
      const key = this.parseString();
      if (key === null) return null;
      if (seen.has(key)) return null;
      seen.add(key);
      this.skipWs();
      if (this.text[this.pos] !== ":") return null;
      this.pos++;
      const value = this.parseValue();
      if (value === null) return null;
      entries.push({ key, value });
      this.skipWs();
      const c = this.text[this.pos];
      if (c === ",") {
        this.pos++;
        continue;
      }
      if (c === "}") {
        this.pos++;
        return { kind: "object", entries };
      }
      return null;
    }
  }

  parseArray(): JsonValue | null {
    this.pos++;
    const items: JsonValue[] = [];
    this.skipWs();
    if (this.text[this.pos] === "]") {
      this.pos++;
      return { kind: "array", items };
    }
    for (;;) {
      const value = this.parseValue();
      if (value === null) return null;
      items.push(value);
      this.skipWs();
      const c = this.text[this.pos];
      if (c === ",") {
        this.pos++;
        continue;
      }
      if (c === "]") {
        this.pos++;
        return { kind: "array", items };
      }
      return null;
    }
  }

  parseString(): string | null {
    this.pos++;
    let out = "";
    for (;;) {
      if (this.pos >= this.text.length) return null;
      const c = this.text[this.pos];
      const code = this.text.charCodeAt(this.pos);
      if (c === '"') {
        this.pos++;
        return out;
      }
      if (code < 0x20) return null;
      if (c !== "\\") {
        out += c;
        this.pos++;
        continue;
      }
      this.pos++;
      if (this.pos >= this.text.length) return null;
      const esc = this.text[this.pos];
      this.pos++;
      switch (esc) {
        case '"':
          out += '"';
          break;
        case "\\":
          out += "\\";
          break;
        case "/":
          out += "/";
          break;
        case "b":
          out += "\b";
          break;
        case "f":
          out += "\f";
          break;
        case "n":
          out += "\n";
          break;
        case "r":
          out += "\r";
          break;
        case "t":
          out += "\t";
          break;
        case "u": {
          const first = this.parseHex4();
          if (first < 0) return null;
          if (first >= 0xd800 && first <= 0xdbff) {
            // High surrogate: pairs with a following \uXXXX low surrogate,
            // otherwise decodes to U+FFFD like Go's encoding/json.
            if (this.text.startsWith("\\u", this.pos)) {
              const save = this.pos;
              this.pos += 2;
              const second = this.parseHex4();
              if (second >= 0xdc00 && second <= 0xdfff) {
                out += String.fromCodePoint(0x10000 + (first - 0xd800) * 0x400 + (second - 0xdc00));
                break;
              }
              this.pos = save;
            }
            out += "�";
            break;
          }
          if (first >= 0xdc00 && first <= 0xdfff) {
            out += "�";
            break;
          }
          out += String.fromCharCode(first);
          break;
        }
        default:
          return null;
      }
    }
  }

  parseHex4(): number {
    if (this.pos + 4 > this.text.length) return -1;
    let n = 0;
    for (let i = 0; i < 4; i++) {
      const c = this.text.charCodeAt(this.pos + i);
      let d: number;
      if (c >= 0x30 && c <= 0x39) d = c - 0x30;
      else if (c >= 0x61 && c <= 0x66) d = c - 0x61 + 10;
      else if (c >= 0x41 && c <= 0x46) d = c - 0x41 + 10;
      else return -1;
      n = n * 16 + d;
    }
    this.pos += 4;
    return n;
  }

  parseNumber(): JsonValue | null {
    const start = this.pos;
    if (this.text[this.pos] === "-") this.pos++;
    if (this.text[this.pos] === "0") {
      this.pos++;
    } else if (this.text[this.pos] >= "1" && this.text[this.pos] <= "9") {
      while (this.text[this.pos] >= "0" && this.text[this.pos] <= "9") this.pos++;
    } else {
      return null;
    }
    if (this.text[this.pos] === ".") {
      this.pos++;
      if (!(this.text[this.pos] >= "0" && this.text[this.pos] <= "9")) return null;
      while (this.text[this.pos] >= "0" && this.text[this.pos] <= "9") this.pos++;
    }
    if (this.text[this.pos] === "e" || this.text[this.pos] === "E") {
      this.pos++;
      if (this.text[this.pos] === "+" || this.text[this.pos] === "-") this.pos++;
      if (!(this.text[this.pos] >= "0" && this.text[this.pos] <= "9")) return null;
      while (this.text[this.pos] >= "0" && this.text[this.pos] <= "9") this.pos++;
    }
    return { kind: "number", literal: this.text.slice(start, this.pos) };
  }
}
