#!/usr/bin/env bash
# Real kimi-k3 rollout adapter (trajectory generation campaign, 2026-08-31).
#
# Unlike forge-flow-fuzz-mock (which only replays existing jobs/), this runs a
# LIVE harbor trial of the case with kimi-k3 via the tingly proxy. The agent
# is ClaudeCodeBinThirdParty (agent-ccbin-kimi3.yaml): harbor uploads a bundled
# native claude binary to /usr/bin/claude (a cp), so the container needs ZERO
# egress for agent setup -- only the LLM proxy is allowlisted. This replaces
# the old ClaudeCodeThirdParty (agent-kimi3.yaml), which bootstrapped
# node/nvm/npm-installed claude inside the container at runtime; that setup
# path intermittently surfaced as HOST_ERROR/UnknownApiError and lost whole
# trials (2026-09-03 mruby-431d4bb). The yaml is the single source of truth
# for the agent (model, base URL, key, egress allowlist) -- this adapter never
# reads or injects the credential itself.
#
# Cases are pre-accepted: no stage1/stage2 skill lifecycle, no caseforge
# mutation -- just one fresh solve attempt whose trajectory is the product.
# The case directory is snapshotted (same tar filter as adapters/harbor.sh) so
# nothing is written into the watched tc_batch2 tree while watch (PID-gated on
# content hash) is live.
#
# Harbor writes the trial under $JOBS/$TRIAL_ID/<case>__<suffix>/. The adapter
# copies the whole job dir into $OUT_DIR/case/trials/<trial_name>/ -- the exact
# layout materialize_week2_trial walks (result.json with trial_name+started_at
# at the trial root).
#
# ENV (from the harbor executor contract):
#   CASE_DIR OUT_DIR WORK_DIR TRIAL_ID AGENT_KIND AGENT_NAME
set -euo pipefail

fail() { printf '%s\n%s\n' "$1" "$2" > "$OUT_DIR/failure.txt"; exit 1; }

# OUT_DIR can arrive relative (default --runs is a relative "runs"); this
# adapter cd's to $HARBOR_CWD before writing anything else under OUT_DIR, so
# absolutize first or every later redirect (harbor.log, failure.txt) misses
# (2026-08-31: full-campaign launch died 189/189 with "harbor.log: No such
# file or directory"; the smokes survived only because --runs was /tmp/...).
OUT_DIR=$(mkdir -p "$OUT_DIR" && cd "$OUT_DIR" && pwd)
WORK_DIR=$(cd "$WORK_DIR" && pwd)

# Absolute path of this adapter's directory, captured before the cd to
# $HARBOR_CWD so the API-error gate (a sibling script) stays reachable.
SELF_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)

case "$AGENT_KIND" in
  oracle|nop)
    exec "${ROLLOUT_MAN_HARBOR_ADAPTER:-adapters/harbor.sh}"
    ;;
  llm) ;;
  *) fail HOST_ERROR "unknown agent kind $AGENT_KIND" ;;
esac

[ "${AGENT_NAME:-}" = "forge-flow-fuzz" ] || \
  fail HOST_ERROR "expected AGENT_NAME=forge-flow-fuzz, got ${AGENT_NAME:-}"

