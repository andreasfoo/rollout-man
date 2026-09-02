#!/usr/bin/env python3
"""Backfill rewards for tc-batch2-kimi3-x3 AGENT_TIMEOUT rows (2026-09-01).

The pre-00:00-UTC kimi3-rollout.sh treated any harbor exception as a failure
before checking verifier_result, so 31 timed-out trials were recorded with
reward=null even though harbor's verifier scored the end state. Their
trajectories already went through post-trial (2 shipped, 29 blocked by the
api-error gate), so this is purely a stats repair: reward in, failure_code
out, timeout fact preserved in notes.

Run ONLY after the campaign runner has exited (it appends to results.jsonl).
Usage: python3 adapters/recover-batch2-timeout-rewards.py <run-dir> [--apply]
Without --apply, prints what would change.
"""
import glob
import json
import subprocess
import sys

def main():
    run_dir, apply = sys.argv[1], "--apply" in sys.argv
    results_path = f"{run_dir}/results.jsonl"

    if subprocess.run(["pgrep", "-f", "rollout-man run .*tc-batch2-kimi3-x3"],
                      capture_output=True).returncode == 0:
        sys.exit("runner still alive -- refusing to rewrite results.jsonl under it")

    rows = [json.loads(l) for l in open(results_path)]
    changed = 0
    for r in rows:
        if r.get("failure_code") != "AGENT_TIMEOUT":
            continue
        hits = glob.glob(f"{run_dir}/trials/{r['trial_id']}/harbor/*/case__*/result.json")
        if not hits:
            sys.exit(f"no harbor result.json for {r['trial_id']} -- cannot backfill")
        res = json.load(open(hits[0]))
        rewards = (res.get("verifier_result") or {}).get("rewards") or {}
        if "reward" not in rewards:
            sys.exit(f"harbor recorded no reward for {r['trial_id']} -- investigate by hand")
        r["reward"] = rewards["reward"]
        # notes is map[string]any in Go (run.Result.Notes) -- a bare string
        # makes results.jsonl unloadable on the next resume. Key it.
        notes = r.get("notes")
        if not isinstance(notes, dict):
            notes = {}
        notes["timeout_backfill"] = \
            "AgentTimeoutError: verifier scored the end state; reward backfilled 2026-09-01"
        r["notes"] = notes
        del r["failure_code"]
        changed += 1
        print(f"  {r['reward']:4.2f}  {r['trial_id'].split('batch2-')[-1]}")

    print(f"{changed} rows to backfill")
    if apply and changed:
        with open(results_path, "w") as f:
            for r in rows:
                f.write(json.dumps(r) + "\n")
        print(f"wrote {results_path}")

if __name__ == "__main__":
    main()
