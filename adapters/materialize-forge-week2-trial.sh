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

# Secret sanitization + leak gate, ported from casefactory-goloopx/add_jobs.py.
# The materializer is the choke point between whatever the case dir carries
# (factory-staged jobs/ or Harbor's raw trials/ output) and HF: harbor's
# config.json/result.json embed the live proxy URL and LAN IPs verbatim, and
# nothing upstream of this script redacts them. Sanitize EVERY text file in
# both published trees, then gate: zero GATE_PATTERNS matches allowed, else
# the step fails rather than shipping a leak (2026-08-31: nghttp2's raw
# Aug-27 factory jobs/ shipped the live proxy URL to HF verbatim).
SANITIZE = [
    # full tingly proxy URL first (before the IP mask fragments it). The
    # letter-lookbehind keeps "interestingly"/"excitingly" inside a URL path
    # from being rewritten -- the real proxy token always follows a
    # non-letter ("/tingly/", "tingly-box-...").
    (re.compile(r'https?://[^\s"\'\\,)\]]*?(?<![A-Za-z])tingly[^\s"\'\\,)\]]*'), 'http://REDACTED/anthropic'),
    # ANTHROPIC_BASE_URL value (JSON or env style)
    (re.compile(r'(ANTHROPIC_BASE_URL["\']?\s*[:=]\s*["\']?)[^"\',\s}\\]+'), r'\1http://REDACTED/anthropic'),
    # ANTHROPIC_API_KEY value (JSON or env style)
    (re.compile(r'(ANTHROPIC_API_KEY["\']?\s*[:=]\s*["\']?)[^"\',\s}\\]+'), r'\1REDACTED'),
    # bare sk- style keys, 16+ chars after the prefix like the core redact
    # package: real keys (sk-ant-..., sk-proj-...) are always that long,
    # and shorter thresholds mangle ordinary identifiers (2026-08-31:
    # gecko-31e2eff's test.sh shipped `mktemp -d /tmp/task-REDACTED.XXXXXX`
    # from "task-verifier"; add_jobs.py, the pattern's origin, mangles the
    # same way -- see its factory trajectory stdout). The negative
    # lookbehind additionally protects suffixes of longer words
    # ("disk-assisted", "risk-management").
    (re.compile(r'(?<![A-Za-z0-9_-])sk-[A-Za-z0-9_-]{16,}'), 'sk-REDACTED'),
    # Claude Code session slug: the CLI mints a random codename per session
    # ("slug":"tingly-forging-pascal") that can begin with "tingly" by pure
    # chance -- it is NOT the proxy token and false-blocks clean trials
    # (2026-09-02: ocserv-8aa1022-1, a 1.0 trial, 230 slug hits, 0 real
    # leaks). Rewrite just the slug's tingly prefix; the gate then passes
    # naturally with no exemption that could hide a real leak.
    (re.compile(r'("slug"\s*:\s*")tingly'), r'\1cc'),
]

# Every IPv4 address is masked, not just one known subnet (2026-09-01
# policy: nothing published may carry an address, public included). A
# hardcoded subnet regex would both name the very address it exists to hide
# -- this is a public repo -- and miss any past/future proxy on a different
# one; exact-secret matching cannot help either, because factory-era content
# carries addresses that are not in this campaign's env. The guards mirror
# the core redact package's IP rule so the two layers never disagree:
#   - octets must be <= 255 (version quads like libtool's 2.4.2.418)
#   - no digit/dot continuation (5-part versions: ch-base:1.26.3.12.3)
#   - not adjacent to a word char, ':' or '.' (image tags ch-base:26.3.12.3,
#     C++ scope tokens, mid-word suffixes)
#   - not in version context ("Version 4.2.1.9", "release 1.2.3.4")
#   - loopback/unspecified stay readable (127.0.0.1, 0.0.0.0)
# sanitize and gate share _ip_spans, so the gate can never be stricter than
# the sanitizer (a stricter gate retries the whole trial forever).
IPV4_RE = re.compile(r'\d{1,3}(?:\.\d{1,3}){3}')
IP_ALLOW = {'127.0.0.1', '0.0.0.0'}
VERSION_WORDS = {'version', 'ver', 'release'}
IP_MASK = 'REDACTED_IP'

def _ip_spans(content):
    spans = []
    for m in IPV4_RE.finditer(content):
        s, e = m.span()
        if s > 0 and (content[s-1].isalnum() or content[s-1] in '_:.'):
            continue
        if e < len(content):
            if content[e] == '.' and e + 1 < len(content) and content[e+1].isdigit():
                continue
            if content[e].isalnum() or content[e] == '_':
                continue
        addr = m.group(0)
        if addr in IP_ALLOW or any(int(o) > 255 for o in addr.split('.')):
            continue
        i = s
        while i > 0 and content[i-1] == ' ':
            i -= 1
        j = i
        while j > 0 and content[j-1].isalpha():
            j -= 1
        if content[j:i].lower() in VERSION_WORDS:
            continue
        spans.append((s, e))
    return spans

