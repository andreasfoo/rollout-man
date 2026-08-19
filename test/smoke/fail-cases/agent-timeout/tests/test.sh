#!/usr/bin/env bash
set -uo pipefail
mkdir -p /logs/verifier
score=0.0
[ -f /app/answer.txt ] && score=1.0
echo "$score" > /logs/verifier/reward.txt
echo "FINAL SCORE: $score"
