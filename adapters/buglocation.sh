#!/usr/bin/env bash
# Per-case bug localization: buglocation.
#
# Produces solution/bug_location.json for a case -- the corpus convention
# (batch4's 31 files, tc_week1.py's REQUIRED_CASE_PATHS): {task, entrypoint,
# critical_code, root_cause, cwe}, where entrypoint/critical_code each carry
# {filename, description, line_range}. Sample shape:
#   batch1/caddy-cve-2026-30851-t3/solution/bug_location.json
#
# The case's own trajectory is the primary evidence: the admission-time
# rollout already found the crash, and its stream names the file/function and
# the ASan stack. The subagent reads the case package + that trajectory and
# writes the JSON; this adapter validates the schema strictly (exact keys,
# string line numbers, CWE-nnn shape) so a malformed generation can never
# ship -- a schema failure is TEMPFAIL (retry), not a silent pass.
#
# Output goes to $RUN_DIR/buglocation/<case-label>.json -- NOT into the case
# directory: watch (PID-gated on HashDir of the case tree) would see
# solution/ change and re-gate the case. Shipping the file into the HF task
# tree is a separate explicit step.
#
# ENV (from rollout-man):
#   CASE_DIR CASE_LABEL RUN_DIR WORK_DIR
#   LLM_BASE_URL LLM_MODEL LLM_API_KEY  optional; mapped onto the ANTHROPIC_*
#                names `claude -p` reads when this command declares llm_spec:
#                in kind: Commands (same mapping acc-quality-audit.sh uses).
set -euo pipefail

[ -n "${LLM_BASE_URL:-}" ] && export ANTHROPIC_BASE_URL="$LLM_BASE_URL"
[ -n "${LLM_API_KEY:-}" ] && export ANTHROPIC_AUTH_TOKEN="$LLM_API_KEY"
[ -n "${LLM_MODEL:-}" ] && export ANTHROPIC_MODEL="$LLM_MODEL"

emit() { [ -n "${ROLLOUT_MAN_OUTPUT:-}" ] && printf '%s=%s\n' "$1" "$2" >> "$ROLLOUT_MAN_OUTPUT" || true; }
fail_case() { printf 'buglocation: FAIL %s -- %s\n' "$CASE_LABEL" "$1" >&2; emit verdict ERROR; exit 1; }
tempfail() {
  printf 'buglocation: TEMPFAIL %s -- %s\n' "$CASE_LABEL" "$1" >&2
  emit verdict TEMPFAIL
  exit 75
}

slug=$(basename "${CASE_LABEL:-$(basename "$CASE_DIR")}")
outdir="$RUN_DIR/buglocation"
mkdir -p "$outdir"
out_file="$outdir/${slug}.json"
report_file="$outdir/${slug}.txt"

# Gateway preflight (90s): a stalled tingly stream would burn the whole
# attempt budget per case. Same rationale as acc-quality-audit.sh.
if ! timeout --signal=TERM --kill-after=10s 90 \
    claude -p --dangerously-skip-permissions --output-format text \
    "Reply with the single word: ok" > /dev/null 2>&1; then
  tempfail "gateway preflight failed (endpoint down or stalling)"
fi

# The case's admission trajectory is the evidence: the freshest job under
# jobs/ carries agent/claude-code.txt (the stream that found the crash) and
# verifier output. Point the subagent at it rather than making it re-derive
# the defect from source alone.
traj_hint=""
latest_job=$(find "$CASE_DIR/jobs" -mindepth 2 -maxdepth 2 -type d 2>/dev/null | sort | tail -1)
if [ -n "$latest_job" ] && [ -d "$latest_job/agent" ]; then
  traj_hint=" The case's own admission rollout already found the crash; its agent stream and verifier output are under $latest_job (agent/claude-code.txt names the crashing file/function and the sanitizer report). Use them as primary evidence, then confirm against the source tree paths named in instruction.md."
fi