def mask_ips(content):
    spans = _ip_spans(content)
    if not spans:
        return content
    out, last = [], 0
    for s, e in spans:
        out.append(content[last:s])
        out.append(IP_MASK)
        last = e
    out.append(content[last:])
    return ''.join(out)

GATE_PATTERNS = [
    re.compile(r':12584\b'),
    # The tingly proxy token always follows a non-letter in real leaks: the
    # URL path (/tingly/), the key prefix (tingly-box-...), an env value
    # (".../tingly"). A bare substring would match ordinary English --
    # "interestingly", "excitingly" -- and false-block clean trajectories
    # (2026-08-31: clickhouse-166a case's reasoning text tripped it).
    re.compile(r'(?<![A-Za-z])tingly'),
    # Same threshold as SANITIZE: a gate stricter than the sanitizer blocks
    # trials over tokens the sanitizer deliberately leaves alone (benign
    # 10-15 char sk- identifiers), which retries the whole trial forever.
    re.compile(r'(?<![A-Za-z0-9_-])sk-[A-Za-z0-9]{16,}'),
]
# lock.json is NOT excluded: harbor's lock embeds the agent env verbatim --
# ANTHROPIC_BASE_URL (the live tingly proxy URL + LAN IP:port) and a masked
# API key -- and an exclusion here means neither sanitize_tree nor gate_check
# touches it (2026-08-31: smoke2's trajectory lock.json carried the live
# proxy URL into the shipped tree).
EXCLUDE_NAMES = {'__pycache__'}

def is_probably_text(path):
    try:
        with open(path, 'rb') as f:
            chunk = f.read(4096)
    except OSError:
        return False
    return b'\x00' not in chunk

def sanitize_tree(tree):
    for root, dirs, files in os.walk(tree):
        dirs[:] = [d for d in dirs if d not in EXCLUDE_NAMES]
        for fn in files:
            if fn in EXCLUDE_NAMES:
                continue
            fp = os.path.join(root, fn)
            if not is_probably_text(fp):
                continue
            try:
                content = open(fp, 'r', encoding='utf-8', errors='replace').read()
            except OSError:
                continue
            orig = content
            for rx, repl in SANITIZE:
                content = rx.sub(repl, content)
            content = mask_ips(content)
            if content != orig:
                open(fp, 'w', encoding='utf-8').write(content)

def gate_check(tree):
    hits = []
    for root, dirs, files in os.walk(tree):
        dirs[:] = [d for d in dirs if d not in EXCLUDE_NAMES]
        for fn in files:
            if fn in EXCLUDE_NAMES:
                continue
            fp = os.path.join(root, fn)
            if not is_probably_text(fp):
                continue
            try:
                content = open(fp, 'r', encoding='utf-8', errors='replace').read()
            except OSError:
                continue
            for rx in GATE_PATTERNS:
                if rx.search(content):
                    hits.append((os.path.relpath(fp, tree), rx.pattern))
                    break
            else:
                if _ip_spans(content):
                    hits.append((os.path.relpath(fp, tree), 'unmasked IPv4 address'))
    return hits

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
# A re-gated case reuses its deterministic trial ID -> the same OUT_DIR ->
# this trial's own previous materialized tree is still here. Replacing it
# is safe (nothing outside this trial's OUT_DIR is ever touched) and is
# the ONLY correct behavior: refusing would strand every repaired case's
# re-run at this step (2026-08-31: nghttp2's post-sanitize re-gate).
if os.path.exists(case_dst):
    shutil.rmtree(case_dst)
# .factory/ is case-generation scratch (triage, oracle-check trials, bg
# logs); trials/ is Harbor's own trial output, copied into the case dir
# as a side effect of running there. jobs/ is the case's own copy of the
# Harbor trajectory that generated it, published separately below under
# trajectory/<name>/jobs/ -- keeping it here would duplicate the full
# trajectory (including agent session transcripts) under task/ too.
shutil.copytree(src, case_dst, symlinks=True,
                ignore=shutil.ignore_patterns('.factory', 'trials', 'jobs'))
scrub_network(os.path.join(case_dst, 'task.toml'))
sanitize_tree(case_dst)
gate = gate_check(case_dst)
if gate:
    raise SystemExit('materialize_week2_trial: LEAK GATE FAILED (task tree): '
                     + '; '.join('%s (%s)' % h for h in gate[:5]))

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
        # Same re-gate logic as the task tree above: this path is unique to
        # this trial (stamp = this replay's started_at); a collision means
        # it's this trial's own earlier materialization.
        if os.path.exists(dst):
            shutil.rmtree(dst)
        shutil.copytree(root, dst, symlinks=True)
        sanitize_tree(dst)
        gate = gate_check(dst)
        if gate:
            raise SystemExit('materialize_week2_trial: LEAK GATE FAILED (trajectory '
                             + dst + '): ' + '; '.join('%s (%s)' % h for h in gate[:5]))
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
