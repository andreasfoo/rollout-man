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

# The documented examples are documentation only for as long as they still
# load. A worked example that no longer parses is worse than none.
#
# What is checked is the submission, not the network: the pipeline is
# validated before any case is fetched, so an example pointing at a repo this
# machine cannot reach is still checked for everything that is ours.
for ex in experiments/*.yaml; do
  case "$ex" in */commands.yaml) continue ;; esac
  log=/tmp/rm-validate-$(basename "$ex" .yaml).log
  "$BIN" run "$ex" --runs /tmp/rm-validate --id v --executor local > "$log" 2>&1
  check "$(basename "$ex") still loads and validates" \
    "! grep -qE 'unknown (input|action|key)|^load:|has no uses:|must be' $log"
done

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
# 2 cases x (1 oracle + 1 nop + 2 leaky). The built-ins run once each however
# high trials goes: oracle runs the case's own solve script and nop does
# nothing, so a second rollout of either measures the same thing again.
check "matrix expanded to 8 trials"   'echo "$OUT" | grep -q "matrix: 8 trials"'
check "and said why it was not 12"    'echo "$OUT" | grep -q "oracle, nop ran once: deterministic"'
check "oracle scores 1.00"            'echo "$OUT" | grep -q "oracle \[1\] want >= 1.00 -> PASS"'
check "nop scores 0.00"               'echo "$OUT" | grep -q "nop \[0\] want <= 0.00 -> PASS"'
check "every trial recorded once"     '[ "$(wc -l < "$DIR/results.jsonl")" -eq 8 ]'
check "the run shipped"               'echo "$OUT" | grep -q "^.*shipped "'
check "shipped copy has the results"  '[ -f "$BUCKET/evals/smoke/smoke/results.jsonl" ]'
check "scratch did not ship"          '[ ! -d "$BUCKET/evals/smoke/smoke/tmp" ]'

step "4. re-running the same id resumes instead of repeating"
OUT2=$("$BIN" run test/smoke/smoke.yaml --runs "$RUNS" --id smoke --executor local 2>&1)
echo "$OUT2" | grep -E 'resuming|reward' | head -3 | sed 's/^/    /'
check "it noticed the finished trials" 'echo "$OUT2" | grep -q "resuming: 8 trials"'
check "it re-ran nothing"              '! echo "$OUT2" | grep -q "reward"'
check "results.jsonl did not grow"     '[ "$(wc -l < "$DIR/results.jsonl")" -eq 8 ]'

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
# Executed directly, so it has to be executable -- the same requirement any
# adapter has, and the same one-line fix.
chmod +x "$PINDIR/adapter.sh"
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
  per_case: [{uses: admission, with: {require: any}}]
  per_trial: [{uses: local}]
  per_experiment: [{uses: ship, with: {using: ship, dest: pin}}]
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
pipeline:
  per_case: [{uses: admission, with: {require: any}}]
  per_trial: [{uses: local}]
C
"$BIN" run "$PINDIR/evil.yaml" --runs "$PINDIR/runs3" --id r --executor local --commands "$PINDIR/commands.yaml" > "$PINDIR/evil.log" 2>&1
check "a submission cannot override the trusted commands" 'grep -q "declares its own kind: Commands" "$PINDIR/evil.log"'

step "13. the pipeline is a list of actions"
# Curation is the point: run it, scrub it, keep only what is worth keeping,
# pack it, publish rows rather than a directory. Each of those is a step, and a
# step that decides nothing gets published still records what was measured.
ADIR="$RUNS/actions"; rm -rf "$ADIR"; mkdir -p "$ADIR"
cat > "$ADIR/exp.yaml" <<C
---
kind: Experiment
name: actions
case_defaults: {source: local}
cases: [{path: test/smoke/pass-case}, {path: test/smoke/leaky-case}]
matrix: {agents: [oracle, nop], trials: 1}
concurrency: 2
pipeline:
  concurrency: 4
  per_case: [{uses: admission, with: {require: any}}]
  per_trial:
    - uses: local
    - uses: redact
      with: {keys: required, ips: {traj: true, logs: false}}
    - {uses: guard, name: only-hard, with: {max_reward: 0.5}, on_violation: drop}
    - {uses: archive, with: {format: zip}}
  per_experiment:
    - {uses: dataset, with: {title: "smoke rollouts"}}
    - {uses: guard, name: enough, with: {min_shipped: 2}}
