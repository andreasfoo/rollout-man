#!/usr/bin/env bash
# Per-case trajectory layout check (post-admission).
#
# Validates that the case's jobs/ directory contains at least one
# accepted-quality trajectory before the mock adapter tries to replay it.
#
# Required layout (per trial-level result.json, identified by trial_name +
# started_at):
#   jobs/.../<trial_name>/result.json            -- trial record
#   jobs/.../<trial_name>/agent/trajectory.json  -- LLM conversation transcript
#
# A missing trajectory.json means the run was incomplete or the agent timed
# out before writing its session. materialize_week2 would publish an empty
# trajectory tree; this check blocks that before any dock-up or shipping step.
#
# Warnings (non-blocking):
#   - verifier/reward.txt absent: verifier did not write a score (partial run)
#   - verifier/reward.json absent: same
#
# ENV (all set by rollout-man):
#   CASE_DIR    path to the case package
#   CASE_LABEL  human-readable label for stderr messages
set -euo pipefail

python3 - "$CASE_DIR" "$CASE_LABEL" <<'PY'
import json, os, sys
case_dir, case_label = sys.argv[1], sys.argv[2]
jobs_dir = os.path.join(case_dir, 'jobs')

if not os.path.isdir(jobs_dir):
    print(f"check_traj_layout: FAIL {case_label} -- no jobs/ directory", file=sys.stderr)
    sys.exit(1)

trials = []   # (trial_dir, trial_name)
for root, dirs, files in os.walk(jobs_dir):
    if 'result.json' not in files:
        continue
    p = os.path.join(root, 'result.json')
    try:
        r = json.load(open(p))
    except (OSError, json.JSONDecodeError):
        continue
    if r.get('trial_name') and r.get('started_at'):
        trials.append((root, r['trial_name']))

if not trials:
    print(
        f"check_traj_layout: FAIL {case_label} -- no trial-level result.json "
        f"(with trial_name + started_at) found under jobs/",
        file=sys.stderr,
    )
    sys.exit(1)

errors = []
warnings = []
for trial_dir, trial_name in trials:
    # Required: the trial record and the LLM conversation transcript.
    for req in ('result.json', 'agent/trajectory.json'):
        p = os.path.join(trial_dir, req)
        if not os.path.isfile(p):
            errors.append(f"  MISSING {os.path.relpath(p, case_dir)}")
    # Warnings: verifier output files (absent if the run was cut short).
    for vf in ('verifier/reward.txt', 'verifier/reward.json'):
        vpath = os.path.join(trial_dir, vf)
        if not os.path.isfile(vpath):
            warnings.append(f"  WARN {os.path.relpath(vpath, case_dir)} not found (partial run?)")

if warnings:
    for w in warnings:
        print(f"check_traj_layout: {case_label}: {w}", file=sys.stderr)

if errors:
    print(
        f"check_traj_layout: FAIL {case_label} -- "
        f"{len(errors)} required file(s) missing:",
        file=sys.stderr,
    )
    for e in errors:
        print(e, file=sys.stderr)
    sys.exit(1)

print(
    f"check_traj_layout: ok {case_label} -- "
    f"{len(trials)} trial(s), result.json + agent/trajectory.json present"
)
PY
