#!/usr/bin/env bash
set -euo pipefail
# Exactly what the real Harbor cases do: read the ground truth out of the
# read-only /solution mount and leave the deliverable in /app. If the runner
# throws the container away between the agent and the verifier, this file --
# and therefore the whole score -- is gone.
cp /solution/testcase.bin /app/crash.bin
echo "oracle: left the deliverable at /app/crash.bin"
