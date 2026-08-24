#!/usr/bin/env bash
# Ship to anything rclone can reach: OneDrive, S3, WebDAV, a box over sftp.
#
#   LOCAL_PATH  what to ship
#   KEY         the path under the remote
#
# The remote and its credentials live in the operator's rclone.conf. rollout-man
# does not read, store or rotate them -- which is also why there is no OneDrive
# code anywhere in this repo.
set -euo pipefail
remote="${RCLONE_REMOTE:?set RCLONE_REMOTE, e.g. onedrive:rollout-man}"
rclone copyto --create-empty-src-dirs -- "$LOCAL_PATH" "$remote/$KEY"
echo "$remote/$KEY"
