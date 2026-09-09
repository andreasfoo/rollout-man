#!/usr/bin/env bash
# Preflight for experiments/tc-batch3-kimi3-topup4.yaml.
#
# The top-up campaign ships with ship_week2_trajectories, which writes ONLY
# under trajectory/<case>/ and assumes task/week3/<case>/ already exists on
# HF. A case whose task tree is missing would get trajectories published
# against no task -- an inconsistent dataset state that is awkward to undo on
# a shared branch. Three of the four cases had task_files=0 on week3 earlier
# today and depend on watch's ship_week2_atomic landing first.
#
# This script answers, per case: does the task tree exist on origin/week3,
# and how many distinct jobs/ trajectory stamps are published? Launch the
# campaign only when every case shows a task tree; the campaign then adds
# 3 trajectories per case to reach the target of 4.
#
# Usage: adapters/preflight-topup4.sh [target_count]
set -euo pipefail

HFCASE="${HFCASE_DIR:-/home/foo/project/hfcase}"
TARGET="${1:-4}"
CASES=(
  kamailio-05b2da1-commit-f98c37a-t4
  mruby-431d4bb-commit-a6b55e7-t4
  spidermonkey-6d489cb-bug-2064093-t4
  yara-d5ae8ef-commit-7b7c796-t4
)

[ -d "$HFCASE/.git" ] || { echo "preflight: not a git repo: $HFCASE" >&2; exit 1; }
git -C "$HFCASE" fetch -q origin week3

# One ls-tree pass; python does the counting so the jobs/ stamp component is
# read positionally (trajectory/<case>/jobs/<stamp>/...) rather than by a
# fragile awk field index -- counting the literal "jobs" component instead of
# the stamp silently reports 1 for every case.
git -C "$HFCASE" ls-tree -r --name-only origin/week3 \
  | python3 -c "
import sys, collections
traj = collections.defaultdict(set)
task = collections.Counter()
for line in sys.stdin:
    p = line.strip().split('/')
    if p[0] == 'trajectory' and len(p) >= 4 and p[2] == 'jobs':
        traj[p[1]].add(p[3])
    elif p[0] == 'task' and len(p) >= 3 and p[1] == 'week3':
        task[p[2]] += 1

cases = sys.argv[1:-1]
target = int(sys.argv[-1])
ready = blocked = 0
print(f'{\"case\":<40} {\"task\":>6} {\"traj\":>5}  state')
for c in cases:
    t, n = task[c], len(traj[c])
    if t == 0:
        state, blocked = 'BLOCKED - no task tree on week3', blocked + 1
    elif n >= target:
        state, ready = f'DONE - already >= {target}', ready + 1
    else:
        state, ready = f'READY - needs +{target - n}', ready + 1
    print(f'{c:<40} {t:>6} {n:>5}  {state}')
print()
if blocked:
    print(f'HOLD: {blocked} case(s) have no task tree on week3.')
    print('Let watch ship_week2_atomic land for those cases first.')
    sys.exit(1)
print(f'CLEAR: all {ready} case(s) have a task tree; safe to launch the top-up.')
" "${CASES[@]}" "$TARGET"
