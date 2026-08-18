#!/usr/bin/env bash
# Smoke test for rollout-man.
#
# Covers the four things it does: orchestrate (resolve, admit, expand), execute
# (agent + verifier), report (results.jsonl and the table), ship.
#
# Needs go and CAP_SYS_ADMIN for the local executor's mount namespace.
# No Docker daemon, no database, no workflow engine.
set -uo pipefail
cd "$(dirname "$0")/../.."

RUNS=${RUNS:-/tmp/rm-smoke-runs}
BUCKET=${ROLLOUT_MAN_BUCKET:-/tmp/rm-smoke-bucket}
BIN=${BIN:-/tmp/rollout-man-smoke}
export ROLLOUT_MAN_BUCKET="$BUCKET"
export SMOKE_API_KEY="sk-smoketest-7f3a9c1d4e8b2a6f0c5d"

pass=0 fail=0
ok()   { printf '  \033[32mPASS\033[0m %s\n' "$1"; pass=$((pass+1)); }
bad()  { printf '  \033[31mFAIL\033[0m %s\n' "$1"; fail=$((fail+1)); }
check(){ if eval "$2"; then ok "$1"; else bad "$1"; fi; }
step() { printf '\n\033[1m%s\033[0m\n' "$1"; }

rm -rf "$RUNS" "$BUCKET"

step "0. build"
go build -o "$BIN" ./cmd/rollout-man || { bad "build"; exit 1; }
go test ./internal/... > /tmp/rm-unit.log 2>&1
check "unit tests pass" '[ $? -eq 0 ]'
ok "binary built"

step "1. cases resolve to a stable content hash"
A=$("$BIN" cases test/smoke/smoke.yaml 2>&1)
B=$("$BIN" cases test/smoke/smoke.yaml 2>&1)
echo "$A" | sed 's/^/    /'
check "the same content hashes the same twice" '[ "$A" = "$B" ]'
check "both smoke cases resolved" '[ "$(echo "$A" | grep -c cpus=)" -eq 2 ]'

step "2. the shipped Harbor cases parse"
R=$("$BIN" cases test/smoke/real-cases.yaml 2>&1)
echo "$R" | sed 's/^/    /'
check "libaom task.toml parsed (4 cpus / 8192MB)" 'echo "$R" | grep -q "libaom.*cpus=4 mem=8192MB"'
check "spidermonkey task.toml parsed"             'echo "$R" | grep -q "spidermonkey.*cpus=4 mem=8192MB"'

step "3. a full run: admit, execute, record, ship"
OUT=$("$BIN" run test/smoke/smoke.yaml --runs "$RUNS" --id smoke --executor local 2>&1)
echo "$OUT" | grep -E 'want|matrix|shipped' | sed 's/^/    /'
DIR="$RUNS/smoke"
check "admission passed four probes" '[ "$(echo "$OUT" | grep -c "want.*-> PASS")" -eq 4 ]'
check "matrix expanded to 12 trials"  'echo "$OUT" | grep -q "matrix: 12 trials"'
check "oracle scores 1.00"            'echo "$OUT" | grep -q "oracle \[1\] want >= 1.00 -> PASS"'
check "nop scores 0.00"               'echo "$OUT" | grep -q "nop \[0\] want <= 0.00 -> PASS"'
check "every trial recorded once"     '[ "$(wc -l < "$DIR/results.jsonl")" -eq 12 ]'
check "the run shipped"               'echo "$OUT" | grep -q "^.*shipped "'
check "shipped copy has the results"  '[ -f "$BUCKET/evals/smoke/smoke/results.jsonl" ]'
check "scratch did not ship"          '[ ! -d "$BUCKET/evals/smoke/smoke/tmp" ]'

step "4. re-running the same id resumes instead of repeating"
OUT2=$("$BIN" run test/smoke/smoke.yaml --runs "$RUNS" --id smoke --executor local 2>&1)
echo "$OUT2" | grep -E 'resuming|reward' | head -3 | sed 's/^/    /'
check "it noticed the finished trials" 'echo "$OUT2" | grep -q "resuming: 12 trials"'
check "it re-ran nothing"              '! echo "$OUT2" | grep -q "reward"'
check "results.jsonl did not grow"     '[ "$(wc -l < "$DIR/results.jsonl")" -eq 12 ]'

step "5. redaction: keys always, IPs only in what ships"
T="$DIR/trials/test-smoke-leaky-case-leaky-fake-prod-1/out"
sed 's/^/    /' "$T/traj.jsonl" "$T/agent.log"
check "no plaintext key under the run dir" '! grep -rq "$SMOKE_API_KEY" "$DIR"'
check "key redacted in the trajectory"     'grep -q "REDACTED_KEY" "$T/traj.jsonl"'
check "IP redacted in the trajectory"      'grep -q "REDACTED_IP" "$T/traj.jsonl"'
check "key redacted in agent.log"          'grep -q "REDACTED_KEY" "$T/agent.log"'
check "IP kept in agent.log"               'grep -q "10.1.2.3" "$T/agent.log"'
check "host environment did not leak"      '! grep -qE "CLAUDE_CODE|AWS_SECRET" "$T/agent.log"'

step "6. status"
"$BIN" status "$DIR" --pass-at 0.8 2>&1 | sed 's/^/    /'
RES=$("$BIN" status "$DIR" --pass-at 0.8 2>&1)
check "three agent rows" '[ "$(echo "$RES" | grep -cE "^(oracle|nop|leaky) ")" -eq 3 ]'
check "oracle mean 1.000" 'echo "$RES" | grep -E "^oracle " | grep -q "1.000"'
check "nop mean 0.000"    'echo "$RES" | grep -E "^nop " | grep -q "0.000"'

step "7. the admission gate refuses an unmeasurable case"
# These cases need their prebuilt ASan images; with no Docker daemon the oracle
# cannot score, so the gate must refuse rather than emit numbers.
RC=$("$BIN" run test/smoke/real-cases.yaml --runs "$RUNS" --id real --executor local 2>&1)
echo "$RC" | grep -E 'want|CASE_NOT_ADMITTED' | sed 's/^/    /'
check "the case is rejected"                  'echo "$RC" | grep -q "CASE_NOT_ADMITTED"'
check "no trials were run"                    '[ ! -f "$RUNS/real/results.jsonl" ]'

printf '\n\033[1m%d passed, %d failed\033[0m\n' "$pass" "$fail"
[ "$fail" -eq 0 ]
