"""Writes a Markdown table of results per test plan case.

Usage: python tools/summary.py results.json [title] >> "$GITHUB_STEP_SUMMARY"

results.json is written by `pytest --case-results results.json`. Cases with
several tests (one per browser or parameter) pass only if all of them pass.
"""

import json
import sys
from collections import Counter
from pathlib import Path

import yaml

HERE = Path(__file__).resolve().parent.parent
ICON = {"passed": "✅ Passed", "failed": "❌ Failed", "error": "❌ Error", "skipped": "⏭️ Skipped"}
RANK = {"error": 3, "failed": 2, "skipped": 1, "passed": 0}


def main() -> int:
    results = json.loads(Path(sys.argv[1]).read_text())
    title = sys.argv[2] if len(sys.argv) > 2 else "External tests"
    cases = yaml.safe_load((HERE / "cases.yaml").read_text())

    by_case: dict[str, dict] = {}
    for r in results:
        c = by_case.setdefault(r["case"], {"outcome": "passed", "tests": 0, "duration": 0.0})
        c["tests"] += 1
        c["duration"] += r["duration"]
        if RANK[r["outcome"]] > RANK[c["outcome"]]:
            c["outcome"] = r["outcome"]

    counts = Counter(c["outcome"] for c in by_case.values())
    status = "❌" if counts["failed"] or counts["error"] else "✅"
    print(f"## {status} {title}\n")
    print(f"{len(by_case)} cases run: " + ", ".join(f"{n} {k}" for k, n in sorted(counts.items())) + ".\n")
    print("| Case | Title | Priority | Result | Tests | Time |")
    print("| --- | --- | --- | --- | ---: | ---: |")
    for cid in cases:  # plan order
        if cid in by_case:
            c, r = cases[cid], by_case[cid]
            print(f"| {cid} | {c['title']} | {c['priority']} | {ICON[r['outcome']]} | {r['tests']} | {r['duration']:.1f} s |")

    other = Counter(c["automation"] for cid, c in cases.items() if cid not in by_case)
    if other:
        print("\nNot run here: " + ", ".join(f"{n} {k}" for k, n in sorted(other.items()))
              + " (see tests/e2e/cases.yaml).")
    return 1 if counts["failed"] or counts["error"] else 0


if __name__ == "__main__":
    sys.exit(main())
