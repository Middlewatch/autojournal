#!/usr/bin/env python3
"""Retrieval quality harness: run a judged query set against an
autojournal binary and report ranking metrics.

The query set is a private JSONL file (it quotes journal content), one
record per query: {"id", "query", "type", "relevant": [episode ids]}.
Types: known_item / plural_probe / paraphrase count toward ranking
metrics; negative queries (empty relevant) report score/confidence of
their best spurious hit instead. No judged set ships with this repository,
so results from it are not reproducible by a reader; bring your own.

Metrics over scored types: MRR@10 (reciprocal rank of the first relevant
hit), recall@1/@10 (any relevant hit in the top 1/10), and page
episode-diversity (distinct episodes in the top 10). Ranks count result
rows, matching what a reader scans. Use --json for machine-readable
output; compare runs with different --binary/--credit-mode/env.
"""
import argparse
import json
import os
import statistics
import subprocess
import sys
import tempfile

# A record {"meta": true, "frozen_at": "<ISO>"} freezes the eval corpus:
# hits from episodes at or after that event time are ignored entirely.
# This keeps the set stable as capture continues — and keeps sessions that
# *work on* the eval (whose own turns get captured, quoting the queries)
# from contaminating results.


def run_query(binary, query, credit_mode, extra_args):
    cmd = [binary, "search"] + query.split() + ["--json", "--limit", "100"]
    if credit_mode:
        cmd += ["--credit-mode", credit_mode]
    cmd += extra_args
    proc = subprocess.run(cmd, capture_output=True, text=True)
    if proc.returncode not in (0, 1):  # 1 = typed no_match
        raise RuntimeError(f"search failed ({proc.returncode}): {proc.stderr}")
    return json.loads(proc.stdout)


def ranking_block(query, out):
    """One --ranking block: the ordered hit list exactly as search returned
    it, plus the term-provenance fields the phase gates classify moves by.

    A query has MOVED between two runs when the ordered (episode_id, line)
    sequence in `hits` differs; the other fields are context for
    classifying a move, never themselves a move. Fields an older binary
    does not emit normalize to their empty values, so artifacts from
    different binaries differ only where behavior differs. Emitted for
    every record including negatives: a scorer change can reorder a
    negative query's spurious hits too.

    Deliberately unfiltered: the metrics mode freezes the eval corpus at
    the query set's frozen_at so judged relevance stays meaningful, but a
    parity block that dropped post-freeze hits would observe less than
    search returned and could only MASK movement, never reveal it.
    """
    hits = out.get("results") or []
    return {
        "query": query,
        "outcome": out.get("outcome") or "",
        "detail": out.get("detail") or "",
        "query_terms": out.get("query_terms") or [],
        "alias_terms": out.get("alias_terms") or [],
        "folded_terms": out.get("folded_terms") or [],
        "hits": [{
            "episode_id": h.get("episode_id") or "",
            "revision": h.get("revision") or "",
            "line": h.get("line") or 0,
            "score": h.get("score") or 0.0,
        } for h in hits],
    }


def self_test(binary):
    """The harness is load-bearing for the phase gates, so it carries its
    own test: a two-episode corpus in a temporary root, two queries, and
    the two properties a parity diff depends on — reordering hits changes
    the ranking block, a snippet-only change does not."""
    with tempfile.TemporaryDirectory() as tmp:
        env = dict(os.environ)
        env["AUTOJOURNAL_THESAURUS"] = os.path.join(tmp, "thesaurus.json")
        env["AUTOJOURNAL_MISS_LOG"] = os.path.join(tmp, "misses.jsonl")
        root, index = os.path.join(tmp, "root"), os.path.join(tmp, "index.v2.json")
        for turn, user, result in [
            ("t1", "the quokka fence was mended", "Mended."),
            ("t2", "the quokka ramp by the fence latch", "Noted."),
        ]:
            payload = json.dumps({
                "schema_version": 1, "world": "selftest", "scope": "global",
                "lane": "conversation", "harness": "eval-selftest",
                "adapter_version": "0.0.0", "session_id": "s1",
                "turn_id": turn, "event_time_ms": 1785240000000,
                "capture_policy": "default-v1", "turn_outcome": "completed",
                "user_content": user, "assistant_result": result,
            })
            proc = subprocess.run(
                [binary, "capture", "--root", root, "--index", index],
                input=payload, capture_output=True, text=True, env=env)
            if '"outcome":"published"' not in proc.stdout:
                raise RuntimeError(f"self-test capture failed: {proc.stdout} {proc.stderr}")

        rest = ["--root", root, "--index", index, "--world", "selftest"]
        out = run_query(binary, "quokka fence", None, rest)
        block = ranking_block("quokka fence", out)
        if len(block["hits"]) != 2:
            raise RuntimeError(f"self-test wants 2 hits, got {block['hits']}")

        swapped = json.loads(json.dumps(out))
        swapped["results"] = list(reversed(swapped["results"]))
        if ranking_block("quokka fence", swapped) == block:
            raise RuntimeError("self-test: reordered hits did not change the ranking block")

        snippet_only = json.loads(json.dumps(out))
        snippet_only["results"][0]["snippet"] = "a different rendering"
        if ranking_block("quokka fence", snippet_only) != block:
            raise RuntimeError("self-test: a snippet-only change moved the ranking block")

        negative = run_query(binary, "zzyzxplugh", None, rest)
        nblock = ranking_block("zzyzxplugh", negative)
        if nblock["outcome"] != "no_match" or nblock["hits"]:
            raise RuntimeError(f"self-test negative block wrong: {nblock}")
    print("retrieval-eval self-test: PASS")


