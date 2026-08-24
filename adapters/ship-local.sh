#!/usr/bin/env bash
# Ship to a directory on this machine.
#
#   LOCAL_PATH  what to ship
#   KEY         where to put it, under $SHIP_ROOT
#
# The simplest destination there is, and the one worth having: it makes a
# pipeline runnable and testable before anyone has credentials for anything.
set -euo pipefail
dst="${SHIP_ROOT:-$HOME/rollout-man-out}/$KEY"
mkdir -p "$dst"
cp -r -- "$LOCAL_PATH/." "$dst/"
echo "$dst"
