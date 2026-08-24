#!/usr/bin/env bash
# Fetch a case from a Hugging Face dataset repo.
#
#   HF_REPO / HF_REVISION   which repo, which revision
#   HF_PATH                 optional subdirectory to keep
#   LOCAL_PATH              where to put it
#
# The revision is what you asked for; the *version* is still the content hash
# rollout-man computes afterwards. A moving revision is allowed and pinned in
# the run's records, so "the same experiment" cannot quietly mean two things.
set -euo pipefail
: "${HF_REPO:?source-hf needs repo: on the case}"
args=(download "$HF_REPO" --repo-type dataset --local-dir "$LOCAL_PATH")
[ -n "${HF_REVISION:-}" ] && args+=(--revision "$HF_REVISION")
[ -n "${HF_PATH:-}" ] && args+=(--include "$HF_PATH/*")
hf "${args[@]}" > /dev/null
