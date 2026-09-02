#!/usr/bin/env bash
# API-failure gate for trajectory trees (2026-08-31).
#
# A trial whose claude CLI fought through 429s / overloads / connection drops
# can still "succeed": harbor's result.json only surfaces TERMINAL API errors
# as exceptions (ApiRateLimitError & co, handled in kimi3-rollout.sh). What
# ships from such a run is a poisoned or truncated transcript -- bad data for
# a trajectory dataset. This gate parses the stream-json transcripts
# (claude-code.txt, session *.jsonl) event by event and fails on any real
# API-failure shape:
#   - system event with an error subtype (api_error)
#   - assistant message flagged isApiErrorMessage
#   - assistant TEXT naming an API failure ("API Error", "rate_limit",
#     "Overloaded", "429 Too Many") -- assistant text only, never tool
#     results: target programs legitimately log 429s and "connection reset",
#     and source code is full of line-number 429s (2026-08-31 sweep: 11
#     trials matched naive greps, 0 were real)
#   - result event with subtype != "success"
#   - claude-code.txt with events but NO result event at all (truncated
#     stream: the CLI died mid-run -- kill, timeout, or network death)
#
# Recovered-429 policy (2026-09-02): if the run SURVIVED its API errors --
# harbor ran the verifier over the end state and wrote a reward
# (verifier/reward.txt or verifier/reward.json anywhere under TRAJ_DIR) --
# the trajectory is finished and scored, and the 429/overload noise in its
# transcript is waived (reported to stderr, exit 0). That includes timeout
# trials: harbor kills the CLI at the agent timeout (no result event, the
# "truncated" shape) and THEN verifies, so a rewarded trial with a
# result-less claude-code.txt is a normal trial, not a dead one. Only
# UNREWARDED runs -- where the API failure killed the trial before scoring
# -- stay blocked.
#
# Two call sites:
#   - per_trial gate over the materialized trajectory tree (same TRAJ_DIR
#     contract as check_no_chinese_traj), on_failure: skip -> never ships
#   - executor-level retry hook in kimi3-rollout.sh, where its failure is
#     reported as HOST_ERROR so attempts() re-runs the whole trial under
#     max_attempts -- "429-poisoned trials get rerun", automatically
#
# ENV (set by rollout-man):
#   TRAJ_DIR   the trajectory tree to scan
set -euo pipefail

: "${TRAJ_DIR:?check-no-api-error-traj needs TRAJ_DIR}"

python3 - "$TRAJ_DIR" <<'PY'
import json, os, re, sys

traj = sys.argv[1]
ASSISTANT_ERR = re.compile(r"API Error|rate_limit|Overloaded|429 Too Many")

def scan(path, require_result):
    """Return the list of API-failure shapes found in one transcript."""
    problems = []
    saw_event = saw_result = False
    with open(path, errors="replace") as fh:
        for lineno, line in enumerate(fh, 1):
            line = line.strip()
            if not line:
                continue
            try:
                ev = json.loads(line)
            except json.JSONDecodeError:
                continue  # half-written tail; the no-result check catches the truncation
            if not isinstance(ev, dict):
                continue
            saw_event = True
            t = ev.get("type")
            if t == "system" and "error" in str(ev.get("subtype", "")):
                problems.append(f"line {lineno}: system/{ev.get('subtype')}")
            elif t == "assistant":
                msg = ev.get("message") or {}
                if msg.get("isApiErrorMessage") or ev.get("isApiErrorMessage"):
                    problems.append(f"line {lineno}: isApiErrorMessage")
                for c in msg.get("content") or []:
                    if isinstance(c, dict) and c.get("type") == "text":
                        m = ASSISTANT_ERR.search(c.get("text") or "")
                        if m:
                            problems.append(f"line {lineno}: assistant text names {m.group(0)!r}")
                            break
            elif t == "result":
                saw_result = True
                st = ev.get("subtype", "")
                if st != "success":
                    problems.append(f"line {lineno}: result subtype {st!r}")
    if require_result and saw_event and not saw_result:
        problems.append("stream ends without a result event (truncated: agent died mid-run)")
    return problems

# A verifier reward under TRAJ_DIR means the trial survived whatever API
# noise is in its transcript: harbor only scores the end state after the
# agent phase terminated, so the trajectory is finished and measured.
rewarded = any(
    fn in ("reward.txt", "reward.json")
    for root, _, files in os.walk(traj) for fn in files
)

blocked = []
scanned = 0
for root, dirs, files in os.walk(traj):
    for fn in sorted(files):
        # claude-code.txt is the CLI stdout capture: a successful run always
        # ends it with a result event, so truncation is detectable there.
        # Session *.jsonl get shape checks only -- their result-event
        # presence is not guaranteed by the format.
        if fn == "claude-code.txt":
            require_result = True
        elif fn.endswith(".jsonl"):
            require_result = False
        else:
            continue
        p = os.path.join(root, fn)
        try:
            probs = scan(p, require_result)
        except OSError:
            continue  # unreadable (e.g. container-owned sessions dir)
        scanned += 1
        for prob in probs:
            blocked.append(f"{os.path.relpath(p, traj)}: {prob}")

if blocked and rewarded:
    print(f"check_no_api_error_traj: ok ({traj}) -- verifier reward present; "
          f"waiving {len(blocked)} API-error shape(s) the trial survived:", file=sys.stderr)
    for b in blocked[:10]:
        print(f"  waived: {b}", file=sys.stderr)
    sys.exit(0)

if blocked:
    print(f"check_no_api_error_traj: FAIL -- API-failure shapes in {len(blocked)} place(s):", file=sys.stderr)
    for b in blocked[:10]:
        print(f"  {b}", file=sys.stderr)
    sys.exit(1)

print(f"check_no_api_error_traj: ok ({traj}) -- {scanned} transcript(s), no API-failure shapes")
PY
