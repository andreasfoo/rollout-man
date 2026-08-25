#!/usr/bin/env bash
# Mock forge-flow-fuzz adapter for pre-accepted cases (tc_batch2 and similar).
#
# All cases are already accepted with trajectories; this adapter skips the LLM
# lifecycle entirely and maps directly to reward=0.0 (accepted).
#
# casefactory produces two jobs/ layouts depending on how the case was driven:
#
#   flat:   jobs/<trial_name>/result.json        (harbor ran directly)
#   nested: jobs/<batch_dir>/<trial_name>/result.json  (casefactory batch)
#
# The adapter identifies trial-level result.json files by the presence of both
# trial_name and started_at fields, then copies each to a flat
# out/case/trials/<trial_name>/ directory so materialize_week2 can walk it
# the same way it does for live forge-flow-fuzz runs.
#
# For oracle/nop trials the request is delegated to the Harbor adapter so the
# admission gate still functions normally.
#
# ENV:
#   CASE_DIR   (in)  source task directory (tc_batch2 case)
#   OUT_DIR    (in)  where to write reward.txt / terminal.json / case/
#   AGENT_KIND (in)  oracle | nop | llm
#   AGENT_NAME (in)  must be forge-flow-fuzz for the llm branch
#   ROLLOUT_MAN_HARBOR_ADAPTER  (opt) path to harbor.sh, default adapters/harbor.sh
set -euo pipefail

fail() { printf '%s\n%s\n' "$1" "$2" > "$OUT_DIR/failure.txt"; exit 1; }

case "$AGENT_KIND" in
  oracle|nop)
    exec "${ROLLOUT_MAN_HARBOR_ADAPTER:-adapters/harbor.sh}"
    ;;
  llm) ;;
  *) fail HOST_ERROR "unknown agent kind $AGENT_KIND" ;;
esac

[ "${AGENT_NAME:-}" = "forge-flow-fuzz" ] || \
  fail HOST_ERROR "expected AGENT_NAME=forge-flow-fuzz, got ${AGENT_NAME:-}"

mkdir -p "$OUT_DIR"

# Walk jobs/ recursively and collect trial-level result.json entries.
# A trial-level file carries both trial_name and started_at; batch-level
# summaries have neither.
trials_json=$(python3 - "$CASE_DIR" <<'PY'
import json, os, sys
case_dir = sys.argv[1]
jobs_dir = os.path.join(case_dir, 'jobs')
if not os.path.isdir(jobs_dir):
    sys.exit(0)
for root, dirs, files in os.walk(jobs_dir):
    if 'result.json' not in files:
        continue
    p = os.path.join(root, 'result.json')
    try:
        r = json.load(open(p))
    except (OSError, json.JSONDecodeError):
        continue
    if r.get('trial_name') and r.get('started_at'):
        print(json.dumps({'dir': root, 'trial_name': r['trial_name'],
                          'started_at': r['started_at']}))
PY
)

if [ -z "$trials_json" ]; then
  fail ENV_FAILED "no trial result.json (with trial_name+started_at) found under $CASE_DIR/jobs/ -- cannot replay trajectories"
fi

# Copy the source case to out/case/ and build a flat trials/ subdir.
# materialize_week2 walks trials/ looking for result.json with trial_name and
# started_at; the flat layout matches what casefactory writes into its own
# cases/*/trials/ after a live run.
cp -a "$CASE_DIR" "$OUT_DIR/case"
rm -rf "$OUT_DIR/case/trials"
mkdir -p "$OUT_DIR/case/trials"

while IFS= read -r line; do
  [ -z "$line" ] && continue
  src_dir=$(printf '%s' "$line" | python3 -c 'import json,sys; print(json.loads(sys.stdin.read())["dir"])')
  trial_name=$(printf '%s' "$line" | python3 -c 'import json,sys; print(json.loads(sys.stdin.read())["trial_name"])')
  dst="$OUT_DIR/case/trials/$trial_name"
  [ -e "$dst" ] && continue
  cp -a "$src_dir" "$dst"
done <<< "$trials_json"

n=$(printf '%s\n' "$trials_json" | grep -c .)
printf 'forge-flow-fuzz-mock: %s -- %d trial(s) replayed from jobs/\n' \
  "$(basename "$CASE_DIR")" "$n"

# Minimal terminal record: materialize only reads out/case/trials/; terminal.json
# is kept as provenance alongside forge-flow-fuzz.log in the trajectory tree.
printf '{"case_id":"%s","status":"accepted","final":{"verdict":"accepted","final_reward":0.0}}\n' \
  "$(basename "$CASE_DIR")" > "$OUT_DIR/terminal.json"

printf '0.0\n' > "$OUT_DIR/reward.txt"
