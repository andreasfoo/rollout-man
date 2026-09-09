#!/bin/bash
# NOTE: absolute bash, not `/usr/bin/env bash`. The ship builtin forwards its
# step params as env vars, and the `path:` param becomes PATH=<materialized
# dir> (upperSnake) appended after the real PATH -- os/exec keeps the LAST
# duplicate, so the shebang's env lookup cannot find bash (exit 127, sngrep
# forge-flow-fuzz-1 2026-09-02 23:43). With an absolute interpreter the
# script starts anyway, and the guard below restores the machine PATH before
# any command is invoked. Remove both when the runner stops forwarding
# param-named vars (task #83).
set -euo pipefail

# PATH guard: the clobbered PATH (see above) points at the materialized tree,
# which contains no executables. Restore a working default rather than trust
# the inherited value.
case ":$PATH:" in
  *:/usr/bin:*) : ;;
  *) # ~/.local/bin must lead the restored PATH: this host's python3 (with
     # huggingface_hub) is a uv-managed install there, and /usr/bin/python3
     # lacks the module -- appended, the system one wins the lookup
     # (haproxy-6fe6018 ship, 2026-09-03 00:33 ModuleNotFoundError).
     export PATH="${HOME:+$HOME/.local/bin:}/usr/local/bin:/usr/bin:/bin" ;;
esac

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

# The repo path is task/week2 by default, overridable so later weeks reuse
# this adapter unchanged: HF_TASK_PATH=task/week3 (host env via the command's
# allowlist, or a with: entry on the step -- with: entries reach commands as
# env vars). Applies to both the atomic and the adapter-override path below.
task_path="${HF_TASK_PATH:-task/week2}"

if [ -n "${ROLLOUT_MAN_SHIP_HF_ADAPTER:-}" ]; then
  echo "ship-hf-week2-atomic: ROLLOUT_MAN_SHIP_HF_ADAPTER set -- shipping each tree via $ROLLOUT_MAN_SHIP_HF_ADAPTER (not atomic)"
  if ! $cases_empty; then
    HF_PATH_IN_REPO="$task_path" "$ROLLOUT_MAN_SHIP_HF_ADAPTER"
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

# ${VAR:-} on both sides of the inner default: set -u evaluates the word
# unconditionally, and an unset RUN_ID (hand-invocation) used to abort here.
msg="${HF_COMMIT_MESSAGE:-rollout-man: ${EXPERIMENT:-batch} ${TRIAL_ID:-${RUN_ID:-manual}}}"
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

add_tree(cases_dir, os.environ.get("HF_TASK_PATH", "task/week2"))
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
