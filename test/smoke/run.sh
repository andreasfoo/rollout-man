#!/usr/bin/env bash
# Smoke test for the rollout-man MVP.
#
# Exercises: multi-document YAML, case resolve + CAS determinism, the admission
# gate (both verdicts), matrix expansion, the trial main chain, tiered
# redaction, bundling, staged batch upload, per_experiment report/deploy, and
# the Postgres read model.
#
# Needs: go, a PostgreSQL reachable at $ROLLOUT_MAN_DSN, and CAP_SYS_ADMIN for
# the local executor's mount namespace. No Docker daemon required.
set -uo pipefail
cd "$(dirname "$0")/../.."

WORK=${WORK:-/tmp/rm-smoke-work}
BUCKET=${ROLLOUT_MAN_BUCKET:-/tmp/rm-smoke-bucket}
BIN=${BIN:-/tmp/rollout-man-smoke}
export ROLLOUT_MAN_BUCKET="$BUCKET"
export SMOKE_API_KEY="sk-smoketest-7f3a9c1d4e8b2a6f0c5d"

pass=0 fail=0
ok()   { printf '  \033[32mPASS\033[0m %s\n' "$1"; pass=$((pass+1)); }
bad()  { printf '  \033[31mFAIL\033[0m %s\n' "$1"; fail=$((fail+1)); }
check(){ if eval "$2"; then ok "$1"; else bad "$1"; fi; }
step() { printf '\n\033[1m%s\033[0m\n' "$1"; }

rm -rf "$WORK" "$BUCKET"

step "0. build"
go build -o "$BIN" ./cmd/rollout-man || { bad "build"; exit 1; }
ok "binary built"

step "1. case resolve is deterministic and content-addressed"
A=$("$BIN" case resolve test/smoke/smoke.yaml --work "$WORK" 2>&1)
B=$("$BIN" case resolve test/smoke/smoke.yaml --work "$WORK" 2>&1)
echo "$A" | sed 's/^/    /'
check "same content resolves to the same sha256 twice" '[ "$A" = "$B" ]'
check "both smoke cases reach READY" '[ "$(echo "$A" | grep -c READY)" -eq 2 ]'
check "objects landed in the CAS" '[ "$(find "$WORK/objects" -type f | wc -l)" -ge 2 ]'

step "2. the shipped Harbor cases parse"
R=$("$BIN" experiment create test/smoke/real-cases.yaml --work "$WORK" --dry-run 2>&1)
echo "$R" | grep -E 'cpus=' | sed 's/^/    /'
check "libaom task.toml parsed (4 cpus / 8192MB)" 'echo "$R" | grep -q "libaom.*cpus=4 mem=8192MB"'
check "spidermonkey task.toml parsed"             'echo "$R" | grep -q "spidermonkey.*cpus=4 mem=8192MB"'

step "3. full experiment: admission, trials, redaction, batch upload"
OUT=$("$BIN" experiment create test/smoke/smoke.yaml --work "$WORK" --executor local 2>&1)
echo "$OUT" | grep -E 'admission|matrix|per_experiment|redact hit' | sed 's/^/    /'
EXP=$(echo "$OUT" | sed -n 's/^experiment id: //p')
check "admission passed for both cases"     '[ "$(echo "$OUT" | grep -c "admission.*PASS")" -eq 4 ]'
check "matrix expanded to 6 tasks/12 trials" 'echo "$OUT" | grep -q "6 tasks / 12 trials"'
check "oracle scores 1.000"                  'echo "$OUT" | grep -q "oracle rewards=\[1\]"'
check "nop scores 0.000"                     'echo "$OUT" | grep -q "nop rewards=\[0\]"'
check "staged artifacts shipped in one call" 'echo "$OUT" | grep -q "shipped in 1 call"'
check "deploy step ran"                      'echo "$OUT" | grep -q "per_experiment deploy: ok"'

step "4. redaction: keys always, IPs only in what ships"
V=$(mktemp -d); tar xzf "$BUCKET"/evals/smoke/*.tar.gz -C "$V"
T=$(ls -d "$V"/*leaky-case-leaky-fake-prod-1 2>/dev/null | head -1)
BD=$(mktemp -d); tar xzf "$T/bundle.tar.gz" -C "$BD"
sed 's/^/    /' "$BD/traj.jsonl"; sed 's/^/    /' "$BD/agent.log"
check "no plaintext key anywhere in the bucket" '! grep -rq "$SMOKE_API_KEY" "$BUCKET"'
check "key redacted in the trajectory"          'grep -q "REDACTED_KEY" "$BD/traj.jsonl"'
check "IP redacted in the trajectory"           'grep -q "REDACTED_IP" "$BD/traj.jsonl"'
check "key redacted in agent.log"               'grep -q "REDACTED_KEY" "$BD/agent.log"'
check "IP kept in agent.log (debuggability)"    'grep -q "10.1.2.3" "$BD/agent.log"'
check "host environment did not leak"           '! grep -qE "CLAUDE_CODE|AWS_SECRET" "$BD/agent.log"'
rm -rf "$V" "$BD"

step "5. read model"
"$BIN" experiment results "$EXP" --pass-at 0.8 2>&1 | sed 's/^/    /'
RES=$("$BIN" experiment results "$EXP" --pass-at 0.8 2>&1)
check "three agent rows recorded" '[ "$(echo "$RES" | grep -cE "^(oracle|nop|leaky) ")" -eq 3 ]'
check "oracle mean is 1.000"      'echo "$RES" | grep -E "^oracle " | grep -q "1.000"'
check "nop mean is 0.000"         'echo "$RES" | grep -E "^nop " | grep -q "0.000"'

step "6. the admission gate refuses an unmeasurable case"
# The shipped cases need their prebuilt ASan images; without a Docker daemon the
# oracle cannot score, so the gate must reject rather than emit numbers.
RC=$("$BIN" experiment create test/smoke/real-cases.yaml --work "$WORK" --executor local 2>&1)
echo "$RC" | grep -E 'admission|CASE_NOT_ADMITTED' | sed 's/^/    /'
check "oracle failing its criterion rejects the case" 'echo "$RC" | grep -q "CASE_NOT_ADMITTED"'
check "the experiment does not proceed to trials"     '! echo "$RC" | grep -q "tasks /"'

printf '\n\033[1m%d passed, %d failed\033[0m\n' "$pass" "$fail"
[ "$fail" -eq 0 ]
