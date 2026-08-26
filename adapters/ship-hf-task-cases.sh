#!/usr/bin/env bash
# Publish accepted-cases/ to a HF dataset, at whatever path_in_repo the caller
# names -- task/week1, task/week2, task/week3, ... One adapter, one sha256
# pin, reused across every week instead of a new fixed-path copy per week
# (the pattern ship-hf-week2-cases.sh/ship-hf-week2-trajectories.sh set).
#
#   KEY   "<repo_id>::<path_in_repo>", e.g. "tinglydev/cyber-xianjin::task/week1"
#         path_in_repo defaults to "." (ship-hf.sh's own default) if the
#         "::" separator is absent.
#
# A rejected or infrastructure-blocked batch has no accepted artifacts. Do
# not create empty dataset commits; a later resumed run can publish real data.
set -euo pipefail

: "${KEY:?ship-hf-task-cases needs KEY=repo_id::path_in_repo}"
repo_id=${KEY%%::*}
path_in_repo=.
[ "$repo_id" != "$KEY" ] && path_in_repo=${KEY#*::}

if ! find "${LOCAL_PATH:?ship command needs LOCAL_PATH}" -mindepth 1 -print -quit | grep -q .; then
  echo "nothing accepted under $LOCAL_PATH; skipping HF upload"
  exit 0
fi

export KEY="$repo_id" HF_PATH_IN_REPO="$path_in_repo"
exec "${ROLLOUT_MAN_SHIP_HF_ADAPTER:-adapters/ship-hf.sh}"
