# rollout-man

A deterministic pipeline for agent evaluation: **orchestrate** the work,
**execute** it, **report** what happened, **ship** the artifacts.

It does not run anything itself. **Harbor** answers *how* a trial runs — the
agent runtime, the container runtime, the verifier, the case format.
rollout-man answers *what runs, when, and whether the number means anything*,
and reaches Harbor through a command you configure.

> **Status: working MVP.** One binary, two dependencies, no database and no
> workflow engine. `docs/design-v0.4.md` describes where this is headed;
> §15.0 explains what was deliberately left out of the MVP and why.

---

## The four things it does

```
orchestrate   resolve each case to a content hash → gate on admission →
              expand case × agent × llm_spec × trials into a fixed trial list

execute       per trial: hand the case and the agent to Harbor, read back the
              reward, the failure code and the artifacts

report        append one line per trial to results.jsonl; aggregate on demand

ship          hand the run directory to a command you configure
```

The trial list is a pure function of the resolved cases and the matrix, so the
same file always produces the same trials with the same ids. That is what
"deterministic" buys, and it is also what makes resuming free: a trial whose id
is already in `results.jsonl` has happened.

```bash
go build -o rollout-man ./cmd/rollout-man

./rollout-man cases  experiment.yaml           # what will run, and from which bytes
./rollout-man run    experiment.yaml --id nightly
./rollout-man status runs/nightly --pass-at 0.8 --failures
./rollout-man ship   runs/nightly experiment.yaml
```

Interrupting a run is safe: re-run with the same `--id` and it picks up the
trials that are missing.

---

## The submission file

One file, several YAML documents. `Commands` says how external things are run,
`LLMSpec` declares a model, `Experiment` is the run. Nothing here refers to an
id you had to obtain from a previous command: cases name their location, models
name themselves.

```yaml
---
kind: Commands
timeout: 30m
max_attempts: 2

source_git:
  script: |
    set -euo pipefail
    git clone --quiet --depth 1 --branch "${GIT_REF}" "${GIT_REPO}" "${LOCAL_PATH}"

ship:
  run: ["rclone", "copyto", "--", "{{.LocalPath}}", "onedrive:{{.Key}}"]

# an agent is just another command; it gets LLM_BASE_URL / LLM_MODEL /
# LLM_API_KEY in its environment
agent_claude-code:
  run: ["/opt/agents/claude-code/run.sh"]

---
kind: LLMSpec
name: opus-prod
base_url: https://api.anthropic.com
model: claude-opus-4-7
api_key_env: ANTHROPIC_API_KEY          # the name of a variable, never the value

---
kind: Experiment
name: spring-cve

case_defaults:                          # cases say where they are
  source: git                           # git | local
  repo: https://github.com/org/eval-cases
  ref: main                             # recorded as the commit it resolved to

cases:
  - path: spring/CVE-2026-1234
  - path: apache/CVE-2026-5678
    ref: v2.1

matrix:
  agents:
    - name: claude-code
      llm_spec: opus-prod
    - name: oracle                      # builtin: no LLM, no cartesian product
    - name: nop
  llm_specs: [opus-prod]
  trials: 10

concurrency: 4
max_attempts: 2                         # retries only what could plausibly differ

# the key names the unit: per_case runs once per case, per_trial once per
# trial, per_experiment once at the end
pipeline:
  per_case:
    admission:                          # is this case even measurable?
      require: admitted                 # admitted (default) | any
      oracle_min_reward: 1.0            # the reference solution must score full marks
      nop_max_reward: 0.0               # doing nothing must score zero
      trials: 2

  per_trial:
    redact:
      keys: required                    # not optional, no switch
      ips: {traj: true, logs: false}    # scrub what ships, keep what you debug with

  per_experiment:
    ship:
      using: ship
      dest: "evals/{{.Experiment}}/{{.RunID}}"
```

### Why a gate before, and a scrub after

**Admission** exists because a broken case is indistinguishable from a weak
agent once the numbers are in the table. If the environment is broken, the
reference solution has rotted, or the verifier has a hole, every score from that
case is noise that *looks exactly like a capability difference*. So a case is
not usable until `oracle` scores full marks and `nop` scores zero. The check
runs through the ordinary execution path, which means passing it also proves the
whole chain works on this machine.

**Redaction** exists because trial output is not shippable as produced: the
trajectory contains the API key almost by construction, since the agent was
handed one and it goes into the prompt. Key scrubbing is mandatory and has no
switch, and a scrub that fails blocks the artifacts rather than shipping them
anyway. IP scrubbing is tiered by destination — on for the trajectory and
result, which leave the team; off for logs, which are what you debug with and
where an IPv4 regex eats version numbers for breakfast.

### What the results look like

rollout-man does not decide what counts as success. It records the reward each
trial produced and why the others produced none; where you cut the distribution
is an analysis-time question that changes with what you are asking.

```
runs/nightly

Agent          LLM Spec      done     mean  median     p25     p75   not measured   pass@0.80
claude-code    opus-prod       88    0.712   0.830   0.420   0.950   HOST_ERROR=2   62/90 = 69%
oracle         -               10    1.000   1.000   1.000   1.000   -              10/10 = 100%
```

