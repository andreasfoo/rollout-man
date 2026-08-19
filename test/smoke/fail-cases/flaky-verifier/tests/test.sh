#!/usr/bin/env bash
set -uo pipefail
mkdir -p /logs/verifier
# /tests is the case directory itself, the one path that outlives the per-trial
# sandbox -- which is exactly what a "fails once, then works" probe needs.
if [ ! -f /tests/.attempted ]; then
  : > /tests/.attempted
  echo "verifier: first attempt, dying on purpose"
  exit 1
fi
echo "1.0" > /logs/verifier/reward.txt
echo "FINAL SCORE: 1.0"
