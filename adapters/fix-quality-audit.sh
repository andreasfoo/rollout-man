#!/usr/bin/env bash
# Per-case auto-fix for a blocking acc-quality-audit verdict.
#
# Mirror image of adapters/acc-quality-audit.sh: that adapter audits a case
# and explicitly forbids the subagent from modifying files; this adapter
# takes the report that audit already wrote and asks a subagent to actually
# close the blocking gaps it found, then hands back control to rollout-man's
# fix-and-retry mechanism (a per_case step's `fix:` + `fix_writeback: true`),
# which re-runs check_quality once to see whether the fix actually worked.
#
# This adapter does not judge success itself -- it only attempts the repair.
# The retried check_quality call is the real validation (a fresh
# acc-quality-audit subagent, not this one grading its own work).
#
# ENV (all passed through rollout-man; see internal/actions/actions.go runFix):
#   CASE_DIR    path to the case package (writable; a live source dir, not a
#               copy -- edits here land on the operator's actual submission)
#   CASE_LABEL  human-readable label for the case (same slug convention
#               acc-quality-audit.sh uses for the report filename)
#   RUN_DIR     rollout-man run directory (where the report to fix was saved)
#   CYBER_GATE_ROOT  optional; defaults to /home/foo/project/cyber-gate
#   LLM_BASE_URL LLM_MODEL LLM_API_KEY  optional; see acc-quality-audit.sh --
#                same llm_spec: mapping onto ANTHROPIC_*, same fallthrough to
#                the ambient claude config when unset.
set -euo pipefail

[ -n "${LLM_BASE_URL:-}" ] && export ANTHROPIC_BASE_URL="$LLM_BASE_URL"
[ -n "${LLM_API_KEY:-}" ] && export ANTHROPIC_AUTH_TOKEN="$LLM_API_KEY"
[ -n "${LLM_MODEL:-}" ] && export ANTHROPIC_MODEL="$LLM_MODEL"

SKILL_FILE="${CYBER_GATE_ROOT:-/home/foo/project/cyber-gate}/.claude/skills/acc-quality-audit/SKILL.md"

[ -f "$SKILL_FILE" ] || {
  printf 'fix-quality-audit: FAIL %s -- skill file not found: %s\n' "$CASE_LABEL" "$SKILL_FILE" >&2
  exit 1
}

# Same slug/report-path convention as acc-quality-audit.sh -- this adapter
# gets no with: of its own (fix: commands are a fixed remedy for a fixed
# check, not another configurable step; see actions.go's runFix comment), so
# the report path has to be recomputed rather than passed in.
slug=$(basename "$CASE_LABEL")
report_file="$RUN_DIR/quality/${slug}.txt"

[ -s "$report_file" ] || {
  printf 'fix-quality-audit: FAIL %s -- no prior audit report at %s (fix invoked before an audit ran?)\n' \
    "$CASE_LABEL" "$report_file" >&2
  exit 1
}

prompt="A prior acc-quality-audit run flagged this case package as blocking. The audit's report is
below; the full category definitions (severities, PASS/FAIL criteria, and per-category remediation
intent) are in the skill file ${SKILL_FILE}.

Case package: ${CASE_DIR}

--- BEGIN AUDIT REPORT (${report_file}) ---
$(cat "$report_file")
--- END AUDIT REPORT ---

Fix ONLY the categories the report marks fail at critical/high/direct_removal severity -- these are
what blocks promotion. Leave any medium-severity fail as-is (those are warnings, not blocking, and
are out of scope for this fix). For each blocking category, make the minimal change under
${CASE_DIR} that closes the underlying trust-boundary gap the category describes -- follow the
skill file's stated Check/PASS criteria for that category, and any concrete remediation the report's
own evidence/narration already suggests, rather than inventing an unrelated approach.

Constraints:
- Only edit files under ${CASE_DIR}.
- Do not touch reward-scoring logic, thresholds, or checks unrelated to the flagged categories.
- Do not weaken or remove a check to make it report pass without actually closing the gap it
  audits (no gaming the re-audit) -- the fix must hold up under a fresh, independent audit of this
  same case.
- Preserve the case's intended reward semantics: an oracle/reference solution must still score
  1.0 and a no-op must still score 0.0 after your change (do not change what the verifier is
  supposed to reward, only how it protects itself while doing so).

When done, summarize what you changed and why, one line per category fixed."

printf 'fix-quality-audit: running subagent for %s (report: %s) ...\n' "$CASE_LABEL" "$report_file"

claude -p \
  --dangerously-skip-permissions \
  --output-format text \
  "$prompt"

printf 'fix-quality-audit: done %s -- check_quality will retry to verify\n' "$CASE_LABEL"
