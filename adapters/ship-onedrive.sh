#!/usr/bin/env bash
# Batch upload to OneDrive: one zip per case, under 我的文件/testcase/.
#
# rollout-man runs this ONCE, after the whole batch, with:
#   LOCAL_PATH  the run directory (runs/<run-id>)
#   KEY         the destination path, from pipeline.per_experiment.ship.dest
#
# The unit is the case, not the trial: each case's zip carries its trials'
# agent output, the harbor jobs dir (the case-format record of the run), and
# the one-line verdicts from results.jsonl. Admission probes stay behind --
# they are the gate's evidence, not the batch's product.
#
# The redaction tiers in the run already scrubbed keys (everywhere) and IPs
# (distributables only). IPs are wanted gone from everything that ships, so
# this is the second, blunt pass: a mask grep over the staging tree before
# zip, refusing to upload rather than uploading something with an address in
# it. Credentials are not ours: rclone uses whatever rclone.conf the machine
# already has.
# Configuration:
#   KEY          (in)  destination path, from pipeline.per_experiment.ship.dest
#   SHIP_REMOTE  (env) rclone remote name, default "onedrive" -- the machine's
#                      own rclone.conf decides which account that is
#   SHIP_BASE    (env) fallback destination when KEY is unset, default "testcase"
#
# What is NOT configurable: the leak guard. "脱敏 ip、token、key" is the
# requirement the upload exists under, so it is enforced, not offered.
set -euo pipefail

REMOTE=${SHIP_REMOTE:-onedrive}
DEST_BASE=${KEY:-${SHIP_BASE:-testcase}}
STAGE=$(mktemp -d)
trap 'rm -rf "$STAGE"' EXIT

# One zip per case. results.jsonl names each trial's case; the trial
# directories themselves group the artifacts.
declare -A case_of trials_of
while IFS= read -r line; do
  id=$(printf '%s' "$line" | python3 -c 'import json,sys; print(json.load(sys.stdin)["trial_id"])')
  cs=$(printf '%s' "$line" | python3 -c 'import json,sys; print(json.load(sys.stdin)["case"])')
  case "$id" in admit-*) continue ;; esac
  case_of["$id"]="$cs"
  trials_of["$cs"]+="$id "
done < "$LOCAL_PATH/results.jsonl"

[ ${#case_of[@]} -gt 0 ] || { echo "ship-onedrive: no trials recorded"; exit 1; }

mkdir -p "$STAGE/zips"
for cs in "${!trials_of[@]}"; do
  name=$(basename "$cs")          # duckdb-3f0eb51-issue-526-t4
  dir="$STAGE/$name"
  mkdir -p "$dir"
  for id in ${trials_of["$cs"]}; do
    out="$LOCAL_PATH/trials/$id/out"
    [ -d "$out" ] || continue
    # The jobs dir rides along inside the trial, named after it.
    mkdir -p "$dir/jobs/$id"
    cp -r "$out/." "$dir/jobs/$id/" 2>/dev/null || true
  done
  # The batch's verdict on this case's trials.
  python3 - "$LOCAL_PATH/results.jsonl" "$cs" > "$dir/results.jsonl" <<'PY'
import json, sys
cs = sys.argv[2]
for line in open(sys.argv[1]):
    line = line.strip()
    if not line:
        continue
    r = json.loads(line)
    if r.get("trial_id", "").startswith("admit-"):
        continue
    if r.get("case") == cs:
        print(line)
PY

  # Second pass, blunt on purpose: everything here leaves the machine, so
  # keys AND ips get masked. A hit we cannot explain stops the upload.
  hits=$(grep -rlE \
    -e 'sk-[A-Za-z0-9_-]{16,}' \
    -e '(authorization|api[_-]?key|token)\s*[:=]' \
    -e '\b([0-9]{1,3}\.){3}[0-9]{1,3}\b' \
    "$dir" 2>/dev/null || true)
  if [ -n "$hits" ]; then
    echo "ship-onedrive: refusing to upload $name, potential secrets/IPs in:" >&2
    echo "$hits" >&2
    exit 1
  fi

  (cd "$STAGE" && zip -qr "zips/$name.zip" "$name")
done

for z in "$STAGE"/zips/*.zip; do
  [ -e "$z" ] || break
  name=$(basename "$z")
  target="${REMOTE}:${DEST_BASE%/}/$name"
  rclone copyto -- "$z" "$target"
  echo "ship-onedrive: uploaded $name -> $target"
done
