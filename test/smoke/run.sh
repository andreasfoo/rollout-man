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

step "9. the trial adapter: rollout-man asks for a number, it does not run containers"
# Building the image, starting it, running the agent inside it and running the
# verifier are Harbor's job. rollout-man reaches Harbor the way it reaches every
# other external system -- a configured command -- so this step runs against a
# stand-in adapter and needs no Harbor and no daemon.
HOUT=$("$BIN" run test/smoke/harbor.yaml --runs "$RUNS" --id harbor 2>&1)
HDIR="$RUNS/harbor"
echo "$HOUT" | grep -E 'want|matrix|reward' | sed 's/^/    /'
HT="$HDIR/trials/test-smoke-harbor-case-oracle-1/out"
check "--executor auto found the configured command" 'echo "$HOUT" | grep -q "(harbor executor)"'
check "the gate ran both probes through the adapter" '[ "$(echo "$HOUT" | grep -c "want.*-> PASS")" -eq 2 ]'
check "oracle scores 1.00 through the adapter" 'echo "$HOUT" | grep -q "oracle \[1\] want >= 1.00 -> PASS"'
check "nop scores 0.00 through the adapter"    'echo "$HOUT" | grep -q "nop \[0\] want <= 0.00 -> PASS"'
check "the score came from the adapter, not from us" '[ "$(cat "$HT/reward.txt")" = "1.0" ]'
check "artifacts came back from the adapter"    'grep -q "deliverable survived" "$HT/verifier.stdout.log"'
# Every limit task.toml declares has to reach the adapter: it is the only thing
# that can actually enforce them, because it is the thing that started the case.
check "the case's limits reached the adapter" \
  'grep -q "^AGENT_TIMEOUT_SEC=120$" "$HT/adapter-env.txt" &&
   grep -q "^VERIFIER_USER=root$"    "$HT/adapter-env.txt" &&
   grep -q "^MEMORY_MB=512$"         "$HT/adapter-env.txt" &&
   grep -q "^ALLOW_INTERNET=0$"      "$HT/adapter-env.txt"'

# An adapter that knows why it failed says so, and the code passes through.
HF=$(FAKE_HARBOR_FAIL=declared "$BIN" run test/smoke/harbor.yaml --runs "$RUNS" --id harbor-f1 2>&1)
echo "$HF" | grep -E 'could not run' | sed 's/^/    /'
check "a declared failure code passes through" 'echo "$HF" | grep -q "AGENT_TIMEOUT: the agent outran its clock"'
# An adapter that just dies must not be read as the agent failing. "Could not
# measure" is ours, and putting it in the agent's denominator corrupts the number.
HS=$(FAKE_HARBOR_FAIL=silent "$BIN" run test/smoke/harbor.yaml --runs "$RUNS" --id harbor-f2 2>&1)
echo "$HS" | grep -E 'could not run' | sed 's/^/    /'
check "an adapter that dies silently is ENV_FAILED, not AGENT_*" \
  'echo "$HS" | grep -q "ENV_FAILED" && ! echo "$HS" | grep -q "AGENT_"'
check "the gate refused rather than scoring the case" '[ ! -f "$RUNS/harbor-f1/results.jsonl" ]'

step "10. the real thing: rollout-man orchestrating, Harbor executing"
# Everything above proves the orchestration. This proves the seam: the same
# submission, the same gate, the same numbers -- but the trial actually runs
# through `harbor run`, in a container Harbor built.
if ! command -v harbor >/dev/null 2>&1; then
  skip "Harbor executor (harbor not installed -- pip install harbor)"
elif ! docker info >/dev/null 2>&1; then
  skip "Harbor executor (no docker daemon on this host)"
else
  ROUT=$("$BIN" run test/smoke/harbor-real.yaml --runs "$RUNS" --id harbor-real 2>&1)
  RDIR="$RUNS/harbor-real"
  echo "$ROUT" | grep -E 'want|matrix|reward' | sed 's/^/    /'
  RT="$RDIR/trials/test-smoke-harbor-case-oracle-1/out"
  check "the gate ran both probes through Harbor" '[ "$(echo "$ROUT" | grep -c "want.*-> PASS")" -eq 2 ]'
  check "oracle scores 1.00 through Harbor" 'echo "$ROUT" | grep -q "oracle \[1\] want >= 1.00 -> PASS"'
  check "nop scores 0.00 through Harbor"    'echo "$ROUT" | grep -q "nop \[0\] want <= 0.00 -> PASS"'
  check "two trials recorded"               '[ "$(wc -l < "$RDIR/results.jsonl")" -eq 2 ]'
  check "the score came out of Harbor's own reward file" '[ "$(cat "$RT/reward.txt")" = "1.0" ]'
  check "Harbor's trial log came back"      '[ -s "$RT/trial.log" ]'
fi

step "11. a real Harbor security case, end to end"
# The synthetic case above proves the seam. This proves the whole thing: a real
# case, a real ASan crash, and a verifier that scores from its own gdb
# backtrace -- nothing here can be satisfied by a plausible-looking artifact.
if ! command -v harbor >/dev/null 2>&1 || ! docker info >/dev/null 2>&1; then
  skip "real Harbor case (needs harbor + docker)"
