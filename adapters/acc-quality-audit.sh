#!/usr/bin/env bash
# Per-case security audit: acc-quality-audit (11-point verifier-trust-boundary).
#
# Invokes the cyber-gate acc-quality-audit skill via `claude -p` (subagent).
# The skill reads the case files statically and checks 11 verifier-trust-boundary
# categories (S1-S11: candidate code isolation, privilege drop, self-report,
# mutable oracle, fd forgery, PoC compile bypass, etc.).
#
# Verdict mapping:
#   PROMOTE               → exit 0  (all clean)
#   PROMOTE-WITH-WARNINGS → exit 0  (medium-only issues, warning printed)
#   DO NOT PROMOTE        → exit 1  (critical/high/direct_removal FAIL, blocks admission)
#
# Report is saved to $RUN_DIR/quality/<case-label>.txt for later inspection.
# Structured result is written to $ROLLOUT_MAN_OUTPUT (verdict, report, blocking)
# so a later pipeline step can read it via {{steps.<label>.outputs.<key>}}
# instead of re-parsing the report text.
#
# ENV (all passed through rollout-man):
#   CASE_DIR    path to the case package
#   CASE_LABEL  human-readable label for the case
#   RUN_DIR     rollout-man run directory (for report output)
#   CYBER_GATE_ROOT  optional; defaults to /home/foo/project/cyber-gate
#   LLM_BASE_URL LLM_MODEL LLM_API_KEY  optional; set when this command
#                declares llm_spec: in kind: Commands. Mapped onto the
#                ANTHROPIC_* names `claude -p` itself reads, same as
#                adapters/harbor.sh maps LLM_* onto the right provider's
#                names for a trial's agent -- an audit subagent's endpoint is
#                a submission setting, not whatever happens to be ambient.
#                Unset, the subagent falls through to the ambient
#                ANTHROPIC_BASE_URL/ANTHROPIC_AUTH_TOKEN/ANTHROPIC_MODEL.
set -euo pipefail

[ -n "${LLM_BASE_URL:-}" ] && export ANTHROPIC_BASE_URL="$LLM_BASE_URL"
[ -n "${LLM_API_KEY:-}" ] && export ANTHROPIC_AUTH_TOKEN="$LLM_API_KEY"
[ -n "${LLM_MODEL:-}" ] && export ANTHROPIC_MODEL="$LLM_MODEL"

emit() { [ -n "${ROLLOUT_MAN_OUTPUT:-}" ] && printf '%s=%s\n' "$1" "$2" >> "$ROLLOUT_MAN_OUTPUT" || true; }

SKILL_FILE="${CYBER_GATE_ROOT:-/home/foo/project/cyber-gate}/.claude/skills/acc-quality-audit/SKILL.md"

[ -f "$SKILL_FILE" ] || {
  printf 'acc-quality-audit: FAIL %s -- skill file not found: %s\n' "$CASE_LABEL" "$SKILL_FILE" >&2
  emit verdict ERROR
  exit 1
}

mkdir -p "$RUN_DIR/quality"
slug=$(basename "$CASE_LABEL")
report_file="$RUN_DIR/quality/${slug}.txt"
stream_file="$RUN_DIR/quality/${slug}.stream.jsonl"

prompt="Read the case package at ${CASE_DIR} and audit it against the 11 static verifier-trust-boundary categories. Definitions and the exact per-category checks are in the skill file ${SKILL_FILE} (fully self-contained). Read tests/verifier.py, tests/test.sh, the Dockerfiles, environment/seed/*, solution/solve.sh, instruction.md, task.toml and apply each rule. Return ONLY the structured report shown at the end of the skill file, with a PROMOTE / DO-NOT-PROMOTE recommendation. Do not modify any files."

printf 'acc-quality-audit: running subagent for %s ...\n' "$CASE_LABEL"