HARBOR_CONFIG="${KIMI3_HARBOR_CONFIG:-/home/foo/project/casefactory-goloopx/harbor-config/agent-ccbin-kimi3.yaml}"
HARBOR_CWD="${KIMI3_HARBOR_CWD:-/home/foo/project/cyber-gate}"
[ -f "$HARBOR_CONFIG" ] || fail ENV_FAILED "harbor config not found: $HARBOR_CONFIG"
# The agent class must be importable with $HARBOR_CWD on sys.path (see the
# PYTHONPATH export below). agent-ccbin-kimi3.yaml uses
# `import_path: cyber_harbor:ClaudeCodeBinThirdParty` -- a package under the
# cyber-gate root, not a root-level module file -- so probe importability of
# the package rather than stat'ing a specific .py. (The prior npm agent,
# agent_cc_third_party:ClaudeCodeThirdParty, lived as a single .py at the
# casefactory root; an override pointing back at it still passes this check
# because that root is then on sys.path and the module imports.)
#
# The probe MUST run under harbor's own interpreter, not whatever `python3`
# PATH resolves to. `cyber_harbor` imports `harbor`, and harbor is installed
# as a uv tool in its own venv (~/.local/share/uv/tools/harbor); the ambient
# python3 here is an unrelated 3.11 that has never had harbor on its path.
# Probing with it raises ModuleNotFoundError('harbor') and fails the trial
# ENV_FAILED before harbor is ever invoked -- a pure false negative, since
# the class imports cleanly under the interpreter that actually runs it
# (2026-09-03: spidermonkey-6d489cb died this way immediately after being
# admitted, and every case would have followed). Derive the interpreter from
# harbor's shebang so the probe tests the real import environment.
HARBOR_BIN=$(command -v harbor) || fail ENV_FAILED "harbor not on PATH"
HARBOR_PY=$(sed -n '1s/^#!//p' "$HARBOR_BIN")
[ -x "$HARBOR_PY" ] || fail ENV_FAILED "could not derive harbor interpreter from $HARBOR_BIN (got '${HARBOR_PY:-}')"
PYTHONPATH="$HARBOR_CWD${PYTHONPATH:+:$PYTHONPATH}" "$HARBOR_PY" - "$HARBOR_CONFIG" <<'PY' || fail ENV_FAILED "agent import_path not importable from $HARBOR_CWD (see stderr)"
import importlib, re, sys
cfg = open(sys.argv[1]).read()
m = re.search(r'import_path:\s*([\w.]+):(\w+)', cfg)
if not m:
    sys.exit("no import_path in harbor config")
mod, cls = m.groups()
getattr(importlib.import_module(mod), cls)
PY

JOBS="$WORK_DIR/harbor"
CASE_SNAPSHOT="$WORK_DIR/case"
# Leftovers from a killed attempt can contain CONTAINER-OWNED files (harbor
# agents write session state as the container's uid, undeletable by foo), and
# under set -e the rm failure killed the trial with no failure.txt
# (2026-08-31: clickhouse-e9a9df4-3 ENV_FAILED). Rename undeleatable dirs
# aside instead -- mv needs only write permission on the parent, which is
# ours. OUT_DIR/case is cleaned for the same reason AND so a retry's
# materialize never walks a killed attempt's stale trial dir (a truncated
# stale copy would fail the API-error gate and block the fresh trial's ship).
rm -rf "$JOBS" "$CASE_SNAPSHOT" "$OUT_DIR/case" 2>/dev/null || true
for stale in "$JOBS" "$CASE_SNAPSHOT" "$OUT_DIR/case"; do
  if [ -e "$stale" ]; then
    mv "$stale" "$stale.stale-$$" 2>/dev/null || true
  fi
done
mkdir -p "$JOBS" "$CASE_SNAPSHOT"

rm -f "$OUT_DIR/failure.txt" "$OUT_DIR/reward.txt"
mkdir -p "$OUT_DIR"

# Isolated definition snapshot: harbor hashes --path, and the source case dir
# carries mutable .factory/ trials/ jobs/ state (sometimes container-owned)
# that would break hashing -- and must never be written into anyway (watch).
tar --exclude='./.git' --exclude='./.factory' --exclude='./trials' --exclude='./jobs' \
  -C "$CASE_DIR" -cf - . | tar -C "$CASE_SNAPSHOT" -xf - ||
  fail HOST_ERROR "could not snapshot case definition"

