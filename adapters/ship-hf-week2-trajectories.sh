#!/usr/bin/env bash
set -euo pipefail

# A rejected or infrastructure-blocked batch has no accepted artifacts.  Do
# not create empty dataset commits; a later resumed run can publish real data.
if ! find "${LOCAL_PATH:?ship command needs LOCAL_PATH}" -mindepth 1 -print -quit | grep -q .; then
  echo "nothing accepted under $LOCAL_PATH; skipping HF upload"
  exit 0
fi
export HF_PATH_IN_REPO=trajectory
exec "${ROLLOUT_MAN_SHIP_HF_ADAPTER:-adapters/ship-hf.sh}"
