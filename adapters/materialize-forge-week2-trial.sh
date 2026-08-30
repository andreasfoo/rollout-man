#!/usr/bin/env bash
# Per-trial materializer: build this one trial's slice of the two HF trees.
#
# The batch version (materialize-forge-week2.sh) scans a finished run's whole
# results.jsonl and rebuilds both trees from every accepted row before a
# single ship call. This one runs inside per_trial, right after redact, and
# materializes only the trial it belongs to -- so the ship step that follows
# it lands that trial's data on HF as soon as the trial is accepted, not
# after the slowest case in the batch closes out.
#
# Everything is scoped inside the trial's own OUT_DIR
# (runs/<run-id>/trials/<trial-id>/out), so concurrent trials can never race
# on a shared tree: nothing here touches another trial's directory, and
# nothing is rm -rf'd.
#
# ENV (set by rollout-man for a per_trial command step):
#   OUT_DIR  this trial's output directory; accepted trials have OUT_DIR/case
# ROLLOUT_MAN_OUTPUT:
#   cases_dir         absolute path of the trial's materialized case tree
#   trajectories_dir  absolute path of the trial's materialized trajectory tree
#   materialized_dir  their shared parent (what an atomic shipper wants)
set -euo pipefail

# A rejected or non-terminal trial never writes OUT_DIR/case -- that absence
# is the accept/reject signal, exactly as the batch script's reward filter is.
# Nothing to materialize is not an error: the guard step has already dropped
# the trial, and ship on a dropped trial is a no-op upstream anyway.
[ -d "${OUT_DIR:?materialize_week2_trial needs OUT_DIR}" ] || exit 0
[ -d "$OUT_DIR/case" ] || exit 0

# The case name is the case directory's basename. OUT_DIR/case is a literal
# copy target (both adapters do `cp -a "$x" "$OUT_DIR/case"`), so the name
# must come from the CASE_DIR this trial was seeded from, never from the copy
# -- basename(".../out/case") is always "case", and publishing that would key
# every trial's HF slot to the same path, each ship overwriting the last.
name="$(basename "${CASE_DIR:?materialize_week2_trial needs CASE_DIR}")"
mat="$OUT_DIR/materialized"
# The two trees are named after where they publish, not what they hold:
# task/ -> HF task/week2/<name>, trajectory/ -> HF trajectory/<name>/.
# A reader comparing local materialized/ against the dataset sees the same
# shape on both sides.
tasks="$mat/task"
trajs="$mat/trajectory"
mkdir -p "$tasks" "$trajs"

python3 - "$OUT_DIR/case" "$tasks" "$trajs" "$name" <<'PY'
import json, os, re, shutil, sys

src, cases, trajs, name = sys.argv[1:]
src, cases, trajs = (os.path.abspath(p) for p in (src, cases, trajs))

# The published task.toml must not leak which network a live task actually
# saw. network_mode/allowed_hosts (in [agent], [verifier], [environment],
# [verifier.environment]) name the proxy IP (e.g. the Kimi proxy) or the
# verifier/candidate-environment's own live egress config; allow_internet
# (in [environment]) reveals the sandbox egress policy. Same class of leak,
# any section -- so the keys are scrubbed globally, not per-table. Keys are
# dropped outright (not masked): a stray "allowed_hosts = [...]" with no
# hosts left behind is itself a tell, so the line goes, not just its value.
# (2026-08-28: allow_internet was missing from the old per-section table and
# leaked through to HF -- task/week1/spidermonkey-3bd2629-bug-1983221-t4.)
SCRUB_KEYS = ('network_mode', 'allow_internet', 'allowed_hosts')
SCRUB_RE = re.compile(r'\s*(' + '|'.join(SCRUB_KEYS) + r')\s*=')

def scrub_network(path):
    if not os.path.isfile(path):
        return
    lines = open(path).read().splitlines(keepends=True)
    out = [line for line in lines if not SCRUB_RE.match(line)]
    open(path, 'w').writelines(out)

case_dst = os.path.join(cases, name)
if os.path.exists(case_dst):
    raise SystemExit('materialize_week2_trial: destination already exists: ' + case_dst)
# .factory/ is case-generation scratch (triage, oracle-check trials, bg
# logs); trials/ is Harbor's own trial output, copied into the case dir
# as a side effect of running there. jobs/ is the case's own copy of the
# Harbor trajectory that generated it, published separately below under
# trajectory/<name>/jobs/ -- keeping it here would duplicate the full
# trajectory (including agent session transcripts) under task/ too.
shutil.copytree(src, case_dst, symlinks=True,
                ignore=shutil.ignore_patterns('.factory', 'trials', 'jobs'))
scrub_network(os.path.join(case_dst, 'task.toml'))

# Harbor's own started_at is the trajectory timestamp. The task owns its
# trajectory namespace, so jobs publish as
# trajectory/<task-slug>/jobs/<timestamp>/<task-slug>__<harbor-run-suffix>/.
src_trials = os.path.join(src, 'trials')
jobs = []
if os.path.isdir(src_trials):
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
    # Unlike the batch script this is fatal-but-local: a trial whose replay
    # produced no individual Harbor job has nothing publishable, and the row
    # is still recorded with this step's failure rather than silently
    # shipping a case with no trajectory. (The mock adapter and a live
    # accepted run both always write trials/.)
    raise SystemExit('trial has no individual Harbor job result: ' + name)
PY

# The outputs must be absolute: the ship step's `path:` uses them verbatim
# when absolute, and joins them under the run dir when not. OUT_DIR is
# relative whenever the run dir itself is (watch's default --runs ./runs),
# and a relative output would make ship point at runs/<run>/runs/<run>/...
# -- a path that does not exist, whose "empty" tree the shipper then skips
# with a success exit.
mat="$(cd "$mat" && pwd)"
tasks="$mat/task"
trajs="$mat/trajectory"
{
  printf 'cases_dir=%s\n' "$tasks"
  printf 'trajectories_dir=%s\n' "$trajs"
  printf 'materialized_dir=%s\n' "$mat"
} >> "${ROLLOUT_MAN_OUTPUT:-/dev/null}"
