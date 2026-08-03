#!/usr/bin/env python3
"""Retrieval quality harness: run a judged query set against an
autojournal binary and report ranking metrics.

The query set is a private JSONL file (it quotes journal content), one
record per query: {"id", "query", "type", "relevant": [episode ids]}.
Types: known_item / plural_probe / paraphrase count toward ranking
metrics; negative queries (empty relevant) report score/confidence of
their best spurious hit instead. See ~/memory/eval/autojournal-retrieval/
on the origin host.

Metrics over scored types: MRR@10 (reciprocal rank of the first relevant
hit), recall@1/@10 (any relevant hit in the top 1/10), and page
episode-diversity (distinct episodes in the top 10). Ranks count result
rows, matching what a reader scans. Use --json for machine-readable
output; compare runs with different --binary/--credit-mode/env.
"""
import argparse
import json
import statistics
import subprocess
import sys


def run_query(binary, query, credit_mode, extra_args):
    cmd = [binary, "search"] + query.split() + ["--json", "--limit", "100"]
    if credit_mode:
        cmd += ["--credit-mode", credit_mode]
    cmd += extra_args
    proc = subprocess.run(cmd, capture_output=True, text=True)
    if proc.returncode not in (0, 1):  # 1 = typed no_match
        raise RuntimeError(f"search failed ({proc.returncode}): {proc.stderr}")
    return json.loads(proc.stdout)


def main():
    ap = argparse.ArgumentParser(description=__doc__.split("\n")[0])
    ap.add_argument("--binary", default="autojournal")
    ap.add_argument("--queries", required=True, help="judged JSONL query set")
    ap.add_argument("--credit-mode", default=None)
    ap.add_argument("--label", default="run")
    ap.add_argument("--json", action="store_true")
    ap.add_argument("rest", nargs="*", help="extra args after --, passed to search")
    args = ap.parse_args()

    queries = [json.loads(line) for line in open(args.queries)
               if line.strip()]

    per_query, negatives = [], []
    for q in queries:
        out = run_query(args.binary, q["query"], args.credit_mode, args.rest)
        hits = out.get("results") or []
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
              ("known_item", "plural_probe")]
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