Pass rates are a query (`--pass-at 0.8`), not a stored setting. And the
denominator only includes the agent's own failures: infrastructure trouble is
ours, and counting it would quietly mark every agent down for our bad day.

---

## Running a trial

rollout-man does not run containers. Building the case image, starting it,
running the agent inside it and running the verifier are **Harbor's** job —
rollout-man's job is to decide *what* to run, ask for it, and record what came
back. Reaching Harbor is a configured command, exactly like storage is:

```yaml
kind: Commands

harbor:
  script: |
    harbor run --case "$CASE_DIR" --agent "$AGENT_NAME" --output "$OUT_DIR" ...
```

```
in    CASE_DIR OUT_DIR TRIAL_ID AGENT_KIND AGENT_NAME AGENT_COMMAND
      AGENT_USER VERIFIER_USER AGENT_TIMEOUT_SEC VERIFIER_TIMEOUT_SEC
      BUILD_TIMEOUT_SEC CPUS MEMORY_MB STORAGE_MB GPUS ALLOW_INTERNET
      LLM_BASE_URL LLM_MODEL LLM_API_KEY

out   $OUT_DIR/reward.txt    the score — the only thing that means "measured"
      $OUT_DIR/failure.txt   first line a failure code, rest an explanation
      $OUT_DIR/*             artifacts to keep (traj.jsonl, agent.log, …)
```

Every limit `task.toml` declares is handed over, because the adapter is the only
thing that can enforce them — it is the thing that started the case.

The answer is read back in order of specificity: a declared failure code, then a
number, then the exit status. An adapter that dies **without** saying why is
`ENV_FAILED`, never an agent code: "could not measure" is ours, and putting it
in the agent's denominator corrupts the number quietly.

`--executor` names the command (`--executor harbor`); `auto` picks up a command
named `harbor` if the submission declares one.

```bash
pip install harbor        # or: uv tool install harbor
rollout-man run experiment.yaml     # --executor auto finds the harbor command
```

`adapters/harbor.sh` is the real one, over `harbor run`. It lets Harbor enforce
every timeout and resource limit `task.toml` declares, and maps Harbor's
exception types onto the taxonomy — `AgentTimeoutError` and
`NonZeroAgentExitCodeError` are the agent's, a sandbox that would not build or a
verifier that produced no number are not.

`test/smoke/fake-harbor.sh` is a stand-in used by the smoke test so it can run
with no Harbor and no daemon.

## The local executor

`--executor local` is the one exception, and it is a **test fixture, not a
runtime**: it runs the same case scripts in a private mount namespace so the
orchestration can be exercised on a machine with no Harbor and no daemon. The
absolute paths cases hardcode still resolve, and the host environment is
deliberately **not** inherited — case scripts are untrusted and their output
becomes an artifact. Its timeouts kill the whole process group, so an agent that
outruns its clock releases the slot then, not when it happens to finish.

## Credentials

rollout-man manages none. `api_key_env` and `api_key_cmd` say *where to find* a
key; the value is read on the machine that needs it and never stored. Storage
and VCS credentials belong to whatever command you configured — `rclone.conf`,
`~/.ssh`, `gh auth`. There is nothing here to rotate or revoke.

## Tests

```bash
go test ./internal/...
test/smoke/run.sh        # 61 assertions: the whole pipeline end to end
test/smoke/resume.sh     # 6 assertions: kill a run mid-trial, re-run, resume
```

`run.sh` covers the four verbs, the admission gate, the redaction tiers, and
two things worth naming:

- **every failure code, on purpose** — an agent that exits non-zero, an agent
  that outruns its clock, a case that cannot be staged, a verifier that
  produces no number, and a retryable failure that succeeds on the second
  attempt. The check that matters is the last line: only the agent's own
  failures land in the denominator.
- **the trial adapter**, against a stand-in Harbor: the case's limits reach it,
  the score and artifacts come back from it, a declared failure code passes
  through, and an adapter that dies silently is `ENV_FAILED` rather than
  anything that would count against the agent.
- **the real seam** — the same submission and the same gate, but the trial runs
  through `harbor run` in a container Harbor built.
- **a real Harbor security case** — libaom's AV1 entropy-decoder
  heap-buffer-overflow, oracle to 1.0 and nop to 0.0, asserted down to the ASan
  report and the crashing frame the case names. Nothing here can be satisfied by
  a plausible-looking artifact: the verifier scores from its own gdb backtrace,
  so the deliverable has to actually crash the target.

Steps 1–9 need `CAP_SYS_ADMIN` for the mount namespace and nothing else. Steps
10–11 need `harbor` and Docker, and report `SKIP` without them — the summary
counts skips separately, so a skipped step never reads as a passing one.

## Scope

One machine, one run at a time. No placement across runners, no queue, no
priorities, no web UI, no multi-tenancy. `docs/design-v0.4.md` §15 has the
staged plan for what comes next and what each stage is actually for.
