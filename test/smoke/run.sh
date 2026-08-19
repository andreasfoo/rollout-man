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

pass=0 fail=0 skipped=0
ok()   { printf '  \033[32mPASS\033[0m %s\n' "$1"; pass=$((pass+1)); }
bad()  { printf '  \033[31mFAIL\033[0m %s\n' "$1"; fail=$((fail+1)); }
skip() { printf '  \033[33mSKIP\033[0m %s\n' "$1"; skipped=$((skipped+1)); }
check(){ if eval "$2"; then ok "$1"; else bad "$1"; fi; }
step() { printf '\n\033[1m%s\033[0m\n' "$1"; }

rm -rf "$RUNS" "$BUCKET"

step "0. build"
go build -o "$BIN" ./cmd/rollout-man || { bad "build"; exit 1; }
if go test ./internal/... > /tmp/rm-unit.log 2>&1; then ok "unit tests pass"; else bad "unit tests pass"; tail -20 /tmp/rm-unit.log | sed 's/^/    /'; fi
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

step "8. the failure taxonomy: every code, and the denominator it protects"
# The taxonomy earns its keep in exactly one place -- who gets blamed. So the
# test provokes an agent failure, an agent timeout, an environment that cannot
# be staged and a verifier that produces no number, all in one run.
rm -f test/smoke/fail-cases/flaky-verifier/tests/.attempted
FSTART=$(date +%s)
FOUT=$("$BIN" run test/smoke/failures.yaml --runs "$RUNS" --id failures --executor local 2>&1)
FELAPSED=$(( $(date +%s) - FSTART ))
FDIR="$RUNS/failures"
echo "$FOUT" | grep -E 'AGENT_|ENV_|VERIFIER_|reward' | sed 's/^/    /'
check "five trials recorded"            '[ "$(wc -l < "$FDIR/results.jsonl")" -eq 5 ]'
check "AGENT_FAILED on a non-zero exit" 'grep -q "\"failure_code\":\"AGENT_FAILED\"" "$FDIR/results.jsonl"'
check "AGENT_TIMEOUT on an overrun"     'grep -q "\"failure_code\":\"AGENT_TIMEOUT\"" "$FDIR/results.jsonl"'
check "ENV_FAILED when the case cannot be staged" 'grep -q "\"failure_code\":\"ENV_FAILED\"" "$FDIR/results.jsonl"'
check "VERIFIER_ERROR when no number is produced" 'grep -q "\"failure_code\":\"VERIFIER_ERROR\"" "$FDIR/results.jsonl"'
# A 2s agent timeout against a 30s agent: without a process-group kill the
# runner would sit on the pipe until the agent finished anyway.
check "the timeout ends the agent, not just the wait" '[ "$FELAPSED" -lt 15 ]'
check "a retryable failure was retried"  'grep "verifier-mute" "$FDIR/results.jsonl" | grep -q "\"attempts\":2"'
check "the retry is what produced the score" 'grep "flaky-verifier" "$FDIR/results.jsonl" | grep -q "\"attempts\":2,\"reward\":1"'
check "an agent failure leaves readable logs" 'grep -q "about to fail" "$FDIR/trials/test-smoke-fail-cases-agent-fail-oracle-1/out/stdout.log"'
FRES=$("$BIN" status "$FDIR" --pass-at 0.8 2>&1)
echo "$FRES" | sed 's/^/    /'
# 1 scored + 2 agent failures = 3. The environment and the verifier are ours.
check "only the agent's own failures are in the denominator" 'echo "$FRES" | grep -q "1/3 = 33%"'

step "9. the docker executor: one container per trial"
# The Harbor contract splits the trial in two -- the agent leaves a file in
# /app, the verifier reads it back -- so the two steps have to see the same
# filesystem. Running them as two `docker run --rm` invocations scores every
# agent zero, silently and plausibly. This step exists to catch that.
if ! docker info >/dev/null 2>&1; then
  skip "docker executor (no docker daemon on this host)"
else
  BASE=${RM_SMOKE_BASE:-}
  if [ -z "$BASE" ]; then
    if docker pull -q debian:bookworm-slim >/dev/null 2>&1; then
      BASE=debian:bookworm-slim
    else
      # No registry reachable: build a base out of the host's own bash and
      # coreutils rather than skip the step and call the run green.
      BASE=$(bash test/smoke/make-base-image.sh) || BASE=""
    fi
  fi
  if [ -z "$BASE" ]; then
    skip "docker executor (no usable base image)"
  else
    CASE_DIR="$RUNS/docker-case"
    rm -rf "$CASE_DIR"; mkdir -p "$RUNS"; cp -r test/smoke/docker-case "$CASE_DIR"
    sed -i "s|^FROM .*|FROM $BASE|" "$CASE_DIR/environment/Dockerfile"
    sed "s|path: test/smoke/docker-case|path: $CASE_DIR|" test/smoke/docker.yaml > "$RUNS/docker.yaml"
    DOUT=$("$BIN" run "$RUNS/docker.yaml" --runs "$RUNS" --id docker --executor docker 2>&1)
    DDIR="$RUNS/docker"
    echo "$DOUT" | grep -E 'want|matrix|reward' | sed 's/^/    /'
    # the trial id is a slug of the case path, so match on it rather than spell it
    DT=$(ls -d "$DDIR"/trials/*docker-case-oracle-1/out)
    check "the gate ran both probes in containers" '[ "$(echo "$DOUT" | grep -c "want.*-> PASS")" -eq 2 ]'
    check "oracle scores 1.00 under docker"        'echo "$DOUT" | grep -q "oracle \[1\] want >= 1.00 -> PASS"'
    check "nop scores 0.00 under docker"           'echo "$DOUT" | grep -q "nop \[0\] want <= 0.00 -> PASS"'
    check "the deliverable survived into the verifier" 'grep -q "deliverable survived" "$DT/verifier.stdout.log"'
    check "the score came from inside the container"   '[ "$(cat "$DT/reward.txt")" = "1.0" ]'
    check "agent and verifier ran as different users"  'grep -q "verifier ran as root" "$DT/verifier.log"'
    check "no container was left behind" '[ -z "$(docker ps -aq --filter name=^rm-)" ]'
    check "no case image was left behind" '[ -z "$(docker images -q "rollout-man/case")" ]'
  fi
fi

printf '\n\033[1m%d passed, %d failed, %d skipped\033[0m\n' "$pass" "$fail" "$skipped"
[ "$fail" -eq 0 ]