C
AOUT=$("$BIN" run "$ADIR/exp.yaml" --runs "$ADIR/runs" --id a --executor local 2>&1)
echo "$AOUT" | grep -E "dropped|dataset:" | sed 's/^/    /'
ADR="$ADIR/runs/a"
check "the guard dropped what the oracle solved" 'echo "$AOUT" | grep -q "dropped by only-hard: reward = 1"'
check "a dropped trial is still measured"        'grep dropped "$ADR/results.jsonl" | grep -q \"reward\":1'
check "nop survived the guard"                   '! grep "nop" "$ADR/results.jsonl" | grep -q dropped'
check "one archive per surviving trial"          '[ "$(ls "$ADR/archives" | wc -l)" -eq 2 ]'
check "archives are zip because it asked for zip" 'ls "$ADR/archives" | grep -q "\.zip$"'
check "the dataset has a row per survivor"       '[ "$(wc -l < "$ADR/dataset/data/trials.jsonl")" -eq 2 ]'
check "dropped trials are not in the dataset"    '! grep -q oracle "$ADR/dataset/data/trials.jsonl"'
check "the card records each case's bytes"       'grep -q "$("$BIN" cases "$ADIR/exp.yaml" | head -1 | awk "{print \$2}")" "$ADR/dataset/README.md"'
check "the batch guard let it through"           '! echo "$AOUT" | grep -q "guard enough"'

# A step that is not spelled right must be caught before anything runs.
sed 's/max_reward/max_rewrad/' "$ADIR/exp.yaml" > "$ADIR/typo.yaml"
"$BIN" run "$ADIR/typo.yaml" --runs "$ADIR/runs2" --id a --executor local > "$ADIR/typo.log" 2>&1
check "a misspelled input is refused before the run" 'grep -q "unknown input" "$ADIR/typo.log"'
check "and nothing ran"                              '[ ! -f "$ADIR/runs2/a/results.jsonl" ]'

