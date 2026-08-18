#!/usr/bin/env bash
# Deliberately slow so the worker can be killed mid-activity.
set -euo pipefail
echo "oracle: working..."
sleep "${SLOW_SECONDS:-25}"
printf 'ROLLOUT-MAN-OK' > /app/answer.txt
echo "oracle: done"
