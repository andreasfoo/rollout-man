#!/usr/bin/env bash
# Interrupt a run mid-trial, then re-run with the same id.
#
# Resuming is a property of the checkpoint, not of an engine: a trial whose id
# is already in results.jsonl has happened, and nothing else needs remembering.
set -uo pipefail
cd "$(dirname "$0")/../.."

RUNS=${RUNS:-/tmp/rm-resume-runs}
BIN=${BIN:-/tmp/rollout-man-resume}

pass=0 fail=0
ok(){ printf '  \033[32mPASS\033[0m %s\n' "$1"; pass=$((pass+1)); }
bad(){ printf '  \033[31mFAIL\033[0m %s\n' "$1"; fail=$((fail+1)); }
check(){ if eval "$2"; then ok "$1"; else bad "$1"; fi; }

rm -rf "$RUNS"
go build -o "$BIN" ./cmd/rollout-man || exit 1
DIR="$RUNS/resume"

# two ~12s trials, concurrency 1: kill during the second one
"$BIN" run test/smoke/resume.yaml --runs "$RUNS" --id resume --executor local > /tmp/rm-resume1.log 2>&1 &
PID=$!
# trial 1 takes ~12s; wake up while trial 2 is in flight
sleep 18
echo "  killing the run (pid $PID) during trial 2"
kill -9 $PID 2>/dev/null
wait $PID 2>/dev/null

FIRST=$(wc -l < "$DIR/results.jsonl" 2>/dev/null || echo 0)
echo "  trials recorded before the kill: $FIRST"
check "the first trial was recorded"   '[ "$FIRST" -ge 1 ]'
check "the run did not finish"         '[ "$FIRST" -lt 2 ]'

OUT=$("$BIN" run test/smoke/resume.yaml --runs "$RUNS" --id resume --executor local 2>&1)
echo "$OUT" | grep -E 'resuming|reward' | sed 's/^/    /'
check "the rerun saw the checkpoint"   'echo "$OUT" | grep -q "resuming: $FIRST trials"'
check "it only ran what was missing"   '[ "$(echo "$OUT" | grep -c reward)" -eq $((2 - FIRST)) ]'
check "the run is complete now"        '[ "$(wc -l < "$DIR/results.jsonl")" -eq 2 ]'
check "no trial was recorded twice"    '[ "$(cut -d, -f1 "$DIR/results.jsonl" | sort -u | wc -l)" -eq 2 ]'

printf '\n\033[1m%d passed, %d failed\033[0m\n' "$pass" "$fail"
[ "$fail" -eq 0 ]
