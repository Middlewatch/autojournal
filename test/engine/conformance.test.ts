// Shared edge-case corpus for the payload contract's token,
// workspace-root and origin-host rules, inherited from the multi-adapter
// era as fixtures for the one engine. Values are data, never rendered on
// failure: assertions report rule and case id only.

import { test } from "node:test";
import assert from "node:assert/strict";
import * as fs from "node:fs";
import * as path from "node:path";

import { validToken, validPath } from "../../src/contracts.ts";
import { sanitizeToken, originHost } from "../../index.ts";
import { REPO_ROOT } from "./helpers.ts";

interface Case {
  id: string;
  value: string;
  expect: "accept" | "reject";
}

const cases: Record<string, { cases: Case[] }> = JSON.parse(
  fs.readFileSync(path.join(REPO_ROOT, "testdata", "conformance_cases.json"), "utf8"),
).rules;

// Each rule is pinned to the functions that carry it, so a rule silently
// dropping out of the engine fails loudly here.
const decisions: Record<string, (value: string) => boolean> = {
  token: (value) => validToken(value),
  workspace_root: (value) => validPath(value),
  origin_host: (value) => originHost(value) !== null,
};

// The adapter's outbound sanitizer must agree with the engine's token
// contract: a value it passes through unchanged is a value the engine
// accepts.
const adapterDecisions: Record<string, ((value: string) => boolean) | undefined> = {
  token: (value) => sanitizeToken(value, "!fallback!") === value,
};

test("conformance cases", async (t) => {
  for (const [rule, { cases: ruleCases }] of Object.entries(cases)) {
    const decide = decisions[rule];
    assert.ok(decide, `no engine decision wired for rule ${rule}`);
    await t.test(rule, () => {
      for (const c of ruleCases) {
        assert.equal(decide(c.value), c.expect === "accept", `${rule}/${c.id}`);
        const adapterDecide = adapterDecisions[rule];
        if (adapterDecide !== undefined) {
          assert.equal(adapterDecide(c.value), c.expect === "accept", `${rule}/${c.id} (adapter)`);
        }
      }
    });
  }
});