step "14. adapters are executable files, not necessarily shell scripts"
# `uses:` names an executable and the contract is "environment in, files out".
# Nothing in it says shell, so the runner executes the file directly and lets
# its shebang decide what runs it. Interpreting it with a shell here would
# force every adapter to be sh and silently ignore the one it declares.
for a in adapters/*; do
  b=$(basename "$a")
  [ -x "$a" ] || { bad "$b is executable"; continue; }
  head -1 "$a" | grep -q '^#!' || { bad "$b names its own interpreter"; continue; }
  ok "$b is executable and names its own interpreter"
done
# The portability of a shell adapter is not about sh vs bash -- it is about the
# utilities. These are the GNU-only spellings that silently do the wrong thing
# or fail outright on macOS, which is where "works on my Linux box" comes from.
NONPORTABLE='readlink -f|sha256sum|stat -c|date -d|base64 -w|grep -P|cp --parents|sort -h|sed -i +[^ ]'
check "no GNU-only utility flags in adapters" \
  '! grep -nEq "$NONPORTABLE" adapters/*'
for a in adapters/*.sh; do :; done
check "every shell adapter parses"  'for a in adapters/*.sh; do bash -n "$a" || exit 1; done'
if command -v shellcheck >/dev/null 2>&1; then
  check "shellcheck is clean" 'shellcheck -S warning adapters/*.sh'
else
  skip "shellcheck (not installed)"
fi

# And the proof that the format is not shell-shaped: a Python step.
if command -v python3 >/dev/null 2>&1; then
  NDIR="$RUNS/nonshell"; rm -rf "$NDIR"; mkdir -p "$NDIR"
  cat > "$NDIR/exp.yaml" <<C
---
kind: Commands
timeout: 5m
notify:
  uses: test/smoke/notify.py
---
kind: Experiment
name: nonshell
case_defaults: {source: local}
cases: [{path: test/smoke/pass-case}]
matrix: {agents: [oracle], trials: 1}
pipeline:
  per_trial: [{uses: local}]
  per_experiment:
    - {uses: notify, with: {channel: evals}}
C
  "$BIN" run "$NDIR/exp.yaml" --runs "$NDIR/runs" --id n --executor local > "$NDIR/out.log" 2>&1
  check "a Python adapter runs as a pipeline step" '[ -f "$NDIR/runs/n/notify.json" ]'
  check "and it was handed the run to read"        'grep -q "\"measured\": 1" "$NDIR/runs/n/notify.json"'
else
  skip "non-shell adapter (no python3)"
fi

# An adapter that cannot be executed should say which one-line fix it needs.
cp adapters/ship-local.sh "$RUNS/noexec.sh" && chmod -x "$RUNS/noexec.sh"
cat > "$RUNS/noexec.yaml" <<C
---
kind: Commands
timeout: 1m
ship: {uses: $RUNS/noexec.sh}
---
kind: Experiment
name: noexec
case_defaults: {source: local}
cases: [{path: test/smoke/pass-case}]
matrix: {agents: [oracle], trials: 1}
pipeline:
  per_trial: [{uses: local}]
  per_experiment: [{uses: ship, with: {using: ship, dest: x}}]
C
"$BIN" run "$RUNS/noexec.yaml" --runs "$RUNS/noexec-run" --id n --executor local > "$RUNS/noexec.log" 2>&1
check "a non-executable adapter names the fix" 'grep -q "chmod +x" "$RUNS/noexec.log"'

step "15. progress: how many, how far, and what is happening right now"
# A pipeline of five steps over hour-long trials used to say nothing between
# "started" and "finished". These are the three questions that silence raised.
PDIR="$RUNS/progress"; rm -rf "$PDIR"; mkdir -p "$PDIR"
cat > "$PDIR/exp.yaml" <<C
---
kind: Commands
timeout: 5m
slowstep: {script: "sleep 20"}
---
kind: Experiment
name: progress
case_defaults: {source: local}
cases: [{path: test/smoke/pass-case}, {path: test/smoke/leaky-case}]
matrix: {agents: [oracle, nop], trials: 1}
concurrency: 4
pipeline:
  concurrency: 4
  per_case: [{uses: admission, with: {require: any}}]
  per_trial:
    - uses: local
    - uses: slowstep
C
"$BIN" run "$PDIR/exp.yaml" --runs "$PDIR/runs" --id p --executor local > "$PDIR/out.log" 2>&1 &
PPID_RUN=$!
# Read the state from outside the process while it is still going -- the half
# that results.jsonl cannot answer, because those trials have not finished.
until [ -f "$PDIR/runs/p/progress.json" ]; do sleep 1; done
sleep 6
LIVE=$("$BIN" status "$PDIR/runs/p" 2>&1)
ONECASE=$("$BIN" status "$PDIR/runs/p" --case pass-case 2>&1)
wait $PPID_RUN
OUT=$(cat "$PDIR/out.log")

check "the total is known before anything finishes" 'echo "$LIVE" | grep -q "0/4 trials done"'
check "the run reports itself as running"           'echo "$LIVE" | grep -q "(running)"'
check "each case has its own denominator"           '[ "$(echo "$LIVE" | grep -cE "test/smoke/(pass|leaky)-case +[0-9]+ +2")" -eq 2 ]'
check "in-flight trials name the step they are in"  'echo "$LIVE" | grep -qE "test-smoke-.*(local|slowstep) +[0-9]+s"'
check "--case narrows to one case"                  '! echo "$ONECASE" | grep -q leaky-case'
# The heartbeat is what makes a long step visible without asking.
check "the log has a heartbeat while it waits"      'echo "$OUT" | grep -qE "[0-9]+/4 done · [0-9]+ running"'
check "the heartbeat names the step"                'echo "$OUT" | grep -q "slowstep×"'
check "and per-case progress"                       'echo "$OUT" | grep -qE "leaky-case [0-9]+/2 · pass-case [0-9]+/2"'
check "every completion carries a counter"          '[ "$(echo "$OUT" | grep -cE "^[0-9:]+ +[[][0-9]/4[]]")" -eq 4 ]'
check "cases are counted as they resolve"           'echo "$OUT" | grep -q "case 2/2 "'
check "the finished run says so"                    '"$BIN" status "$PDIR/runs/p" | grep -q "(finished)"'
# One line per command, not one per invocation: at batch scale the pin
# announcement would otherwise outnumber the results.
check "a command announces itself once"             '[ "$(echo "$OUT" | grep -c "command slowstep ->")" -le 1 ]'

printf '\n\033[1m%d passed, %d failed, %d skipped\033[0m\n' "$pass" "$fail" "$skipped"
[ "$fail" -eq 0 ]
