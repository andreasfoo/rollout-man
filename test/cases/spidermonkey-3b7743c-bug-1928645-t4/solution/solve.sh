#!/usr/bin/env bash
# Deliver the crash input to /app/crash.js for the verifier.
set -euo pipefail
cp /solution/testcase.js /app/crash.js
chmod 644 /app/crash.js
