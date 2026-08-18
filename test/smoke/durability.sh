#!/usr/bin/env bash
# Durability check: kill the worker mid-activity and confirm the workflow
# resumes rather than restarting. This is the only thing Temporal is here for,
# so it gets its own test.
set -uo pipefail
cd "$(dirname "$0")/../.."

WORK=${WORK:-/tmp/rm-dur-work}
BIN=${BIN:-/tmp/rollout-man-dur}
export ROLLOUT_MAN_BUCKET=${ROLLOUT_MAN_BUCKET:-/tmp/rm-dur-bucket}
export SLOW_SECONDS=${SLOW_SECONDS:-25}

pass=0; fail=0
ok(){ printf '  \033[32mPASS\033[0m %s\n' "$1"; pass=$((pass+1)); }
bad(){ printf '  \033[31mFAIL\033[0m %s\n' "$1"; fail=$((fail+1)); }
check(){ if eval "$2"; then ok "$1"; else bad "$1"; fi; }

rm -rf "$WORK" "$ROLLOUT_MAN_BUCKET"
go build -o "$BIN" ./cmd/rollout-man || exit 1

start_worker() {
  "$BIN" worker start --config test/smoke/durability.yaml --work "$WORK" \
    --executor local --runner runner-01 >> /tmp/rm-dur-worker.log 2>&1 &
  echo $! > /tmp/rm-dur-worker.pid
}

: > /tmp/rm-dur-worker.log
start_worker
sleep 4

"$BIN" experiment submit test/smoke/durability.yaml > /tmp/rm-dur-submit.log 2>&1 &
SUBMIT=$!
EXP=""
for _ in $(seq 1 20); do
  EXP=$(sed -n 's/^started \([^ ]*\).*/\1/p' /tmp/rm-dur-submit.log)
  [ -n "$EXP" ] && break
  sleep 1
done
echo "  experiment: $EXP"

sleep 10
W=$(cat /tmp/rm-dur-worker.pid)
echo "  killing worker pid $W while RunAgent is in flight"
kill -9 "$W" 2>/dev/null
sleep 2
check "worker is gone" '! kill -0 "$W" 2>/dev/null'

# The activity's heartbeat timeout has to expire before Temporal re-delivers,
# so give it room; the point is that the workflow survives, not that it is fast.
sleep 8
echo "  restarting worker"
start_worker

if wait $SUBMIT; then RC=0; else RC=$?; fi
tail -6 /tmp/rm-dur-submit.log | sed 's/^/    /'

check "the experiment completed after the crash" '[ "$RC" -eq 0 ]'
check "reward landed in the read model" \
  'psql -h /tmp -p 5433 -U rollout -d rollout_man -tAc "select reward from trials tr join tasks t on t.id=tr.task_id where t.experiment_id='"'"'"'"$EXP"'"'"'"'" 2>/dev/null | grep -q 1'
check "artifacts were uploaded" '[ -n "$(find "$ROLLOUT_MAN_BUCKET" -name "result.json" 2>/dev/null)" ]'

# A resumed workflow keeps its history: the same run id, with the agent step
# retried in place rather than the trial starting over.
H=$(temporal workflow show --address localhost:7233 -n rollout-man -w "$EXP" -o json 2>/dev/null)
check "the experiment kept a single run" '[ "$(echo "$H" | jq -r "[.events[]|select(.eventType==\"EVENT_TYPE_WORKFLOW_EXECUTION_STARTED\")]|length")" -eq 1 ]'

TRIAL=$(temporal workflow list --address localhost:7233 -n rollout-man --limit 50 -o json 2>/dev/null \
        | jq -r --arg e "$EXP" '.[].execution.workflowId | select(startswith($e + "-"))' | head -1)
TH=$(temporal workflow show --address localhost:7233 -n rollout-man -w "$TRIAL" -o json 2>/dev/null)
STARTS=$(echo "$TH" | jq -r '[.events[]|select(.eventType=="EVENT_TYPE_ACTIVITY_TASK_STARTED")]|length')
SCHED=$(echo "$TH" | jq -r '[.events[]|select(.eventType=="EVENT_TYPE_ACTIVITY_TASK_SCHEDULED" and .activityTaskScheduledEventAttributes.activityType.name=="PrepareCase")]|length')
echo "  trial $TRIAL: PrepareCase scheduled ${SCHED}x, activity starts ${STARTS}"
check "PrepareCase was not re-run after the crash" '[ "$SCHED" -eq 1 ]'

kill -9 "$(cat /tmp/rm-dur-worker.pid)" 2>/dev/null
printf '\n\033[1m%d passed, %d failed\033[0m\n' "$pass" "$fail"
[ "$fail" -eq 0 ]
