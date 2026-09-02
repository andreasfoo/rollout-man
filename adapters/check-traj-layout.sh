#!/usr/bin/env bash
# Per-case trajectory layout check (post-admission).
#
# Validates that the case's jobs/ directory contains at least one
# accepted-quality trajectory before the mock adapter tries to replay it,
# and that the case's essential runnable files exist (2026-08-31: user
# direction -- test.sh, solve.sh and environment/ are essential; a case
# shipped without them cannot be verified or reproduced).
#
# Required case-level files:
#   tests/test.sh        -- the verifier driver
#   solution/solve.sh    -- the reference solution
#   environment/         -- the image build context (dir with a Dockerfile)
#
# Required layout (per trial-level result.json, identified by trial_name +
# started_at):
#   jobs/.../<trial_name>/result.json            -- trial record
#   jobs/.../<trial_name>/agent/trajectory.json  -- LLM conversation transcript
#   jobs/.../<trial_name>/verifier/reward.txt    -- the verifier's score
#   (reward.json accepted as the alternative reward file)
#
# A missing trajectory.json means the run was incomplete or the agent timed
# out before writing its session. A missing reward file means the verifier
# never scored the run -- an unscored trajectory is not publishable
# (2026-08-31: promoted from WARN to hard failure, user direction).
# materialize_week2 would publish an empty trajectory tree; this check
# blocks that before any dock-up or shipping step.
#
# ENV (all set by rollout-man):
#   CASE_DIR    path to the case package
#   CASE_LABEL  human-readable label for stderr messages
set -euo pipefail

python3 - "$CASE_DIR" "$CASE_LABEL" <<'PY'
import json, os, sys
case_dir, case_label = sys.argv[1], sys.argv[2]

errors = []

# Case-level essentials: the runnable triad (2026-08-31, user direction --
# "test.sh and solve.sh /environment dir are also essential").
for req in ('tests/test.sh', 'solution/solve.sh'):
    p = os.path.join(case_dir, req)
    if not os.path.isfile(p):
        errors.append(f"  MISSING {req} (essential case file)")
env_dir = os.path.join(case_dir, 'environment')
if not os.path.isdir(env_dir):
    errors.append("  MISSING environment/ (essential case dir)")
elif not any(os.path.isfile(os.path.join(env_dir, f)) for f in os.listdir(env_dir)):
    errors.append("  EMPTY environment/ (no files at all)")

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

for trial_dir, trial_name in trials:
    # Required: the trial record, the LLM conversation transcript, and the
    # verifier's score (reward.txt, reward.json accepted).
    for req in ('result.json', 'agent/trajectory.json'):
        p = os.path.join(trial_dir, req)
        if not os.path.isfile(p):
            errors.append(f"  MISSING {os.path.relpath(p, case_dir)}")
    rd = os.path.join(trial_dir, 'verifier')
    if not (os.path.isfile(os.path.join(rd, 'reward.txt'))
            or os.path.isfile(os.path.join(rd, 'reward.json'))):
        errors.append(
            f"  MISSING {os.path.relpath(os.path.join(rd, 'reward.txt'), case_dir)}"
            f" (verifier/reward.json neither) -- unscored trial [{trial_name}]")

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
    f"{len(trials)} trial(s), result.json + agent/trajectory.json + verifier/reward.* present"
)
PY