# Preflight the endpoint with a 90s ping before burning a 20-minute audit
# attempt on a dead gateway: the tingly gateway has been observed to accept
# the connection and then stall the stream indefinitely, so a hang here
# means every real attempt would hang too. Fail fast as TEMPFAIL so the
# case is retried later rather than cached as a rejection after two 20-min
# timeouts (which also blow the command-level timeout mid-retry).
if ! timeout --signal=TERM --kill-after=10s 90 \
    claude -p --dangerously-skip-permissions --output-format text \
    "Reply with the single word: ok" > /dev/null 2>&1; then
  emit verdict TEMPFAIL
  emit blocking false
  printf 'acc-quality-audit: TEMPFAIL %s -- gateway preflight failed (endpoint down or stalling)\n' \
    "$CASE_LABEL" >&2
  exit 75
fi

parse_recommendation() {
  # Collect every "recommendation:" line, not just the first: subagents
  # sometimes echo the skill's template ("recommendation: PROMOTE | DO NOT
  # PROMOTE (...) | PROMOTE-WITH-WARNINGS") before their actual verdict --
  # option lists contain a pipe, verdicts never do, so drop pipe lines and
  # take the last surviving line. Markdown emphasis (**PROMOTE**) and
  # trailing punctuation are prose-y decoration, not different verdicts.
  # (Three cases were false-rejected by the first-match parser on exactly
  # these shapes, 2026-08-30.)
  # The trailing || true is load-bearing: under set -e + pipefail, an empty
  # report makes the first grep exit 1 and would kill the whole script
  # before the retry/tempfail logic below ever runs (observed 2026-08-30
  # on a timeout-killed subagent: silent exit 1, cached as a rejection).
  grep -oP '(?i)recommendation:\s*\K.*' "$1" \
    | grep -v '|' \
    | tail -1 \
    | tr -d '*_`' \
    | sed -e 's/^[[:space:]]*//' -e 's/[.!,;:[:space:]]*$//' || true
}

