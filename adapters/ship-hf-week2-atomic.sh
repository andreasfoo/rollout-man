#!/usr/bin/env bash
# Atomically publish accepted-cases (task/week2) and accepted-trajectories
# (trajectory) to HF in a single commit so the two trees are never partially
# uploaded.
#
# Replaces the two separate ship_week2_cases / ship_week2_trajectories steps
# in per_experiment. If either source tree is empty the upload is skipped
# entirely (consistent with the single-tree guards in the individual adapters).
#
# The source root may name its two trees either way: accepted-cases/
# accepted-trajectories/ (what the batch materializer writes) or task/
# trajectory/ (what the per-trial materializer writes inside a trial's own
# OUT_DIR/materialized -- named after where they publish). Same two trees,
# same two destinations.
#
# ENV (set by rollout-man ship builtin or per_experiment env):
#   LOCAL_PATH   the root directory (contains the two trees as siblings)
#   KEY          HF repo id, e.g. tinglydev/cyber-xianjin
#   HF_REVISION  (opt) branch/revision, default repo default branch
#   HF_PRIVATE   (opt) "1" (default) to create private repo if new
#   HF_TOKEN     (opt) overrides hf auth login credential
#   ROLLOUT_MAN_SHIP_HF_ADAPTER (opt) alternate ship-hf.sh path; when set it
#                is used per tree instead of huggingface_hub -- atomicity is
#                traded for the override, which exists for stubbed smoke tests.
set -euo pipefail

: "${LOCAL_PATH:?ship-hf-week2-atomic needs LOCAL_PATH=run directory}"
: "${KEY:?ship-hf-week2-atomic needs KEY=hf-repo-id}"

pick_dir() { # pick_dir <preferred> <fallback>
  if [ -d "$LOCAL_PATH/$1" ]; then printf '%s' "$1"
  elif [ -d "$LOCAL_PATH/$2" ]; then printf '%s' "$2"
  else printf '%s' "$1"; fi
}
cases_name=$(pick_dir accepted-cases task)
trajs_name=$(pick_dir accepted-trajectories trajectory)
cases_dir="$LOCAL_PATH/$cases_name"
trajs_dir="$LOCAL_PATH/$trajs_name"

cases_empty=true
trajs_empty=true
if find "$cases_dir" -mindepth 1 -print -quit 2>/dev/null | grep -q .; then
  cases_empty=false
fi
if find "$trajs_dir" -mindepth 1 -print -quit 2>/dev/null | grep -q .; then
  trajs_empty=false
fi

if $cases_empty && $trajs_empty; then
  echo "ship-hf-week2-atomic: nothing accepted; skipping HF upload"
  exit 0
fi

if [ -n "${ROLLOUT_MAN_SHIP_HF_ADAPTER:-}" ]; then
  echo "ship-hf-week2-atomic: ROLLOUT_MAN_SHIP_HF_ADAPTER set -- shipping each tree via $ROLLOUT_MAN_SHIP_HF_ADAPTER (not atomic)"
  if ! $cases_empty; then
    HF_PATH_IN_REPO=task/week2 "$ROLLOUT_MAN_SHIP_HF_ADAPTER"
  fi
  if ! $trajs_empty; then
    HF_PATH_IN_REPO=trajectory "$ROLLOUT_MAN_SHIP_HF_ADAPTER"
  fi
  exit 0
fi

if $cases_empty; then
  echo "ship-hf-week2-atomic: WARN accepted-cases/ is empty but accepted-trajectories/ is not -- uploading trajectories only" >&2
fi
if $trajs_empty; then
  echo "ship-hf-week2-atomic: WARN accepted-trajectories/ is empty but accepted-cases/ is not -- uploading cases only" >&2
fi

msg="${HF_COMMIT_MESSAGE:-rollout-man: ${EXPERIMENT:-batch} ${TRIAL_ID:-$RUN_ID}}"
private="${HF_PRIVATE:-1}"
revision="${HF_REVISION:-}"

python3 - "$KEY" "$cases_dir" "$trajs_dir" "$msg" "$private" "$revision" <<'PY'
import sys, os
from pathlib import Path
from huggingface_hub import HfApi, CommitOperationAdd

repo_id, cases_dir, trajs_dir, commit_msg, private_flag, revision = sys.argv[1:]
api = HfApi()

# Ensure the repo exists (create if needed).
try:
    api.repo_info(repo_id=repo_id, repo_type="dataset")
except Exception:
    api.create_repo(repo_id=repo_id, repo_type="dataset",
                    private=(private_flag == "1"), exist_ok=True)

ops = []

def add_tree(local_root, hf_prefix):
    root = Path(local_root)
    if not root.is_dir():
        return
    for p in sorted(root.rglob("*")):
        if not p.is_file():
            continue
        rel = p.relative_to(root)
        path_in_repo = f"{hf_prefix}/{rel}".lstrip("/")
        ops.append(CommitOperationAdd(path_in_repo=path_in_repo, path_or_fileobj=str(p)))

add_tree(cases_dir, "task/week2")
add_tree(trajs_dir, "trajectory")

if not ops:
    print("ship-hf-week2-atomic: no files to upload after scanning both trees")
    sys.exit(0)

kwargs = dict(
    repo_id=repo_id,
    repo_type="dataset",
    operations=ops,
    commit_message=commit_msg,
)
if revision:
    kwargs["revision"] = revision

result = api.create_commit(**kwargs)
print(f"ship-hf-week2-atomic: committed {len(ops)} file(s) → {result.commit_url}")
PY
