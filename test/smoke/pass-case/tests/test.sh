#!/usr/bin/env bash
set -uo pipefail
mkdir -p /logs/verifier
score=0.0
if [ -f /app/answer.txt ] && [ "$(cat /app/answer.txt)" = "ROLLOUT-MAN-OK" ]; then
  score=1.0
  echo "PASS: token present and correct"
else
  echo "FAIL: token missing or wrong"
fi
echo "$score" > /logs/verifier/reward.txt
echo "FINAL SCORE: $score"
