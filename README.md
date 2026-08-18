# rollout-man

An orchestration layer for large-scale agent evaluation. It answers **when** a rollout runs
(queueing), **where** it runs (placement), **what** runs (experiment matrix), **how it went**
(state and failure attribution), and **whether the artifacts are safe to hand out** (redaction and
packaging).

It does not run agents itself — [Harbor](#) does that. rollout-man decides what Harbor should run,
on which machine, in what order, and what happens to the output.

> **Status: design stage.** No code yet. The full design lives in
> [`docs/design-v0.4.md`](docs/design-v0.4.md) — 16 sections with inline `[MVP]` / `[P1]` / `[P2]`
> delivery markers.

---

## Topology

```text
            ┌─────────────┐   REST /api/v1
            │  CLI / Web  │──────────────┐
            └─────────────┘              ▼
                                ┌──────────────────┐      ┌──────────────────────┐
                                │    API Server    │─SQL──│  PostgreSQL          │
                                │  registry        │      │  app db (read model) │
                                │  read model      │      │  temporal db         │
                                │  placement matcher│     └──────────────────────┘
                                │  workflow worker  │
                                └───────┬──────────┘
                     StartWorkflow / Signal│gRPC
                                          ▼
                                ┌──────────────────┐
                                │  Temporal Server │  namespace: rollout-man
                                └───────┬──────────┘
              long-poll (outbound gRPC 7233, NAT-friendly)
        ┌───────────────────┬───────────┴────────┬───────────────────┐
        ▼                   ▼                    ▼                   ▼
  workflow worker      activity worker      activity worker     activity worker
  queue: orchestrator  queue: runner.01     queue: runner.02    queue: runner.03
  (in API Server)      (in Runner Agent)    (in Runner Agent)   (in Runner Agent)
                                 │
                                 ▼
                     ┌──────────────────────┐
                     │   External storage   │  reached through configured
                     │  logs / artifacts /  │  shell commands, not an SDK
                     │    case archives     │  (rclone → OneDrive, S3, …)
                     └──────────────────────┘
```

Three ideas carry most of the design:

- **Temporal owns "do every step, in order, without losing anything."** The hand-written state
  machine, lease/reclaim logic, leader election, heartbeat protocol and cascading cancel are all
  gone — they are the parts that are hardest to get right, and a durable-execution engine already
  solves them. PostgreSQL steps down from source of truth to read model.
- **Placement owns "who goes first, and on which machine."** Resource accounting, priority, aging
  and affinity stay hand-written, because that is the actual domain logic. One Temporal task queue
  per runner (`runner.<id>`) pins every step of a trial to the machine placement picked, so the
  whole trial shares one local workdir.
- **Everything external is a shell command.** Object storage and git sources are reached through
  command templates you configure, not through vendored SDKs. rollout-man never reads, stores or
  forwards your GitHub or OneDrive credentials — the command finds its own.

---

## The experiment file

This is the primary interface. One YAML declares the matrix, the admission gate that runs before
it, and the post-processing that runs after every trial.

```yaml
apiVersion: v1
kind: Experiment
name: spring-cve-comparison

cases:
  - {id: case_123, version: v17}

matrix:
  agents:
    - name: claude-code
      llm_spec: opus-prod              # per-agent override of matrix.llm_specs
      parameters: {max_tokens: 65536}
    - name: codex
    - name: oracle                     # builtin: no LLM, no cartesian product
  llm_specs: [opus-prod, gpt5-prod]    # named entries in the LLM Spec registry
  trials: 10

concurrency: 8                         # in-flight cap (queued + executing)
priority: normal                       # critical | high | normal | low
queue_timeout: 24h                     # give up waiting for a runner → UNPLACED

# ── pipeline: the steps around the main chain ───────────────────────────
#   pipeline.pre    once per case version, at confirm time
#   ── main chain (per trial): fetch → prepare → run → verify → collect
#   pipeline.post   per trial, before any artifact leaves the runner
#   ── cleanup
# ────────────────────────────────────────────────────────────────────────
pipeline:
  pre:
    - step: admission                  # gate: is this case even measurable?
      require: admitted                # admitted (default) | any (debug only)
      auto_admit: false
      criteria:
        oracle: {min_reward: 1.0}      # reference solution must score full marks
        nop:    {max_reward: 0.0}      # doing nothing must score zero
        trials: 2

  post:                                # ordered; failure handling differs per step
    - step: redact
      keys: required                   # never optional
      ips: {traj: true, logs: false}   # scrub what ships out, keep what you debug with
    - step: bundle
      format: tar.zst
      include: [traj, logs, result]
    - step: upload
      objects: [bundle, result]

retry_policy:
  max_total_attempts: 3                # including the first
  retry_on: [DOCKER_ERROR, NETWORK_ERROR, AGENT_OOM]
```

### Why a gate before, and a scrub after

**Admission** (`pipeline.pre`) exists because a broken case is indistinguishable from a weak agent
once the numbers are in the table. If the environment is broken, the reference solution has rotted,
or the verifier has a hole, every score produced from that case is noise that *looks exactly like a
capability difference*. So a case version is not usable until `oracle` scores full marks and `nop`
scores zero. The check runs through the ordinary execution path, which means passing it also proves
the whole chain works on this cluster.

**Post-processing** (`pipeline.post`) exists because trial output is not shippable as produced. The
trajectory contains the API key almost by construction — the agent was handed one, and it goes into
the prompt. Redaction is therefore a separate step from collection, so that a failed scrub *blocks
the upload* instead of quietly shipping the raw file. Key scrubbing is mandatory and has no switch.
IP scrubbing is tiered by destination: on for the trajectory and result (they leave the team), off
for logs (they are what you debug with, and an IPv4 regex eats version numbers for breakfast).

### What the results look like

rollout-man does not decide what counts as success. It records the reward each trial produced and
why the others did not produce one; where you cut the distribution is an analysis-time question that
changes with what you are asking.

```text
Agent / Model      done  reward: mean median  p25   p75   Agent-Fail  Agent-TO  Env-TO  Infra
Claude / Opus        88          0.71  0.83  0.42  0.95      14         4        3      12
Codex   / GPT-5      92          0.76  0.86  0.51  0.97      12         3        2       8
```

Pass rates are a query, not a stored setting: `rollout-man experiment results exp_182 --pass-at 0.8`.

Note that agent timeouts and environment timeouts are separate columns. Collapsing them and dropping
both — the obvious thing to do — systematically overstates every agent, because an agent that ran
out its own clock is a capability result, not an infrastructure one.

---

## CLI

```text
rollout-man
├── case        upload / register / admit / list / get
├── experiment  create / get / list / cancel / results
├── task        get / retry / cancel
├── trial       get / events / logs
├── queue
├── llm-spec    create / list / get / update / delete
└── runner      list / get / drain / disable
```

## Configuration for external systems

No SDKs, no credential management. Each action is a command template — argv form with `{{.Key}}` /
`{{.LocalPath}}` placeholders, or a shell script reading the same values from the environment:

```yaml
commands:
  timeout: 30m
  max_attempts: 3

  storage_upload:
    run: ["rclone", "copyto", "--", "{{.LocalPath}}", "onedrive:rollout-man/{{.Key}}"]

  storage_download:
    script: |
      set -euo pipefail
      rclone copyto -- "onedrive:rollout-man/${KEY}" "${LOCAL_PATH}"

  source_git:
    script: |
      set -euo pipefail
      git clone --depth 1 --branch "${GIT_REF}" "${GIT_REPO}" "${LOCAL_PATH}"
```

Exit code zero means success. SHA-256 is always computed locally — backends disagree about which
digest they return, and some do not return SHA-256 at all.

---

## Scope

The MVP is deliberately small: one team, **≤3 runners**, **≤500 trials per experiment**, no GPU.
It has to get from one YAML file to one results table; survive a runner dying without losing work;
survive a runner *restarting* without redoing work; run for a week without filling a disk; never
ship a key in an artifact; and never leave a queued trial without an ending.

Explicit non-goals: multi-tenancy, billing, Kubernetes scheduling, cross-region, autoscaling, and a
web UI (Temporal's own UI covers per-trial timelines until there is a reason to build one).

See [`docs/design-v0.4.md` §15](docs/design-v0.4.md) for the full MVP / P1 / P2 breakdown and the
phase plan with acceptance criteria.
