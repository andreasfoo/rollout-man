#!/usr/bin/env bash
# Execute the case-forge forge-flow-fuzz skill as a rollout-man trial.
#
# oracle/nop are delegated to the normal Harbor adapter so `admission` remains
# the authoritative oracle=1/nop=0 gate.  The LLM trial drives the full
# case-forge stage1 -> stage2 flow and maps its terminal decision to a reward:
# accepted -> 0.0, rejected -> 1.0.  A strict guard in the experiment therefore
# retains exactly accepted cases (0.0 < 0.6) and drops rejections.
set -euo pipefail

fail() { printf '%s\n%s\n' "$1" "$2" > "$OUT_DIR/failure.txt"; exit 1; }

case "$AGENT_KIND" in
  oracle|nop)
    exec "${ROLLOUT_MAN_HARBOR_ADAPTER:-adapters/harbor.sh}"
    ;;
  llm) ;;
  *) fail HOST_ERROR "unknown agent kind $AGENT_KIND" ;;
esac

[ "${AGENT_NAME:-}" = "forge-flow-fuzz" ] || fail HOST_ERROR "expected AGENT_NAME=forge-flow-fuzz, got ${AGENT_NAME:-}"
: "${FORGE_CASES_ROOT:?set FORGE_CASES_ROOT to the casefactory repository root}"
: "${FORGE_TASKS_ROOT:?set FORGE_TASKS_ROOT to the fuzz-harbor-task/tasks directory}"

case_id=$(basename "$CASE_DIR")
bridge=${FORGE_BRIDGE:-/home/foo/project/cyber-action/case-forge/bin/forge_bridge.py}
forge_py=${FORGE_PYTHON:-/home/foo/project/cyber-action/case-forge/.venv/bin/python3}
caseforge_dir=$(dirname "$(dirname "$bridge")")
mkdir -p "$OUT_DIR"

# Fast path: if caseforge already holds a terminal verdict for this case, skip
# the LLM call entirely. The skill cannot reverse a terminal decision, and
# re-running it wastes tokens and cloud session time for a known answer.
_pre_status=$(
  "$forge_py" "$bridge" status "$case_id" --cases-root "$FORGE_CASES_ROOT" 2>/dev/null \
    | sed -n 's/^RESULT //p' | tail -n 1 || true
)
_pre_verdict=$(printf '%s' "$_pre_status" | python3 -c '
import json,sys
x=json.loads(sys.stdin.read() or "{}")
f=x.get("final") or {}
print(f.get("verdict") or x.get("status") or "")
' 2>/dev/null || true)

if [ "$_pre_verdict" = accepted ] || [ "$_pre_verdict" = rejected ]; then
  printf 'forge-flow-fuzz: case %s already terminal (%s) -- skipping LLM run\n' \
    "$case_id" "$_pre_verdict" > "$OUT_DIR/forge-flow-fuzz.log"
  status="$_pre_status"
else
  # A caseforge case is a separate, versioned working copy. Admission is safe to
  # repeat only when it already exists; the agent receives explicit instructions
  # to use the forge-flow-fuzz skill and to stop at its stage2 terminal decision.
  prompt=$(cat <<PROMPT
Use the /forge-flow-fuzz skill to drive case ${case_id} to its terminal stage2
state. Source task: ${CASE_DIR}. Casefactory root: ${FORGE_CASES_ROOT}. If the
case is not admitted, admit that exact source task with ${bridge} first. Follow
the skill exactly: no getsrc, no codetransform, no automatic stage3. Repair
only legitimate runnability/verifier defects as permitted by the skill. A
zero-token/upstream-capacity failure is infrastructure noise, not a terminal
verdict. Finish only after terminal status is accepted or rejected and its
caseforge records (stage tag/status/triage) are persisted. Do not upload data.
Return a short terminal summary.
PROMPT
)

  # The skill itself is the policy engine: it may edit the admitted copy, run
  # Harbor trials, and writes the terminal result. The task source is read-only
  # input to this adapter.
  (
    # Running from case-forge is important: Claude discovers its project-local
    # `.claude/skills/forge-flow-fuzz` there, rather than treating the workflow
    # name in the prompt as an undocumented convention.
    cd "$caseforge_dir"
    # This is the workflow controller, not the agent being evaluated. It uses
    # the host Claude Code login and its configured `tingly/cc` alias. The actual stage1
    # and stage2 agent-under-test remains kimi-k3, exclusively configured by
    # case-forge's scheduler (.case-forge.env -> agent-kimi3.yaml). Do not read
    # or inject that credential here.
    # --add-dir takes a variable-length argument list in this Claude CLI; put
    # the prompt before it so it cannot be swallowed as another directory.
    claude -p --model tingly/cc --dangerously-skip-permissions \
      "$prompt" \
      --add-dir "$FORGE_CASES_ROOT" \
      --add-dir "$FORGE_TASKS_ROOT"
  ) >"$OUT_DIR/forge-flow-fuzz.log" 2>&1 || true
  # Keep controller diagnostics but remove accidental credential echoes before
  # rollout-man retains this log as an artifact.
  python3 -c 'import re,sys; p=sys.argv[1]; s=open(p,encoding="utf-8",errors="replace").read(); s=re.sub(r"(?im)^(ANTHROPIC_(?:API_KEY|AUTH_TOKEN)=).*$", r"\1[REDACTED]", s); s=re.sub(r"(?i)(tingly-box-)[A-Za-z0-9._-]+", r"\1[REDACTED]", s); open(p,"w",encoding="utf-8").write(s)' "$OUT_DIR/forge-flow-fuzz.log"

  status=$(
    "$forge_py" "$bridge" status "$case_id" --cases-root "$FORGE_CASES_ROOT" 2>>"$OUT_DIR/forge-flow-fuzz.log" \
      | sed -n 's/^RESULT //p' | tail -n 1 || true
  )
fi
[ -n "$status" ] || fail ENV_FAILED "forge-flow-fuzz completed without a readable caseforge status"
printf '%s\n' "$status" > "$OUT_DIR/terminal.json"

verdict=$(python3 - "$OUT_DIR/terminal.json" <<'PY'
import json, sys
x = json.load(open(sys.argv[1]))
f = x.get("final") or {}
print(f.get("verdict") or x.get("status") or "")
PY
)

case_copy="$FORGE_CASES_ROOT/cases/$case_id"
case "$verdict" in
  accepted)
    [ -d "$case_copy" ] || fail ENV_FAILED "accepted terminal state has no case directory"
    # The complete accepted case is the published case artifact. Trajectories
    # remain inside it for provenance, and are separately materialized later.
    cp -a "$case_copy" "$OUT_DIR/case"
    printf '0.0\n' > "$OUT_DIR/reward.txt"
    ;;
  rejected)
    printf '1.0\n' > "$OUT_DIR/reward.txt"
    ;;
  *)
    fail ENV_FAILED "forge-flow-fuzz ended non-terminally (status=$verdict)"
    ;;
esac
