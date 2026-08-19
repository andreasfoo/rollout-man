#!/usr/bin/env bash
echo "solve: sleeping past the agent timeout"
sleep 30
printf 'ROLLOUT-MAN-OK' > /app/answer.txt
