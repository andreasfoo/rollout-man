#!/usr/bin/env bash
# Template: a custom step that asks a model something.
#
# An audit, a repair, a judge. Point a command at it and give that command an
# llm_spec, so which endpoint it talks to is a submission setting rather than
# something baked into this file:
#
#   kind: LLMSpec
#   name: auditor
#   provider: anthropic
#   model: anthropic/claude-sonnet-4-5
#   api_key_env: ANTHROPIC_API_KEY
#
#   kind: Commands
#   audit: {uses: templates/action-llm.sh, llm_spec: auditor}
#
#   pipeline:
#     per_case:
#       - uses: audit
#         fix: repair            # optional: a repair step, then re-check
#         fix_writeback: true
#         fix_attempts: 2
#
# ---------------------------------------------------------------- inputs ---
#   LLM_BASE_URL  LLM_MODEL  LLM_API_KEY   from the llm_spec above
#   plus the same positional variables any step gets (CASE_DIR, OUT_DIR, ...)
set -euo pipefail

: "${LLM_MODEL:?this step needs an llm_spec: on its command}"

# Providers disagree about which variable holds the key, so the spec's provider
# decides and this maps it. Never echo the key, and never write it to a file
# under $RUN_DIR.
export ANTHROPIC_BASE_URL="${LLM_BASE_URL:-}"
export ANTHROPIC_AUTH_TOKEN="${LLM_API_KEY:-}"

# ------------------------------------------------- the part that matters ---
# Two failures look the same from here and must not be treated the same:
#
#   the model answered, and the answer is no    -> a VERDICT. Exit non-zero.
#      The step failing is the point. Do not retry it: asking again costs
#      money and changes nothing, which is why pipeline steps run once.
#
#   the call did not complete (timeout, 5xx,    -> an INCIDENT. Also exit
#   rate limit, no credentials)                    non-zero, but say so
#      distinctly on stderr, and give the step `retries: 2` in the pipeline so
#      it is retried and the verdict case is not.
#
# Getting this backwards is expensive in both directions: retrying a verdict
# bills you N times for one answer, and treating an outage as a verdict
# rejects work that was never judged.

response=$(cat <<'PROMPT'
Replace this with the actual question, and make it one whose answer is
checkable. "Is this case well formed?" invites agreement; "list every file
under solution/ that the verifier reads" can be wrong in a visible way.
PROMPT
)

# ... call the model with "$response", inspect the result ...

if [ -z "${response:-}" ]; then
  echo "action-llm: the model call did not complete -- transient, safe to retry" >&2
  exit 1
fi

echo "action-llm: replace this with the real check" >&2
exit 1
