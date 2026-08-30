#!/usr/bin/env bash
# Append an idempotent rollout-man terminal ledger to the requested progress doc.
set -euo pipefail
: "${LOCAL_PATH:?record command needs LOCAL_PATH=run directory}"
# No PROGRESS_FILE given: fall back to ./Progress.md rather than failing the
# whole per_experiment tail. A missing ledger target is a default, not an error.
PROGRESS_FILE="${PROGRESS_FILE:-Progress.md}"
python3 - "$LOCAL_PATH" "$PROGRESS_FILE" <<'PY'
import datetime, json, os, sys
run, progress = sys.argv[1:]
run_id = os.path.basename(run)
rows = []
for line in open(os.path.join(run, 'results.jsonl')):
    r=json.loads(line)
    if r['trial_id'].startswith('admit-'): continue
    reward = r.get('reward')
    # An absent score is infrastructure noise, never a terminal rejection.
    # It must stay eligible for a later re-drive once the model service recovers.
    if reward is None:
        verdict = 'blocked'
    elif reward < .6:
        verdict = 'accepted'
    else:
        verdict = 'rejected'
    rows.append((os.path.basename(r['case']), verdict, reward, r.get('failure_message','')))
marker = f'<!-- rollout-man-week2:{run_id} -->'
old = open(progress, encoding='utf-8').read() if os.path.exists(progress) else '# Progress\n'
if marker in old: raise SystemExit(0)
with open(progress, 'a', encoding='utf-8') as f:
    f.write(f'\n{marker}\n\n## rollout-man week2 — {run_id}\n\n')
    f.write('| case | terminal | score | note |\n| --- | --- | ---: | --- |\n')
    for case, verdict, reward, note in sorted(rows):
        score = '-' if reward is None else f'{reward:.2f}'
        note_line = note.splitlines()[0] if note else ''
        f.write(f'| {case} | {verdict} | {score} | {note_line.replace("|", "/")} |\n')
    f.write(f'\nRecorded {datetime.datetime.now(datetime.timezone.utc).isoformat()}.\n')
PY