prompt="Read the security case package at $CASE_DIR (instruction.md, tests/, solution/solve.sh, environment/, task.toml).$traj_hint
Identify the single memory-safety or logic defect this case is built around and write a bug-localization report as a JSON object with EXACTLY these keys:
task (the case name: $slug), entrypoint {filename, description, line_range}, critical_code {filename, description, line_range}, root_cause, cwe {id, name}.
- entrypoint: the function/call site where the attack first becomes reachable (how attacker-controlled data enters).
- critical_code: the specific site that would need to change to eliminate the defect (where the weakness manifests).
- line_range: two strings, the start and end line numbers of the ENCLOSING FUNCTION -- its signature/opening line through its closing brace (e.g. [\"64\", \"275\"]). This must be the full function span, never a single line: do not collapse it to the one crashing line (no [\"120\",\"120\"]) and never emit [\"1\",\"1\"]. The end line must be strictly greater than the start line.
- root_cause: 2-4 sentences on why the weakness exists and how it becomes exploitable.
- cwe: the most applicable CWE id (\"CWE-NNN\") and its official name.
filenames are relative to the source tree root the instruction names.
Return ONLY the JSON object, no markdown fence, no commentary."

run_subagent() {
  # 20m cap mirrors acc-quality-audit; || true keeps set -e out of the way --
  # the output's content drives every decision, not claude's exit code.
  timeout --signal=TERM --kill-after=30s 1200 \
    claude -p --dangerously-skip-permissions --output-format text \
    "$prompt" > "$report_file" 2>&1 || true
}

run_subagent

# Extract the JSON object: the subagent may wrap it in a fence or prose.
extract_json() {
  python3 - "$1" <<'PY'
import json, re, sys
text = open(sys.argv[1], encoding="utf-8", errors="replace").read()
# Try the whole text, then the first {...} block (fenced or not).
for candidate in [text, re.search(r"\{.*\}", text, re.S).group(0) if re.search(r"\{.*\}", text, re.S) else ""]:
    if not candidate.strip():
        continue
    try:
        obj = json.loads(candidate)
    except json.JSONDecodeError:
        continue
    if not isinstance(obj, dict):
        continue
    # Pretty-print for the humans who audit these files: 2-space indent and
    # real Unicode (ensure_ascii=False) so an em-dash reads as "--" not
    # "—". Still valid JSON -- validate() json.loads it unchanged.
    print(json.dumps(obj, indent=2, ensure_ascii=False))
    sys.exit(0)
sys.exit(1)
PY
}

validate() {
  # Strict schema: exact key set, right shapes, CWE-nnn, numeric line_range.
  python3 - "$1" "$slug" <<'PY'
import json, sys
d = json.load(open(sys.argv[1]))
name = sys.argv[2]
errs = []
if set(d) != {"task", "entrypoint", "critical_code", "root_cause", "cwe"}:
    errs.append(f"top-level keys {sorted(d)}")
    print("; ".join(errs)); sys.exit(1)
if d["task"] != name:
    errs.append(f"task {d['task']!r} != case name {name!r}")
for sec in ("entrypoint", "critical_code"):
    s = d.get(sec) or {}
    if not isinstance(s, dict) or set(s) != {"filename", "description", "line_range"}:
        errs.append(f"{sec} keys {sorted(s) if isinstance(s, dict) else type(s)}"); continue
    lr = s["line_range"]
    if (not isinstance(lr, list) or len(lr) != 2
            or not all(isinstance(x, str) and x.isdigit() for x in lr)):
        errs.append(f"{sec}.line_range {lr!r} (want two numeric strings)")
    else:
        # A collapsed (start==end, e.g. ["1","1"]) or inverted (start>end)
        # range is a placeholder, not a real span -- it throws away the
        # enclosing-function extent (entry point through the manifesting
        # site) that makes the localization useful. Reject it so the model
        # is retried rather than shipping a single-line pointer.
        start, end = int(lr[0]), int(lr[1])
        if end <= start:
            errs.append(f"{sec}.line_range {lr!r} degenerate "
                        f"(need end>start; a real function spans multiple lines)")
    if not s["filename"] or "/" not in s["filename"] and "." not in s["filename"]:
        errs.append(f"{sec}.filename {s['filename']!r} not a path")
rc = d.get("root_cause")
if not isinstance(rc, str) or len(rc) < 40:
    errs.append("root_cause missing/too short")
cwe = d.get("cwe") or {}
if not isinstance(cwe, dict) or set(cwe) != {"id", "name"} \
        or not str(cwe.get("id", "")).startswith("CWE-") \
        or not cwe.get("name"):
    errs.append(f"cwe {cwe!r}")
print("; ".join(errs))
sys.exit(1 if errs else 0)
PY
}

if ! json_line=$(extract_json "$report_file"); then
  printf 'buglocation: %s -- no JSON object in subagent output (%s bytes); retrying once\n' \
    "$CASE_LABEL" "$(wc -c < "$report_file")" >&2
  run_subagent
  json_line=$(extract_json "$report_file") || tempfail "no JSON object after retry (report: $report_file)"
fi

printf '%s\n' "$json_line" > "$out_file.tmp"
if errs=$(validate "$out_file.tmp" "$slug"); then
  mv "$out_file.tmp" "$out_file"
else
  # Malformed output is the model misbehaving, not the case failing:
  # TEMPFAIL (retry later), never cached as a rejection.
  tempfail "schema validation failed: $errs (report: $report_file)"
fi

emit location "$out_file"
emit case "$slug"
emit verdict OK
printf 'buglocation: OK %s -> %s\n' "$CASE_LABEL" "$out_file"