elif [ ! -s test/cases/libaom-cc9e46cb-bug-6491968-t4/solution/testcase.bin ]; then
  skip "real Harbor case (the case ships no PoC)"
else
  LOUT=$("$BIN" run test/smoke/libaom.yaml --runs "$RUNS" --id libaom 2>&1)
  LDIR="$RUNS/libaom"
  echo "$LOUT" | grep -E 'want|reward' | sed 's/^/    /'
  LT="$LDIR/trials/test-cases-libaom-cc9e46cb-bug-6491968-t4-oracle-1/out"
  check "the case is admitted"        '! echo "$LOUT" | grep -q CASE_NOT_ADMITTED'
  check "oracle scores 1.00"          'echo "$LOUT" | grep -q "oracle \[1\] want >= 1.00 -> PASS"'
  check "nop scores 0.00"             'echo "$LOUT" | grep -q "nop \[0\] want <= 0.00 -> PASS"'
  # The score has to come from the defect, not from the file merely existing.
  check "the reward is a real ASan crash" 'grep -q "AddressSanitizer: heap-buffer-overflow" "$LT/stdout.log"'
  check "in the frame the case names"     'grep -q "od_ec_dec_refill" "$LT/stdout.log"'
  check "no verifier check failed"           '! grep -q "^FAIL (+" "$LT/stdout.log"'
  check "the verifier scored it a full 1.00" 'grep -q "^FINAL SCORE: 1.00" "$LT/stdout.log"'
fi

step "12. commands as pinned, isolated adapters"
# The safety story: a command can be a script file pinned by hash, run with only
# the environment it declares, taken from a file the submission cannot override.
PINDIR="$RUNS/pin"; rm -rf "$PINDIR"; mkdir -p "$PINDIR"
cat > "$PINDIR/adapter.sh" <<'A'
#!/usr/bin/env bash
# Record exactly what environment the ship command was handed. OUT_DIR is not
# set for a ship command, so use LOCAL_PATH (the run directory) directly.
echo "KEEP=${KEEP:-<unset>} DROP=${DROP:-<unset>}" > "$LOCAL_PATH/saw.txt"
A
SUM=$(sha256sum "$PINDIR/adapter.sh" | cut -d" " -f1)
cat > "$PINDIR/commands.yaml" <<C
---
kind: Commands
inherit_env: false
ship:
  uses: $PINDIR/adapter.sh
  sha256: $SUM
  env: [KEEP]
C
cat > "$PINDIR/exp.yaml" <<C
---
kind: Experiment
name: pin
case_defaults: {source: local}
cases: [{path: test/smoke/pass-case}]
matrix: {agents: [oracle], trials: 1}
concurrency: 1
pipeline:
  per_case: {admission: {require: any}}
  per_experiment: {ship: {using: ship, dest: pin}}
C
# A run whose commands come from the trusted file.
KEEP=yes DROP=secret "$BIN" run "$PINDIR/exp.yaml" --runs "$PINDIR/runs" --id r --executor local --commands "$PINDIR/commands.yaml" > "$PINDIR/out.log" 2>&1
SAW="$PINDIR/runs/r/saw.txt"
check "the run records which command ran and its hash" 'grep -q "\"sha256\": \"'"$SUM"'\"" "$PINDIR/runs/r/manifest.json"'
check "the manifest marks the commands as trusted"     'grep -q "commands_from_trusted_file\": true" "$PINDIR/runs/r/manifest.json"'
check "the declared env var reached the command"       'grep -q "KEEP=yes"     "$SAW"'
check "an undeclared env var did NOT"                   'grep -q "DROP=<unset>" "$SAW"'

# Tamper with the pinned file: the run must refuse rather than run it.
echo "# tampered" >> "$PINDIR/adapter.sh"
KEEP=yes "$BIN" run "$PINDIR/exp.yaml" --runs "$PINDIR/runs2" --id r --executor local --commands "$PINDIR/commands.yaml" > "$PINDIR/tamper.log" 2>&1
check "a changed pin is refused, not run" 'grep -q "refusing to run" "$PINDIR/tamper.log"'

# A submission that smuggles its own commands alongside a trusted file is refused.
cat > "$PINDIR/evil.yaml" <<C
---
kind: Commands
ship: {script: "echo pwned"}
---
kind: Experiment
name: pin
case_defaults: {source: local}
cases: [{path: test/smoke/pass-case}]
matrix: {agents: [oracle], trials: 1}
concurrency: 1
pipeline: {per_case: {admission: {require: any}}}
C
"$BIN" run "$PINDIR/evil.yaml" --runs "$PINDIR/runs3" --id r --executor local --commands "$PINDIR/commands.yaml" > "$PINDIR/evil.log" 2>&1
check "a submission cannot override the trusted commands" 'grep -q "declares its own kind: Commands" "$PINDIR/evil.log"'

printf '\n\033[1m%d passed, %d failed, %d skipped\033[0m\n' "$pass" "$fail" "$skipped"
[ "$fail" -eq 0 ]