def main():
    ap = argparse.ArgumentParser(description=__doc__.split("\n")[0])
    ap.add_argument("--binary", default="autojournal")
    ap.add_argument("--queries", help="judged JSONL query set")
    ap.add_argument("--credit-mode", default=None)
    ap.add_argument("--label", default="run")
    ap.add_argument("--json", action="store_true")
    ap.add_argument("--ranking", action="store_true",
                    help="emit one ranking block per query (JSONL, every "
                         "record including negatives, no label key) and "
                         "exit; parity between runs is a diff of this output")
    ap.add_argument("--self-test", action="store_true",
                    help="exercise the ranking-block properties against a "
                         "temporary two-episode corpus and exit")
    ap.add_argument("rest", nargs="*", help="extra args after --, passed to search")
    args = ap.parse_args()

    if args.self_test:
        self_test(args.binary)
        return
    if not args.queries:
        ap.error("--queries is required outside --self-test")

    records = [json.loads(line) for line in open(args.queries)
               if line.strip()]
    frozen_at = None
    queries = []
    for rec in records:
        if rec.get("meta"):
            frozen_at = rec.get("frozen_at")
        else:
            queries.append(rec)

    if args.ranking:
        for q in queries:
            out = run_query(args.binary, q["query"], args.credit_mode, args.rest)
            print(json.dumps(ranking_block(q["query"], out)))
        return

    per_query, negatives = [], []
    for q in queries:
        out = run_query(args.binary, q["query"], args.credit_mode, args.rest)
        hits = out.get("results") or []
        if frozen_at:
            hits = [h for h in hits if h.get("event_time", "") < frozen_at]
        if q["type"] == "negative":
            top = hits[0] if hits else None
            negatives.append({
                "id": q["id"],
                "best_score": top["score"] if top else 0.0,
                "confidence": top["confidence"] if top else None,
                "total": out.get("total", 0),
            })
            continue
        relevant = set(q["relevant"])
        rank = next((i + 1 for i, h in enumerate(hits)
                     if h["episode_id"] in relevant), None)
        top10 = hits[:10]
        per_query.append({
            "id": q["id"], "type": q["type"], "rank": rank,
            "total": out.get("total", 0),
            "top_score": top10[0]["score"] if top10 else 0.0,
            "distinct_top10": len({h["episode_id"] for h in top10}),
            "relevant_in_top10": sum(
                1 for h in top10 if h["episode_id"] in relevant),
        })

    scored = [q for q in per_query if q["type"] in
              ("known_item", "plural_probe", "paraphrase")]
    mrr = statistics.mean(
        (1.0 / q["rank"]) if q["rank"] and q["rank"] <= 10 else 0.0
        for q in scored)
    recall1 = statistics.mean(
        1.0 if q["rank"] == 1 else 0.0 for q in scored)
    recall10 = statistics.mean(
        1.0 if q["rank"] and q["rank"] <= 10 else 0.0 for q in scored)
    diversity = statistics.mean(q["distinct_top10"] for q in scored)

    summary = {
        "label": args.label,
        "queries_scored": len(scored),
        "mrr_at_10": round(mrr, 4),
        "recall_at_1": round(recall1, 4),
        "recall_at_10": round(recall10, 4),
        "mean_distinct_episodes_top10": round(diversity, 2),
        "per_query": per_query,
        "negatives": negatives,
    }
    if args.json:
        json.dump(summary, sys.stdout, indent=1)
        print()
        return
    print(f"[{args.label}] {len(scored)} scored queries: "
          f"MRR@10={mrr:.3f}  recall@1={recall1:.3f}  "
          f"recall@10={recall10:.3f}  distinct-episodes@10={diversity:.2f}")
    for q in per_query:
        rank = q["rank"] if q["rank"] else "-"
        print(f"  {q['id']:22} rank={rank:>3}  total={q['total']:>5}  "
              f"distinct@10={q['distinct_top10']:>2}  "
              f"rel@10={q['relevant_in_top10']}  [{q['type']}]")
    for n in negatives:
        print(f"  {n['id']:22} spurious best={n['best_score']:.2f} "
              f"({n['confidence']}) total={n['total']}")


if __name__ == "__main__":
    main()
