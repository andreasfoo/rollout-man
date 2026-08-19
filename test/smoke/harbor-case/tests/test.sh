#!/usr/bin/env bash
set -uo pipefail
mkdir -p /logs/verifier
score=0.0
if [ -f /app/crash.bin ] && [ "$(cat /app/crash.bin)" = "ROLLOUT-MAN-OK" ]; then
  score=1.0
  echo "PASS: deliverable survived from the agent step"
else
  echo "FAIL: no usable /app/crash.bin"
fi
# The Harbor contract: the number goes here, inside the container, and nowhere
# the runner gets to choose.
echo "$score" > /logs/verifier/reward.txt
echo "verifier ran as $(id -un)" > /logs/verifier/verifier.log
echo "FINAL SCORE: $score"
