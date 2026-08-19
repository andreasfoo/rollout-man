#!/usr/bin/env bash
# Batch upload: commit the run's trajectories into this repo under $KEY.
#
# rollout-man runs this ONCE, after the whole batch, with:
#   LOCAL_PATH  the run directory (runs/<run-id>)
#   KEY         the destination path, from pipeline.per_experiment.ship.dest
#
# Only trajectories and results go up -- not the whole run directory. Build
# logs and verifier output belong to whoever is debugging; what a batch is
# published for is what the agents did.
#
# Credentials are not ours: this uses whatever git identity and remote auth the
# machine already has. Nothing is stored, nothing is rotated here.
set -euo pipefail

REPO=${SHIP_REPO:-$(git rev-parse --show-toplevel)}
BRANCH=${SHIP_BRANCH:-$(git -C "$REPO" rev-parse --abbrev-ref HEAD)}
DEST="$REPO/$KEY"

mkdir -p "$DEST"
for trial in "$LOCAL_PATH"/trials/*/; do
  id=$(basename "$trial")
  # Admission probes are the gate's evidence, not the batch's product.
  case "$id" in admit-*) continue ;; esac
  out="$trial/out"
  [ -d "$out" ] || continue
  mkdir -p "$DEST/$id"
  # The trail: whatever the agent itself left behind, plus the one-line verdict.
  [ -d "$out/agent" ]         && cp -r "$out/agent/."   "$DEST/$id/" 2>/dev/null || true
  [ -s "$out/traj.jsonl" ]    && cp "$out/traj.jsonl"   "$DEST/$id/" || true
  [ -f "$out/result.json" ]   && cp "$out/result.json"  "$DEST/$id/" || true
done
cp "$LOCAL_PATH/results.jsonl" "$DEST/results.jsonl" 2>/dev/null || true

cd "$REPO"
git add -- "$KEY"
if git diff --cached --quiet -- "$KEY"; then
  echo "ship: nothing new under $KEY"
  exit 0
fi
git commit -q -m "trails: $(basename "$LOCAL_PATH")

Uploaded by rollout-man's per_experiment.ship after the batch finished.
$(cd "$DEST" && ls -d */ 2>/dev/null | tr -d /  | tr '\n' ' ')"
for attempt in 1 2 3 4; do
  git push origin "HEAD:$BRANCH" && exit 0
  sleep $((2 ** attempt))
done
echo "ship: push to $BRANCH failed" >&2
exit 1
