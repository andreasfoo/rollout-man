#!/usr/bin/env bash
# Template: a custom step that runs a deterministic local tool.
#
# Copy this, point a command at it, and name that command in a pipeline:
#
#   kind: Commands
#   my_step: {uses: templates/action-tool.sh, env: [MY_TOOL_HOME]}
#
#   pipeline:
#     per_trial: [{uses: harbor}, {uses: my_step}]
#
# Most of what people write as a custom step is already built in -- redact,
# guard, archive, dataset, report, check_case, ship_hf, ship_rclone,
# ship_github. Reach for this when the thing you need is genuinely yours.
#
# ---------------------------------------------------------------- inputs ---
# Which variables arrive depends on where the step sits.
#
#   always        EXPERIMENT  RUN_ID  RUN_DIR  LOCAL_PATH
#   per_case      CASE_DIR  CASE_LABEL  CASE_SHA
#   per_trial     TRIAL_ID  OUT_DIR   (LOCAL_PATH is OUT_DIR here)
#   with: keys    my_key: v  ->  MY_KEY=v
#
# With inherit_env: false, the ONLY host variables you get are the ones the
# command declares in env:. That is deliberate: a step that talks to one system
# has no business holding another system's credentials.
set -euo pipefail

: "${RUN_DIR:?this step must be run by rollout-man}"

# --------------------------------------------------------------- outputs ---
# Exit 0 for success. Exit non-zero to fail the step, and say why on stderr --
# that message is what someone reads three days later, so make it name the
# thing that is wrong rather than the line that noticed.
#
# To hand a value to a later step, write key=value lines to $STEP_OUTPUTS
# (when set); the next step reads them as {{steps.<this step's label>.outputs.<key>}}.

# ------------------------------------------------------------------ body ---
# Do the work. Two rules worth keeping:
#
#  1. Be safe to run twice. A step can be re-run after an interrupted batch,
#     and "append" without a marker turns that into silent duplication.
#  2. Do not write secrets into anything under $RUN_DIR. Artifacts are
#     published; a key that reaches a published dataset cannot be taken back.

echo "action-tool: nothing to do -- replace this with the actual work" >&2
exit 1
