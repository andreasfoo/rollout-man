#!/usr/bin/env python3
"""A pipeline step that is not a shell script.

`uses:` names an executable file and the contract is "environment in, files
out". Nothing in it says shell -- this exists so that stays true, and so the
smoke test would notice if it stopped being true.
"""
import json
import os
import pathlib
import sys

run_dir = pathlib.Path(os.environ["RUN_DIR"])
results = run_dir / "results.jsonl"
rows = [json.loads(l) for l in results.read_text().splitlines() if l.strip()]
scored = [r for r in rows if r.get("reward") is not None]

summary = {
    "channel": os.environ.get("CHANNEL", "log"),
    "experiment": os.environ["EXPERIMENT"],
    "run": os.environ["RUN_ID"],
    "trials": len(rows),
    "measured": len(scored),
    "mean_reward": round(sum(r["reward"] for r in scored) / len(scored), 4) if scored else None,
}
(run_dir / "notify.json").write_text(json.dumps(summary, indent=2) + "\n")
print(json.dumps(summary), file=sys.stderr)
