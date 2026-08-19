#!/usr/bin/env bash
# A stand-in for Harbor, and the reference for what a trial adapter has to do.
#
# rollout-man hands over a case, an agent and a place to put the answer, then
# stays out of the way. Everything below -- how the environment is built, who
# the agent runs as, when it is killed -- is the adapter's business, which is
# the whole point of the split: rollout-man does not run containers.
#
#   in   CASE_DIR OUT_DIR TRIAL_ID AGENT_KIND AGENT_NAME AGENT_COMMAND
#        AGENT_USER VERIFIER_USER AGENT_TIMEOUT_SEC VERIFIER_TIMEOUT_SEC
#        BUILD_TIMEOUT_SEC CPUS MEMORY_MB ALLOW_INTERNET LLM_BASE_URL
#        LLM_MODEL LLM_API_KEY
#   out  $OUT_DIR/reward.txt   the score -- the only thing that means "measured"
#        $OUT_DIR/failure.txt  a failure code, when the adapter knows why
#        $OUT_DIR/*            artifacts to keep (traj.jsonl, logs)
#
# A real adapter would be `harbor run --case "$CASE_DIR" ...`. This one runs the
# case scripts in a private mount namespace so the smoke test needs no Harbor.
set -uo pipefail

fail() { printf '%s\n%s\n' "$1" "$2" > "$OUT_DIR/failure.txt"; exit 1; }

# Fault injection, for the smoke test only.
case "${FAKE_HARBOR_FAIL:-}" in
  declared) fail AGENT_TIMEOUT "the agent outran its clock" ;;
  silent)   echo "adapter died and did not say why" >&2; exit 9 ;;
esac

# Record what we were told, so the test can assert the contract holds.
env | grep -E '^(CASE_DIR|OUT_DIR|TRIAL_ID|AGENT_KIND|AGENT_NAME|AGENT_USER|VERIFIER_USER|AGENT_TIMEOUT_SEC|VERIFIER_TIMEOUT_SEC|BUILD_TIMEOUT_SEC|CPUS|MEMORY_MB|ALLOW_INTERNET|LLM_MODEL)=' | sort > "$OUT_DIR/adapter-env.txt"

ROOT=$(mktemp -d); trap 'rm -rf "$ROOT"' EXIT
mkdir -p "$ROOT"/app/seed "$ROOT"/logs/agent "$ROOT"/logs/verifier "$ROOT"/solution
cp -r "$CASE_DIR/solution/." "$ROOT/solution/" 2>/dev/null
cp -r "$CASE_DIR/environment/seed/." "$ROOT/app/seed/" 2>/dev/null

# One sandbox for the whole trial: the agent leaves its deliverable in /app and
# the verifier reads it back, so both steps have to see the same filesystem.
step() { # step <shell-command> <timeout-seconds>
  unshare --mount --propagation private bash -c "
    mkdir -p /app /logs /solution /tests
    mount --bind '$ROOT/app' /app
    mount --bind '$ROOT/logs' /logs
    mount --bind '$ROOT/solution' /solution
    mount --bind '$CASE_DIR/tests' /tests
    cd /app
    exec timeout -k 5 '$2' bash -lc '$1'"
}

case "$AGENT_KIND" in
  nop) echo "nop: no action taken" > "$OUT_DIR/stdout.log" ;;
  oracle)
    step /solution/solve.sh "$AGENT_TIMEOUT_SEC" > "$OUT_DIR/stdout.log" 2>&1
    rc=$?
    [ "$rc" -eq 124 ] && fail AGENT_TIMEOUT "oracle exceeded ${AGENT_TIMEOUT_SEC}s"
    [ "$rc" -ne 0 ] && fail AGENT_FAILED "oracle exited $rc" ;;
  llm)
    [ -n "$AGENT_COMMAND" ] || fail ENV_FAILED "no agent command configured for $AGENT_NAME"
    LLM_BASE_URL="$LLM_BASE_URL" LLM_MODEL="$LLM_MODEL" LLM_API_KEY="$LLM_API_KEY" \
      step "$AGENT_COMMAND" "$AGENT_TIMEOUT_SEC" > "$OUT_DIR/stdout.log" 2>&1
    rc=$?
    [ "$rc" -eq 124 ] && fail AGENT_TIMEOUT "agent exceeded ${AGENT_TIMEOUT_SEC}s"
    [ "$rc" -ne 0 ] && fail AGENT_FAILED "agent exited $rc" ;;
  *) fail HOST_ERROR "unknown agent kind $AGENT_KIND" ;;
esac

step /tests/test.sh "$VERIFIER_TIMEOUT_SEC" > "$OUT_DIR/verifier.stdout.log" 2>&1
rc=$?
[ "$rc" -eq 124 ] && fail VERIFIER_ERROR "verifier exceeded ${VERIFIER_TIMEOUT_SEC}s"

for f in logs/agent/agent.log logs/agent/traj.jsonl logs/verifier/verifier.log logs/verifier/reward.txt; do
  [ -f "$ROOT/$f" ] && cp "$ROOT/$f" "$OUT_DIR/$(basename "$f")"
done
[ -f "$OUT_DIR/traj.jsonl" ] || : > "$OUT_DIR/traj.jsonl"
[ -f "$OUT_DIR/reward.txt" ] || fail VERIFIER_ERROR "verifier exited $rc without writing a reward"
exit 0
