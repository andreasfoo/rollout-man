#!/usr/bin/env bash
set -euo pipefail

# A rejected or infrastructure-blocked batch has no accepted artifacts.  Do
# not create empty dataset commits; a later resumed run can publish real data.
if ! find "${LOCAL_PATH:?ship command needs LOCAL_PATH}" -mindepth 1 -print -quit | grep -q .; then
  echo "nothing accepted under $LOCAL_PATH; skipping HF upload"
  exit 0
fi
# The repo path is task/week2 by default, overridable so later weeks reuse
# this adapter unchanged: HF_PATH_IN_REPO=task/week3 (host env via the
# command's allowlist, or a with: entry on the step -- with: entries reach
# commands as env vars).
export HF_PATH_IN_REPO="${HF_PATH_IN_REPO:-task/week2}"
exec "${ROLLOUT_MAN_SHIP_HF_ADAPTER:-adapters/ship-hf.sh}"
