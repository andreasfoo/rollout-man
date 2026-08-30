#!/usr/bin/env bash
# Publish to the Hugging Face Hub as a dataset.
#
#   LOCAL_PATH  the directory to publish (usually the run's dataset/ directory)
#   KEY         the repo id, e.g. my-org/rollout-libaom
#
# Credentials are not ours: `hf` reads HF_TOKEN, or whatever `hf auth login`
# left behind. Nothing here reads, stores or forwards a token.
#
# Why this is the destination worth getting right: the Hub already is content
# addressing, versioning and access control, so there is no reason to build a
# CAS or an artifact registry next to it. What it needs from us is the *shape* --
# rows plus a card -- which is what the `dataset` action produces. Upload a
# directory tree instead and you get a folder on a server, not a dataset.
set -euo pipefail

: "${KEY:?ship-hf needs dest: to be the repo id, e.g. my-org/rollout-libaom}"
# TRIAL_ID is present only for a per-trial ship; without it this is a
# batch-level call, and RUN_ID stays the distinguisher.
msg="${HF_COMMIT_MESSAGE:-rollout-man: ${EXPERIMENT:-batch} ${TRIAL_ID:-$RUN_ID}}"

args=(upload "$KEY" "$LOCAL_PATH" "${HF_PATH_IN_REPO:-.}"
      --repo-type dataset --commit-message "$msg")
[ -n "${HF_REVISION:-}" ] && args+=(--revision "$HF_REVISION")
[ "${HF_PRIVATE:-1}" = "1" ] && args+=(--private)

hf "${args[@]}"
