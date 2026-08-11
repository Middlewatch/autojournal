#!/usr/bin/env python3
"""Cross-adapter conformance for the payload contract's field rules.

Three adapters implement the contract's token, workspace-root and
origin-host rules: the Claude Code and Codex hooks in Python, and the Pi
adapter in TypeScript. `adapters/conformance_cases.json` is the shared
edge-case corpus; this runner feeds every case to every implementation of
its rule and asserts the same accept/reject decision everywhere, so the
adapters cannot quietly desynchronize.

Not every adapter carries every rule, and the corpus pins who carries what
(re-pinned below, so shrinking the list means editing two files on
purpose): the Pi payload sends no workspace_root, and the Codex hook's one
use of the token class is its origin-host field, which the origin-host
cases exercise across all three.

The Pi implementation runs through `node --experimental-strip-types`
against a small generated harness importing `adapters/pi/index.ts`
directly — there is no compiled output to import, and the adapter's own
test script runs TypeScript through the same flag. That needs
`adapters/pi/node_modules` (index.ts imports typebox at runtime) and
Node >= 22.6, which is why verify.sh runs this suite after its adapter
gate. Failure messages name the rule, case id and implementation only;
case values are data and are never echoed.

Run directly (`python3 adapters/test_conformance.py`) or through
scripts/verify.sh. Standard library only.
"""

import importlib.util
import json
import pathlib
import subprocess
import sys
import tempfile
import unittest
import unittest.mock

ADAPTERS = pathlib.Path(__file__).resolve().parent

# Who implements which rule. conformance_cases.json states the same lists;
# the test asserts they agree before judging any case.
RULE_IMPLEMENTATIONS = {
    "token": ["claude-code", "pi"],
    "workspace_root": ["claude-code", "codex"],
    "origin_host": ["claude-code", "codex", "pi"],
}

# Never a case value: handed to sanitizeToken so an input the Pi adapter
# would mangle or replace comes back different from what went in.
PI_FALLBACK = "conformance-fallback"

PI_HARNESS = """\
import { sanitizeToken, originHost } from %(index_url)s;
const chunks = [];
process.stdin.on("data", (c) => chunks.push(c));
process.stdin.on("end", () => {
  const cases = JSON.parse(Buffer.concat(chunks).toString("utf8"));
  const out = { token: {}, origin_host: {} };
  for (const c of cases.token) {
    out.token[c.id] = sanitizeToken(c.value, %(fallback)s) === c.value;
  }
  for (const c of cases.origin_host) {
    out.origin_host[c.id] = originHost(c.value) !== null;
  }
  process.stdout.write(JSON.stringify(out));
});
"""


def load(name: str, relative: str):
    spec = importlib.util.spec_from_file_location(name, ADAPTERS / relative)
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


cc = load("aj_cc_hook_conf", "claude-code/autojournal_capture.py")
codex = load("aj_codex_hook_conf", "codex/autojournal_capture.py")


def pi_decisions(rules: dict) -> dict:
    """Run the Pi implementation once over its two rule categories."""
    index_url = (ADAPTERS / "pi" / "index.ts").as_uri()
    source = PI_HARNESS % {
        "index_url": json.dumps(index_url),
        "fallback": json.dumps(PI_FALLBACK),
    }
    payload = json.dumps(
        {
            "token": rules["token"]["cases"],
            "origin_host": rules["origin_host"]["cases"],
        }
    )
    with tempfile.NamedTemporaryFile("w", suffix=".mts", delete=False) as fh:
        fh.write(source)
        harness = fh.name
    try:
        run = subprocess.run(
            ["node", "--experimental-strip-types", harness],
            input=payload.encode(),
            capture_output=True,
            timeout=60,
        )
    finally:
        pathlib.Path(harness).unlink(missing_ok=True)
    if run.returncode != 0:
        raise RuntimeError(
            f"pi harness exited {run.returncode}: {run.stderr.decode(errors='replace')}"
        )
    return json.loads(run.stdout.decode())


def python_host_accepts(module, value: str) -> bool:
    """The hooks read the hostname from the OS, so the case value arrives
    through a patched gethostname; accept means a host label was sent."""
    with unittest.mock.patch.object(
        module.socket, "gethostname", return_value=value
    ):
        return module.origin_host() is not None


class ConformanceAcrossAdapters(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.rules = json.loads(
            (ADAPTERS / "conformance_cases.json").read_text(encoding="utf-8")
        )["rules"]
        cls.pi = pi_decisions(cls.rules)

    def decision(self, rule: str, impl: str, case: dict) -> bool:
        value = case["value"]
        if rule == "token":
            if impl == "claude-code":
                return cc.valid_token(value)
            return self.pi["token"][case["id"]]
        if rule == "workspace_root":
            module = cc if impl == "claude-code" else codex
            return module.workspace_root(value) is not None
        if rule == "origin_host":
            if impl == "pi":
                return self.pi["origin_host"][case["id"]]
            return python_host_accepts(cc if impl == "claude-code" else codex, value)
        raise AssertionError(f"unknown rule {rule!r}")

    def test_conformance_all_adapters_agree(self):
        self.assertEqual(
            {name: rule["implementations"] for name, rule in self.rules.items()},
            RULE_IMPLEMENTATIONS,
            "the corpus and this test disagree about who implements what",
        )
        judged = 0
        for name, rule in self.rules.items():
            for case in rule["cases"]:
                want = case["expect"] == "accept"
                for impl in rule["implementations"]:
                    got = self.decision(name, impl, case)
                    self.assertEqual(
                        got,
                        want,
                        f"{name}/{case['id']}: {impl} "
                        f"{'accepted' if got else 'rejected'}, "
                        f"expected {case['expect']}",
                    )
                    judged += 1
        self.assertGreaterEqual(judged, 100, "conformance corpus shrank")


if __name__ == "__main__":
    unittest.main(verbosity=2)
