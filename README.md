# rollout-man

A deterministic pipeline for agent evaluation: **orchestrate** the work,
**execute** it, **report** what happened, **ship** the artifacts.

It does not run agents itself — [Harbor](#) cases do that, via their own
`Dockerfile`, `solution/solve.sh` and `tests/test.sh`. rollout-man decides what
runs, makes sure the numbers mean something, and hands you the results.

> **Status: working MVP.** One binary, two dependencies, no database and no
> workflow engine. `docs/design-v0.4.md` describes where this is headed;
> §15.0 explains what was deliberately left out of the MVP and why.

---

## The four things it does

```
orchestrate   resolve each case to a content hash → gate on admission →
              expand case × agent × llm_spec × trials into a fixed trial list

execute       per trial: build the environment → run the agent → run the
              verifier → take the reward

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

## Executors

| executor | how it runs a trial |
|---|---|
| `docker` | builds the case's `environment/Dockerfile`, runs agent and verifier in containers |
| `local` | runs the same case scripts inside a private mount namespace with `/app`, `/logs`, `/solution` and `/tests` bind-mounted from a per-trial sandbox |

`local` exists so the pipeline can be exercised without a daemon. The absolute
paths cases hardcode still resolve, and the host environment is deliberately
**not** inherited — case scripts are untrusted and their output becomes an
artifact.

## Credentials

rollout-man manages none. `api_key_env` and `api_key_cmd` say *where to find* a
key; the value is read on the machine that needs it and never stored. Storage
and VCS credentials belong to whatever command you configured — `rclone.conf`,
`~/.ssh`, `gh auth`. There is nothing here to rotate or revoke.

## Tests

```bash
go test ./internal/...
test/smoke/run.sh        # 28 assertions: the whole pipeline end to end
test/smoke/resume.sh     # 6 assertions: kill a run mid-trial, re-run, resume
```

Both need `CAP_SYS_ADMIN` for the local executor. Neither needs Docker.

## Scope

One machine, one run at a time. No placement across runners, no queue, no
priorities, no web UI, no multi-tenancy. `docs/design-v0.4.md` §15 has the
staged plan for what comes next and what each stage is actually for.
