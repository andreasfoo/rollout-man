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
#
# ENV (all passed through rollout-man):
#   CASE_DIR    path to the case package
#   CASE_LABEL  human-readable label for the case
#   RUN_DIR     rollout-man run directory (for report output)
#   CYBER_GATE_ROOT  optional; defaults to /home/foo/project/cyber-gate
set -euo pipefail

SKILL_FILE="${CYBER_GATE_ROOT:-/home/foo/project/cyber-gate}/.claude/skills/acc-quality-audit/SKILL.md"

[ -f "$SKILL_FILE" ] || {
  printf 'acc-quality-audit: FAIL %s -- skill file not found: %s\n' "$CASE_LABEL" "$SKILL_FILE" >&2
  exit 1
}

mkdir -p "$RUN_DIR/quality"
report_file="$RUN_DIR/quality/${CASE_LABEL}.txt"

prompt="Read the case package at ${CASE_DIR} and audit it against the 11 static verifier-trust-boundary categories. Definitions and the exact per-category checks are in the skill file ${SKILL_FILE} (fully self-contained). Read tests/verifier.py, tests/test.sh, the Dockerfiles, environment/seed/*, solution/solve.sh, instruction.md, task.toml and apply each rule. Return ONLY the structured report shown at the end of the skill file, with a PROMOTE / DO-NOT-PROMOTE recommendation. Do not modify any files."

printf 'acc-quality-audit: running subagent for %s ...\n' "$CASE_LABEL"

claude -p \
  --dangerously-skip-permissions \
  --output-format text \
  "$prompt" > "$report_file" 2>&1

recommendation=$(grep -oP '(?i)recommendation:\s*\K.*' "$report_file" | head -1 | sed 's/[[:space:]]*$//')

case "$recommendation" in
  PROMOTE)
    printf 'acc-quality-audit: CLEAN %s -- PROMOTE  (report: %s)\n' "$CASE_LABEL" "$report_file"
    ;;
  PROMOTE-WITH-WARNINGS)
    printf 'acc-quality-audit: WARNINGS %s -- medium issues found (not blocking)  (report: %s)\n' \
      "$CASE_LABEL" "$report_file" >&2
    printf 'acc-quality-audit: ok %s -- PROMOTE-WITH-WARNINGS\n' "$CASE_LABEL"
    ;;
  "DO NOT PROMOTE"*)
    printf 'acc-quality-audit: FAIL %s -- %s\n' "$CASE_LABEL" "$recommendation" >&2
    printf '  report: %s\n' "$report_file" >&2
    exit 1
    ;;
  *)
    printf 'acc-quality-audit: FAIL %s -- could not parse recommendation from subagent output\n' \
      "$CASE_LABEL" >&2
    printf '  got: %s\n' "${recommendation:-(empty)}" >&2
    printf '  report: %s\n' "$report_file" >&2
    exit 1
    ;;
esac