# Same shape case_forge/trial.py builds. start_new_session keeps harbor's
# subtree killable without touching this adapter's own group; harbor enforces
# the agent/verifier timeouts from the snapshot's task.toml itself.
#
# CLAUDE_CODE_PROMPT_VIA_STDIN=1 makes the agent module pipe the task prompt
# into `claude --print` over stdin instead of argv. Daemon-style prompts name
# the vulnerable binary (/opt/vuln/<target>), and `pkill -f /opt/vuln/<target>`
# -- the agent's natural way to restart a hung daemon -- matches FULL command
# lines, claude's own included, SIGTERM-ing the CLI mid-attempt (exit 143;
# 2026-08-31 memcached smoke died this way 20s after finding the crash). Both
# the npm agent (casefactory/agent_cc_third_party.py) and the bin agent
# (cyber_harbor:ClaudeCodeBinThirdParty) honor this env flag; the env pass
# below reaches whichever one the active HARBOR_CONFIG selects.
#
# The agent module import: agent-ccbin-kimi3.yaml says
# `import_path: cyber_harbor:ClaudeCodeBinThirdParty`. Harbor's plain-import
# sys.path order is bin-dir, PYTHONPATH, site-packages, and the
# `cyber_harbor` package itself is installed via a venv .pth file at
# site-packages/zz_cyber_gate_local.pth pointing at /home/foo/project/cyber-gate
# (the .pth is the durable import fix; a harbor reinstall/venv wipe loses it
# and must be re-created). PYTHONPATH is exported here too as belt-and-suspenders:
# PYTHONPATH outranks site-packages, so a venv that DOES have cyber_harbor on
# its .pth AND in $HARBOR_CWD always picks $HARBOR_CWD's copy (the one the
# active yaml's env contract is written against, and the one carrying the
# stdin-prompt fix). This makes $HARBOR_CWD authoritative.
export PYTHONPATH="$HARBOR_CWD${PYTHONPATH:+:$PYTHONPATH}"

cd "$HARBOR_CWD"
# --allow-agent-host injects the tingly LLM proxy CIDR into the agent-phase
# egress allowlist AT RUNTIME, so the published task.toml can carry an empty
# allowed_hosts (the materializer normalizes it to []): the proxy IP is a
# local-network secret and must never be written into a shipped file. The
# case's own task.toml sets network_mode="allowlist" with allowed_hosts=[];
# this flag (a run-specific CIDR harbor merges into that allowlist) is what
# lets kimi reach ANTHROPIC_BASE_URL at 143.89.191.124 during agent.run().
# 143.89.0.0/16 (not a bare host) matches the active agent yaml's
# extra_allowed_hosts -- a CIDR, since wildcards in harbor are hostname-only
# (*.domain).
setsid harbor run \
  -c "$HARBOR_CONFIG" \
  -p "$CASE_SNAPSHOT" \
  --job-name "$TRIAL_ID" \
  --jobs-dir "$JOBS" \
  --n-concurrent 1 \
  --allow-agent-host 143.89.0.0/16 \
  --agent-env CLAUDE_CODE_PROMPT_VIA_STDIN=1 \
  --agent-kwarg disallowed_tools=WebSearch,WebFetch \
  --yes --quiet > "$OUT_DIR/harbor.log" 2>&1 || rc=$?
rc="${rc:-0}"

# Locate the trial dir harbor created (<case>__<random> under the job name).
trial=$(find "$JOBS/$TRIAL_ID" -mindepth 1 -maxdepth 1 -type d -name '*__*' 2>/dev/null | head -1)
if [ -z "$trial" ]; then
  fail ENV_FAILED "harbor run exited $rc and produced no trial directory (see harbor.log)"
fi

# Build the materializer's expected layout: OUT_DIR/case/trials/<trial_name>/.
# The job dir itself IS the trial dir (result.json + agent/ + verifier/).
# The claude CLI inside the container writes its session jsonl as the
# container's agent uid (host uid 1000) mode 600 across harbor's bind
# mount; when this adapter's uid differs (foo=1007 here) cp -a dies with
# "Permission denied" and the whole trial becomes a bogus ENV_FAILED
# (mupdf-e4693b0 forge-flow-fuzz-1, 2026-09-03 04:40). We are in the
# docker group: a throwaway root container chown's the trial tree back
# to the invoking uid so the copy (and every later gate reading it)
# succeeds. Best-effort: without docker the copy proceeds and surfaces
# its own error if it truly cannot read the tree.
trial_name=$(basename "$trial")
docker run --rm -v "$JOBS/$TRIAL_ID:/j:ro" alpine:3 sh -c \
    "chown -R $(id -u):$(id -g) /j" 2>/dev/null || true
