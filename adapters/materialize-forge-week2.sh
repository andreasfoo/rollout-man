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
# saw -- but it must still CARRY a network config: a task.toml with the keys
# simply deleted does not match the corpus convention and leaves Harbor to
# guess. So the keys are NORMALIZED, not scrubbed: each of [verifier],
# [agent] and [environment] is rewritten to a fixed, non-leaking literal.
#
# This is strictly safer than the old global scrub it replaces. The observed
# value is OVERWRITTEN with a constant rather than merely dropped, and
# allowed_hosts is forced to [] -- the real allowlist (the tingly LLM proxy
# CIDR) is injected at harbor runtime via --allow-agent-host and never
# reaches a shipped file. allow_internet is still dropped outright and never
# re-emitted: it reveals the sandbox egress policy and has no canonical form.
# (2026-08-28: allow_internet was missing from the old per-section table and
# leaked through to HF -- task/week1/spidermonkey-3bd2629-bug-1983221-t4.)
# None of the emitted literals contains an IPv4 address or matches a leak
# gate pattern, so injection cannot trip gate_check.
#
# Keep this block byte-identical to the copy in the sibling materializer and
# to adapters/normalize-task-network.py (the standalone backfill tool).
SCRUB_KEYS = ('network_mode', 'allow_internet', 'allowed_hosts')
SCRUB_RE = re.compile(r'\s*(' + '|'.join(SCRUB_KEYS) + r')\s*=')
SCHEMA_RE = re.compile(r'^(schema_version\s*=\s*")(\d+\.\d+)("[^\n]*)$')
TARGET_SECTIONS = {
    'verifier': ['network_mode = "no-network"'],
    'agent': ['network_mode = "allowlist"', 'allowed_hosts = []'],
    'environment': ['network_mode = "public"'],
}
# ANY section header, dotted sub-tables ([verifier.env], [solution.env])
# included. A sub-table header is exactly where the parent table's DIRECT
# keys stop, so it is both the flush point for the section being left and a
# non-target section we must not inject into. Deferring the flush to the next
# TOP-LEVEL header instead is silently wrong: a task.toml whose [environment]
# is followed by [environment.env] and no further top-level section emits
# network_mode at EOF -- inside [environment.env], parsing as
# environment.env.network_mode while environment.network_mode, the key Harbor
# reads, stays absent. Valid TOML, right-looking diff, zero effect.
ANY_HEADER_RE = re.compile(r'^\s*\[([^\[\]]+)\]\s*(?:#.*)?$')


def normalize_task_network(path):
    """Rewrite task.toml's network config to the canonical published shape.

    Bumps schema_version 1.2 -> 1.3, strips every SCRUB_KEYS line, and gives
    each existing target section its canonical lines as the section's last
    content. Only sections already present are touched -- no section is
    synthesized. Idempotent: strip-then-append plus the guarded schema bump
    means a second pass yields identical bytes (required, since watch
    re-gates a case on any content change).
    """
    if not os.path.isfile(path):
        return
    lines = open(path).read().splitlines(keepends=True)
    out, cur, pending = [], None, []

    def flush():
        # Canonical lines become the section's last CONTENT line -- inserted
        # before whatever blank-line separator the file already uses, so the
        # section/blank/section rhythm survives and re-running is a no-op.
        if not pending:
            return
        blanks = []
        while out and not out[-1].strip():
            blanks.append(out.pop())
        out.extend(pending)
        out.extend(reversed(blanks))
        del pending[:]

    for line in lines:
        # A plain string replace, not a regex rewrite: splitlines(keepends=1)
        # leaves the newline inside `line`, and a regex suffix group is either
        # greedy (swallowing past the closing quote) or newline-excluding
        # (dropping the line ending). Replacing just the quoted version keeps
        # the line's own whitespace intact.
        m = SCHEMA_RE.match(line)
        if m:
            out.append(line.replace('"1.2"', '"1.3"', 1)
                       if m.group(2) == '1.2' else line)
            continue
        h = ANY_HEADER_RE.match(line)
        if h:
            flush()
            name_ = h.group(1).strip()
            # Only an undotted header names a table we inject into; a dotted
            # sub-table sets cur to None so its lines never arm `pending`.
            cur = name_ if '.' not in name_ else None
            out.append(line)
            continue
        if SCRUB_RE.match(line):
            continue
        out.append(line)
        if cur in TARGET_SECTIONS and not pending:
            pending[:] = [ln + '\n' for ln in TARGET_SECTIONS[cur]]
    flush()
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
    normalize_task_network(os.path.join(case_dst, 'task.toml'))
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
