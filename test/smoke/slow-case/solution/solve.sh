#!/usr/bin/env bash
# Deliberately slow so a run can be interrupted mid-trial.
set -euo pipefail
echo "oracle: working..."
sleep 12
printf 'ROLLOUT-MAN-OK' > /app/answer.txt
echo "oracle: done"