run_subagent() {
  # The gateway connection can hang with a stalled stream (observed 2026-09-03:
  # kamailio-05b2da1 retried into 0 bytes, 7.7s CPU over 20+ min, and the 90s
  # endpoint preflight passed it because the gateway answers a trivial ping then
  # stalls the real stream). Two failure modes need killing, and both are the
  # STREAM going quiet, not the audit being legitimately long:
  #   * stalled stream -- bytes stop flowing though the connection is up.
  #   * runaway        -- the subagent reasons far past a finished report and
  #     runs into CLAUDE_CODE_MAX_OUTPUT_TOKENS (64000). Successful reports are
  #     5.5-8.5 KB, ~2500 tokens; 64000 is ~8x that, so hitting it is a runaway,
  #     not a verbose report. We do NOT raise the cap: it is the last guard on
  #     audit spend. We fail fast instead.
  # The liveness signal MUST come from a stream-json event stream, not from
  # stdout in --output-format text mode: text mode emits NOTHING until the
  # final report-writing phase, so an audit whose read/think phase outlasts
  # the stall window looks exactly like a dead stream (kamailio-05b2da1 was
  # false-killed 6x on 2026-09-3 while its stream was verifiably alive --
  # thinking_tokens events and tool calls flowing the whole time). With
  # stream-json every turn produces events (init, thinking trickle, tool
  # calls), so NO GROWTH for $STALL_AFTER_S really does mean a stalled
  # stream -- kill it now (and free watch) rather than waiting out 1200s.
  # The report_file contract is preserved by distilling the final text out
  # of the event stream after the process exits; report_is_broken then sees
  # an empty/partial report on killed attempts and the retry/TEMPFAIL path
  # runs unchanged.
  # No `timeout` wrapper and NO `|| true &`: backgrounding a compound
  # command makes $! a forked SUBSHELL with claude as its child, so
  # kill -KILL $! kills the subshell and orphans claude (observed 2026-09-03:
  # killed kamailio attempts kept streaming for 13+ min as PPID-1 orphans).
  # claude is backgrounded bare so $! IS claude; the nonzero exit is handled
  # at the wait. The watchdog loop enforces both the stall window and the
  # wall-clock cap itself.
  #
  # Stall window is 300s, not 120s: with a large accumulated context the
  # gateway's SINGLE-TURN latency for this audit is ~130s (measured gaps
  # between response events in kamailio's stream, 2026-09-03, host load
  # ~335). A 120s window killed healthy audits mid-turn; 300s of total
  # event silence is what a genuinely dead stream looks like. Wall cap is
  # 5400s: kamailio's audit was still fully productive (49 tool calls, 9
  # verification subagents, ~130s/turn) when a 2400s cap killed it on
  # 2026-09-03 -- this case's audit legitimately needs >40 min. The stall
  # window is the liveness guard; the wall cap only bounds total turn-count
  # burn, and per-turn spend is already capped (16k thinking, 64k output).
  #
  # MAX_THINKING_TOKENS caps PER-TURN thinking at 16k (2026-09-03: kamailio's
  # audit blew the 64000 OUTPUT cap inside a single turn -- output tokens
  # include thinking -- producing the "response exceeded the 64000 output
  # token maximum" API error that had masqueraded as gateway stalls all
  # day). The audit is an 11-category checklist spread over many turns, so
  # no single turn needs 64k of reasoning; 16k per turn leaves 48k of
  # headroom for report text under the unchanged output cap.
  : > "$stream_file"
  : > "$report_file"
  local stall_after="${ACC_AUDIT_STALL_AFTER_S:-300}"
  local wall_cap="${ACC_AUDIT_WALL_CAP_S:-5400}"
  MAX_THINKING_TOKENS="${ACC_AUDIT_THINKING_BUDGET:-16000}" \
  claude -p \
    --dangerously-skip-permissions \
    --output-format stream-json --verbose \
    "$prompt" > "$stream_file" 2>"$stream_file.err" < /dev/null &
  local claude_pid=$!
  local last_size=-1 idle=0 elapsed=0
  while kill -0 "$claude_pid" 2>/dev/null; do
    sleep 10
    elapsed=$((elapsed+10))
    local size
    size=$(wc -c < "$stream_file" 2>/dev/null || echo 0)
    if [ "$size" = "$last_size" ]; then
      idle=$((idle+10))
    else
      idle=0; last_size=$size
    fi
    if [ "$idle" -ge "$stall_after" ] || [ "$elapsed" -ge "$wall_cap" ]; then
      printf 'acc-quality-audit: %s -- %s; killing attempt\n' \
        "$CASE_LABEL" \
        "$([ "$idle" -ge "$stall_after" ] && printf 'stream stalled (no event growth for %ds)' "$idle" || printf 'wall clock cap (%ds) reached' "$elapsed")" >&2
      kill -KILL "$claude_pid" 2>/dev/null || true
      break
    fi
  done
  wait "$claude_pid" 2>/dev/null || true
  # Distill the final report text out of the event stream: the terminal
  # "result" event carries the whole final message; on a killed attempt
  # there is none, so fall back to the last assistant text blocks (partial
  # report -- still parseable if the recommendation line made it out).
  python3 - "$stream_file" "$report_file" <<'PYEOF'
import json, sys
src, dst = sys.argv[1], sys.argv[2]
result_text, last_texts = None, []
with open(src, errors='replace') as fh:
    for line in fh:
        line = line.strip()
        if not line.startswith('{'):
            continue
        try:
            e = json.loads(line)
        except ValueError:
            continue
        if e.get('type') == 'result':
            result_text = e.get('result') or ''
        elif e.get('type') == 'assistant':
            texts = [c.get('text', '') for c in e.get('message', {}).get('content', [])
                     if isinstance(c, dict) and c.get('type') == 'text']
            if texts:
                last_texts = texts
out = result_text if result_text is not None else '\n'.join(last_texts)
with open(dst, 'w') as fh:
    fh.write(out)
PYEOF
  # stderr now has its own file (it would corrupt the JSONL stream); if the
  # attempt produced no report text at all, promote stderr into report_file
  # so the report_is_broken heuristics still see API-error blobs.
  if [ ! -s "$report_file" ] && [ -s "$stream_file.err" ]; then
    cp "$stream_file.err" "$report_file"
  fi
}

run_subagent
recommendation=$(parse_recommendation "$report_file")

