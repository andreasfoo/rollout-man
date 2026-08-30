#!/usr/bin/env bash
# Build the two independently publishable HF trees from accepted rollout rows.
set -euo pipefail
: "${LOCAL_PATH:?materialize command needs LOCAL_PATH=run directory}"
run=$LOCAL_PATH
cases="$run/accepted-cases"
trajs="$run/accepted-trajectories"
rm -rf "$cases" "$trajs"
mkdir -p "$cases" "$trajs"

python3 - "$run" "$cases" "$trajs" <<'PY'
import json, os, re, shutil, sys
run, cases, trajs = map(os.path.abspath, sys.argv[1:])

# The published task.toml must not leak which network a live task actually
# saw. network_mode/allowed_hosts (in [agent], [verifier], [environment],
# [verifier.environment]) name the proxy IP (e.g. the Kimi proxy) or the
# verifier/candidate-environment's own live egress config; allow_internet
# (in [environment]) reveals the sandbox egress policy. Same class of leak,
# any section -- so the keys are scrubbed globally, not per-table. Keys are
# dropped outright (not masked): a stray "allowed_hosts = [...]" with no
# hosts left behind is itself a tell, so the line goes, not just its value.
SCRUB_KEYS = ('network_mode', 'allow_internet', 'allowed_hosts')
SCRUB_RE = re.compile(r'\s*(' + '|'.join(SCRUB_KEYS) + r')\s*=')

def scrub_network(path):
    if not os.path.isfile(path):
        return
    lines = open(path).read().splitlines(keepends=True)
    out = [line for line in lines if not SCRUB_RE.match(line)]
    open(path, 'w').writelines(out)

for line in open(os.path.join(run, 'results.jsonl')):
    r = json.loads(line)
    if r.get('dropped') or r.get('reward') is None or r.get('reward', 1) >= .6:
        continue
    out = os.path.join(run, 'trials', r['trial_id'], 'out')
    src = os.path.join(out, 'case')
    if not os.path.isdir(src):
        raise SystemExit('accepted trial missing case artifact: ' + r['trial_id'])
    # Case IDs are task directory basenames and must be unique in a batch.
    name = os.path.basename(r['case'])
    case_dst = os.path.join(cases, name)
    # .factory/ is case-generation scratch (triage, oracle-check trials, bg
    # logs); trials/ is Harbor's own trial output, copied into the case dir
    # as a side effect of running there. jobs/ is the case's own copy of the
    # Harbor trajectory that generated it, already published separately below
    # under trajectory/<name>/jobs/ -- keeping it here would duplicate the
    # full trajectory (including agent session transcripts) under task/ too.
    # None of the three is part of the task -- all three would otherwise ride
    # along into the published dataset undetected.
    shutil.copytree(src, case_dst, symlinks=True,
                     ignore=shutil.ignore_patterns('.factory', 'trials', 'jobs'))
    scrub_network(os.path.join(case_dst, 'task.toml'))
    # Harbor's own started_at is the trajectory timestamp. Each accepted task
    # owns its trajectory namespace, so jobs publish as
    # trajectory/<task-slug>/jobs/<timestamp>/<task-slug>__<harbor-run-suffix>/.
    # Keeping the complete trial_name is essential: the random run suffix
    # alone does not identify which task produced the trajectory. The timestamp
    # is made filesystem-safe while retaining its UTC instant.
    src_trials = os.path.join(src, 'trials')
    if not os.path.isdir(src_trials):
        raise SystemExit('accepted case has no Harbor trials: ' + name)
    jobs = []
    for root, dirs, files in os.walk(src_trials):
        if 'result.json' not in files:
            continue
        result_path = os.path.join(root, 'result.json')
        try:
            result = json.load(open(result_path))
        except (OSError, json.JSONDecodeError):
            continue
        # A job result has trial_name + started_at; batch result.json does not.
        if not result.get('trial_name') or not result.get('started_at'):
            continue
        stamp = result['started_at'].replace(':', '-').replace('+00:00', 'Z')
        task_suffix = result['trial_name']
        # Harbor's trial_name is already `<task-slug>__<random-suffix>`.
        # Refuse path separators rather than allowing a malformed result to
        # escape the desired HF tree.
        if os.path.basename(task_suffix) != task_suffix:
            raise SystemExit('unsafe Harbor trial_name: ' + task_suffix)
        dst = os.path.join(trajs, name, 'jobs', stamp, task_suffix)
        if os.path.exists(dst):
            raise SystemExit('duplicate Harbor trajectory destination: ' + dst)
        shutil.copytree(root, dst, symlinks=True)
        jobs.append(dst)
    if not jobs:
        raise SystemExit('accepted case has no individual Harbor job result: ' + name)
PY