mkdir -p "$OUT_DIR/case/trials"
cp -a "$trial" "$OUT_DIR/case/trials/$trial_name"

# Score from harbor's own result: an exception or missing reward is a failure
# code for rollout-man's taxonomy; a reward means measured. There is no
# accept/reject verdict here -- every completed rollout is a publishable
# trajectory (the dataset convention for this campaign: reward < 0.6 keeps
# the case interesting; the guard step decides, not this adapter).
python3 - "$trial/result.json" "$OUT_DIR" <<'PY'
import json, pathlib, sys

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
HOST = {
    "ApiRateLimitError", "ApiOverloadedError", "ApiConnectionError",
    "ApiConnectionClosedError", "ApiInternalServerError", "ApiResponseStalledError",
    "NetworkConnectionError", "UnknownApiError", "RuntimeRequestError",
}

res, out = json.loads(pathlib.Path(sys.argv[1]).read_text()), pathlib.Path(sys.argv[2])
exc = res.get("exception_info") or {}
kind = exc.get("exception_type", "")
rewards = (res.get("verifier_result") or {}).get("rewards") or {}

# Harbor verifies the end state after an AgentTimeoutError, so "timeout" is
# the task not fitting the budget, not a failed rollout: the reward wins over
# the timeout exception (2026-09-01: all 31 stale AGENT_TIMEOUT rows had
# rewards; one even 1.0). Every OTHER exception kind stays a failure even
# when a reward exists -- a scored end state does not make a self-killed
# agent or a rate-limited rollout a normal one.
if "reward" in rewards and (not kind or kind in AGENT_TIMEOUT):
    if kind:
        print(f"note: {kind} but verifier scored the end state; keeping reward",
              file=sys.stderr)
    (out / "reward.txt").write_text(f"{rewards['reward']}\n")
    sys.exit(0)

if kind:
    code = ("AGENT_TIMEOUT" if kind in AGENT_TIMEOUT else
            "AGENT_FAILED" if kind in AGENT_FAILED else
            "VERIFIER_ERROR" if kind in VERIFIER else
            "HOST_ERROR" if kind in HOST else "ENV_FAILED")
    msg = (exc.get("exception_message") or "").strip().splitlines()
    (out / "failure.txt").write_text(f"{code}\n{kind}: {msg[0] if msg else ''}\n")
    sys.exit(1)

(out / "failure.txt").write_text(
    "VERIFIER_ERROR\nharbor reported no exception and no reward\n")
sys.exit(1)
PY

# Trajectory API-health gate (2026-08-31, relaxed 2026-09-02): harbor's
# result.json only surfaces TERMINAL API errors as exceptions. A run whose
# claude CLI retried through 429s/overloads and kept going may still carry
# the noise in its transcript. The gate fails UNREWARDED trials on that
# noise so attempts() re-runs them; a trial that survived to a verifier
# reward -- including a timeout killed at the cap and then scored -- is a
# finished, measured trajectory and passes (check-no-api-error-traj's
# reward waiver). (The per_trial check_no_api_error_traj gate is the
# ship-side backstop, same waiver.)
if [ -f "$OUT_DIR/reward.txt" ]; then
  TRAJ_DIR="$OUT_DIR/case/trials/$trial_name" "$SELF_DIR/check-no-api-error-traj.sh" ||
    fail HOST_ERROR "trajectory failed the API-error check (429/overload/truncation); retrying the trial"
fi

# Keep the trajectory even for a failing reward: the campaign's product is the
# kimi-k3 attempt itself. Guard (<0.6) drops only the SHIP, not the artifact,
# so reward.txt above is all the pipeline needs from this step.
exit $?