# The audit subagent rides the same inference gateway as everything else,
# and that upstream has been observed to fail in ways that look like
# reports: a 500 streaming error written as the whole "report", or an
# empty stream (1-byte file), or a truncated one that dies mid-S-category
# before ever reaching a recommendation line. A PARSE-ERROR verdict that
# drops the case should mean "the audit genuinely produced no verdict",
# not "the gateway hiccuped" -- so when the report carries no
# recommendation line AND looks like an upstream failure (empty, or an
# API-error blob, or cut off before the report's tail), retry the
# subagent once before giving up. The retry overwrites report_file, so
# the archived report is always the one the verdict was read from.
report_is_broken() {
  local n
  n=$(wc -c < "$report_file")
  [ "$n" -le 1 ] && return 0
  grep -q 'API Error' "$report_file" && [ "$n" -lt 3000 ] && return 0
  # A real report ends past S11 with the recommendation block; one that
  # stops mid-category (stream cut) has no recommendation and is short.
  [ -z "$(parse_recommendation "$report_file")" ] && [ "$n" -lt 3000 ] && return 0
  return 1
}
if [ -z "$recommendation" ] && report_is_broken; then
  printf 'acc-quality-audit: %s -- subagent produced no verdict (empty/error/truncated report, %s bytes); retrying once\n' \
    "$CASE_LABEL" "$(wc -c < "$report_file")" >&2
  run_subagent
  recommendation=$(parse_recommendation "$report_file")
fi

emit report "$report_file"
emit case "$slug"

if [ -z "$recommendation" ]; then
  # No verdict even after the retry: the gateway/subagent failed to produce
  # an audit, so there is nothing to judge the case by. Exit 75
  # (EX_TEMPFAIL) tells rollout-man to record NO verdict -- the case is
  # retried on a later watch poll instead of being cached as a rejection
  # (three cases were poisoned that way before this protocol existed).
  emit verdict TEMPFAIL
  emit blocking false
  printf 'acc-quality-audit: TEMPFAIL %s -- subagent produced no verdict after retry (%s bytes)\n' \
    "$CASE_LABEL" "$(wc -c < "$report_file")" >&2
  printf '  report: %s\n' "$report_file" >&2
  exit 75
fi

case "$recommendation" in
  PROMOTE)
    emit verdict PROMOTE
    emit blocking false
    printf 'acc-quality-audit: CLEAN %s -- PROMOTE  (report: %s)\n' "$CASE_LABEL" "$report_file"
    ;;
  PROMOTE-WITH-WARNINGS*)
    emit verdict PROMOTE-WITH-WARNINGS
    emit blocking false
    printf 'acc-quality-audit: WARNINGS %s -- medium issues found (not blocking)  (report: %s)\n' \
      "$CASE_LABEL" "$report_file" >&2
    printf 'acc-quality-audit: ok %s -- PROMOTE-WITH-WARNINGS\n' "$CASE_LABEL"
    ;;
  # "PROMOTE (conditional on ...)" / "PROMOTE, conditioned on ...": the
  # subagent promotes but hangs runtime conditions on it (typically R1/R2
  # blocked-needs-build in the dockerless audit env). Those conditions are
  # exactly what the pipeline's own admission step verifies downstream, so
  # this admits like PROMOTE-WITH-WARNINGS rather than tempfailing.
  PROMOTE[\ ,\(:\]]*)
    emit verdict PROMOTE-WITH-WARNINGS
    emit blocking false
    printf 'acc-quality-audit: WARNINGS %s -- conditional promote: %s  (report: %s)\n' \
      "$CASE_LABEL" "$recommendation" "$report_file" >&2
    printf 'acc-quality-audit: ok %s -- PROMOTE (conditional)\n' "$CASE_LABEL"
    ;;
  "DO NOT PROMOTE"*|DO-NOT-PROMOTE*)
    emit verdict DO-NOT-PROMOTE
    emit blocking true
    printf 'acc-quality-audit: FAIL %s -- %s\n' "$CASE_LABEL" "$recommendation" >&2
    printf '  report: %s\n' "$report_file" >&2
    exit 1
    ;;
  *)
    # A verdict-shaped line we still cannot classify is the subagent
    # misbehaving, not the case failing: tempfail, do not cache a rejection.
    emit verdict TEMPFAIL
    emit blocking false
    printf 'acc-quality-audit: TEMPFAIL %s -- could not parse recommendation from subagent output\n' \
      "$CASE_LABEL" >&2
    printf '  got: %s\n' "${recommendation:-(empty)}" >&2
    printf '  report: %s\n' "$report_file" >&2
    exit 75
    ;;
esac
