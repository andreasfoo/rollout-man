#!/usr/bin/env bash
# The Harbor adapter: rollout-man's trial contract on top of `harbor run`.
#
# Harbor owns how a trial runs -- it builds the environment, starts it, runs the
# agent inside it, runs the verifier, and enforces every timeout and resource
# limit declared in the case's own task.toml. Nothing here re-decides any of
# that; this script translates one call and one answer.
#
#   in   CASE_DIR OUT_DIR WORK_DIR TRIAL_ID AGENT_KIND AGENT_NAME
#        LLM_BASE_URL LLM_MODEL LLM_API_KEY
#   out  $OUT_DIR/reward.txt   the score
#        $OUT_DIR/failure.txt  first line a failure code, rest an explanation
#        $OUT_DIR/*            artifacts kept from the trial
#
# Needs `harbor` (pip install harbor) and python3 on PATH.
set -uo pipefail

fail() { printf '%s\n%s\n' "$1" "$2" > "$OUT_DIR/failure.txt"; exit 1; }

JOBS="$WORK_DIR/harbor"
rm -rf "$JOBS"; mkdir -p "$JOBS"

agent=()
case "$AGENT_KIND" in
  oracle) agent=(-a oracle) ;;
  nop)    agent=(-a nop) ;;
  llm)
    # AGENT_NAME is a Harbor agent id (claude-code, codex, openhands, ...).
    agent=(-a "$AGENT_NAME")
    [ -n "${LLM_MODEL:-}" ] && agent+=(-m "$LLM_MODEL")
    # Which env var the key belongs in is the provider's business, not ours.
    case "${LLM_PROVIDER:-openai}" in
      anthropic) key_var=ANTHROPIC_API_KEY; url_var=ANTHROPIC_BASE_URL ;;
      google|gemini) key_var=GEMINI_API_KEY; url_var=GOOGLE_GEMINI_BASE_URL ;;
      *) key_var=OPENAI_API_KEY; url_var=OPENAI_BASE_URL ;;
    esac
    [ -n "${LLM_BASE_URL:-}" ] && agent+=(--ae "$url_var=$LLM_BASE_URL")
    if [ -n "${LLM_API_KEY:-}" ]; then
      agent+=(--ae "$key_var=$LLM_API_KEY")
    else
      # Fail here rather than let Harbor start a container that cannot talk to
      # anything: an agent with no key is a setup mistake, not a measurement.
      fail ENV_FAILED "no API key for $AGENT_NAME (llm_spec resolved to an empty key)"
    fi ;;
  *) fail HOST_ERROR "unknown agent kind $AGENT_KIND" ;;
esac

# -k 1 / -n 1: one attempt, one trial. Repetition and retries are the run's
# decision, made from the failure code, and doing them here would run the agent
# more times than the results file admits to.
harbor run \
  --path "$CASE_DIR" \
  "${agent[@]}" \
  --jobs-dir "$JOBS" \
  --job-name "$TRIAL_ID" \
  -k 1 -n 1 --max-retries 0 \
  --yes --quiet > "$OUT_DIR/harbor.log" 2>&1
rc=$?

trial=$(find "$JOBS/$TRIAL_ID" -mindepth 1 -maxdepth 1 -type d -name '*__*' | head -1)
if [ -z "$trial" ]; then
  fail ENV_FAILED "harbor run exited $rc and produced no trial directory"
fi

# Artifacts first: a trial you cannot read is a trial you cannot fix, and the
# failed ones are the ones worth reading.
cp "$trial/verifier/verifier.log"     "$OUT_DIR/verifier.log"  2>/dev/null
cp "$trial/verifier/test-stdout.txt"  "$OUT_DIR/stdout.log"    2>/dev/null
cp "$trial/trial.log"                 "$OUT_DIR/trial.log"     2>/dev/null
cp -r "$trial/agent"                  "$OUT_DIR/agent"         2>/dev/null
[ -f "$OUT_DIR/traj.jsonl" ] || : > "$OUT_DIR/traj.jsonl"

python3 - "$trial/result.json" "$OUT_DIR" <<'PY'
import json, pathlib, sys

# Harbor's exception types, mapped onto the four things the taxonomy needs to
# tell apart. Only the first group belongs in an agent's denominator; the rest
# mean "we could not measure", which is ours and not the agent's.
AGENT_TIMEOUT = {"AgentTimeoutError"}
AGENT_FAILED = {
    "NonZeroAgentExitCodeError", "AgentSafetyRefusalError",
    "ContextLengthExceededError", "ContextWindowExceededError",
    "OutputLengthExceededError", "OutputTokenExceededError",
}
VERIFIER = {
    "VerifierTimeoutError", "VerifierOutputParseError", "RewardFileNotFoundError",
    "RewardFileEmptyError", "RegradeError", "DownloadVerifierDirError",
    "AddTestsDirError",
}
# Transient and worth another go; everything unrecognised falls through to
# ENV_FAILED instead, which is the conservative reading.
HOST = {
    "ApiRateLimitError", "ApiOverloadedError", "ApiConnectionError",
    "ApiConnectionClosedError", "ApiInternalServerError", "ApiResponseStalledError",
    "NetworkConnectionError", "UnknownApiError", "RuntimeRequestError",
}

res, out = json.loads(pathlib.Path(sys.argv[1]).read_text()), pathlib.Path(sys.argv[2])
exc = res.get("exception_info") or {}
kind = exc.get("exception_type", "")
rewards = (res.get("verifier_result") or {}).get("rewards") or {}

if kind:
    code = ("AGENT_TIMEOUT" if kind in AGENT_TIMEOUT else
            "AGENT_FAILED" if kind in AGENT_FAILED else
            "VERIFIER_ERROR" if kind in VERIFIER else
            "HOST_ERROR" if kind in HOST else "ENV_FAILED")
    msg = (exc.get("exception_message") or "").strip().splitlines()
    (out / "failure.txt").write_text(f"{code}\n{kind}: {msg[0] if msg else ''}\n")
    sys.exit(1)

if "reward" not in rewards:
    (out / "failure.txt").write_text(
        "VERIFIER_ERROR\nharbor reported no exception and no reward\n")
    sys.exit(1)

(out / "reward.txt").write_text(f"{rewards['reward']}\n")
PY
exit $?
