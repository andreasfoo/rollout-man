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

## The submission file

One file, several YAML documents. `rollout-man experiment create rollout.yaml` reads them together:
`Commands` says how external actions are run, `LLMSpec` declares a model, `Experiment` is the run
itself. Nothing here refers to an ID you had to obtain from a previous command — cases name their
location, models name themselves.

```yaml
---
kind: Commands                          # normally in deployment config; inlined here to self-contain
timeout: 30m
max_attempts: 3
source_git:
  script: |
    set -euo pipefail
    git clone --depth 1 --branch "${GIT_REF}" "${GIT_REPO}" "${LOCAL_PATH}"
storage_upload:
  run: ["rclone", "copyto", "--", "{{.LocalPath}}", "onedrive:{{.Key}}"]
storage_download:
  run: ["rclone", "copyto", "--", "onedrive:{{.Key}}", "{{.LocalPath}}"]

---
kind: LLMSpec
name: opus-prod
base_url: https://api.anthropic.com
model: claude-opus-4-7
api_key_env: ANTHROPIC_API_KEY          # the name of a variable, never the value
parameters: {max_tokens: 65536}

---
kind: LLMSpec
name: gpt5-prod
base_url: https://api.openai.com/v1
model: gpt-5
api_key_cmd: ["pass", "show", "eval/openai"]

---
kind: Experiment
name: spring-cve-comparison

case_defaults:                          # cases say where they are, not what they are called
  source: git                           # git | object | local
  repo: https://github.com/org/eval-cases
  ref: main                             # pinned to a commit at confirm time
  fetch: source_git

cases:
  - path: spring/CVE-2026-1234
  - path: apache/CVE-2026-5678
    ref: v2.1
  - source: object
    key: cases/legacy/cve-2025-0001.tar.zst
    sha256: 3f9a1c...                   # given → skip resolve, hit the CAS directly
  - source: local
    path: /data/wip/my-case             # for debugging; rehashed every confirm

matrix:
  agents:
    - name: claude-code
      llm_spec: opus-prod
    - name: codex
    - name: oracle                      # builtin: no LLM, no cartesian product
  llm_specs: [opus-prod, gpt5-prod]
  trials: 10

concurrency: 8                          # in-flight cap (queued + executing)
priority: normal
queue_timeout: 24h                      # stop waiting for a runner → UNPLACED

# ── pipeline: hooks around the main chain. The key names the unit. ──────
pipeline:

  per_case:                             # ← once per case, at confirm, on a runner
    - step: resolve                     # fetch → sha256 → CAS → parse task.toml
      on_unchanged: skip
    - step: admission                   # is this case even measurable?
      require: admitted
      criteria:
        oracle: {min_reward: 1.0}       # the reference solution must score full marks
        nop:    {max_reward: 0.0}       # doing nothing must score zero
        trials: 2

  # ──────────────────────────────────────────────────────────────────
  #  Once every case passes, the matrix expands into N trials. Each
  #  trial's main chain — fetch → prepare → run → verify → collect — is
  #  built in and not configurable. per_trial steps run right after it,
  #  on the same runner.
  # ──────────────────────────────────────────────────────────────────

  per_trial:                            # ← once per trial, on that trial's runner
    - step: redact
      keys: required                    # never optional
      ips: {traj: true, logs: false}    # scrub what ships, keep what you debug with
    - step: bundle
      format: tar.zst
      include: [traj, logs, result]
    - step: stage                       # hold locally; per_experiment ships it
      max_pending: 20Gi                 # per runner; exceeding it ships early
      on_cancel: upload
    # To ship per trial instead, swap that step for:
    #   - step: upload
    #     using: storage_upload
    #     dest: "evals/{{.ExperimentName}}/{{.CasePath}}/{{.TrialID}}/"
    #     objects: [bundle, result]

  per_experiment:                       # ← once, after every trial is terminal
    - step: upload                      # each runner ships its staged artifacts in one go
      using: storage_upload
      dest: "evals/{{.ExperimentName}}/"
      objects: [bundle, result]
    - step: report                      # [P1]
      formats: [json, csv]
      dest: "evals/{{.ExperimentName}}/report/"
    - step: deploy                      # [P1] publish, notify, hand off downstream
      run: ["./scripts/publish.sh", "{{.ExperimentID}}", "{{.ReportURL}}"]
      on_failure: warn

retry_policy:
  max_total_attempts: 3                 # including the first
  retry_on: [DOCKER_ERROR, NETWORK_ERROR, AGENT_OOM]
```

### Three units, one per block

The pipeline key names its own unit — a step runs at the scale of the block it sits in, and nothing
is declared in one place but executed in another:

```text
experiment                                              ← submitted once
│
├── per_case          × N_cases        at confirm, on a runner
│     resolve → admission                               all must pass before queueing
│
├── trial             × N_trials       the main chain, built in
│     fetch → prepare → run → verify → collect
│   └── per_trial     × N_trials       right after it, same runner
│         redact → bundle → (stage | upload)
│   └── cleanup
│
└── per_experiment    × 1              after every trial is terminal
      upload(staged) → report → deploy
```

`per_case` runs on a runner rather than the server because resolving a case can mean cloning several
gigabytes; bytes never pass through the API server.

### Batching the uploads

Where you put `upload` decides whether artifacts ship per trial or in bulk — there is no separate
mode flag. `upload` in `per_trial` ships each trial as it finishes. `stage` in `per_trial` plus
`upload` in `per_experiment` holds them on the runner and ships one packed upload per runner: 500
trials × 2 objects becomes three command invocations instead of a thousand, which matters a lot
against a backend that throttles per request.

The cost is real and worth stating: staged artifacts occupy runner disk until the experiment ends,
so the staging directory has to be pinned against the housekeeper (which would otherwise reclaim it
as an expired workdir — correctly, by its own rules), counted in placement's disk accounting, capped
by `max_pending`, and shipped before drain completes and before a cancel finishes. And if a runner
dies ungracefully, its staged artifacts are gone; rewards and failure attribution survive in
Postgres, logs and trajectories do not. Those experiments are marked `ARTIFACTS_INCOMPLETE` rather
than quietly looking complete. Move `upload` back into `per_trial` when that trade is not
acceptable.

### Why a gate before, and a scrub after

**Admission** (`per_case`) exists because a broken case is indistinguishable from a weak agent
once the numbers are in the table. If the environment is broken, the reference solution has rotted,
or the verifier has a hole, every score produced from that case is noise that *looks exactly like a
capability difference*. So a case version is not usable until `oracle` scores full marks and `nop`
scores zero. The check runs through the ordinary execution path, which means passing it also proves
the whole chain works on this cluster.

**Post-processing** (`per_trial`) exists because trial output is not shippable as produced. The
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
├── case        resolve / admit / list / get
├── experiment  create / get / list / cancel / results
├── task        get / retry / cancel
├── trial       get / events / logs
├── queue
├── llm-spec    list / get          read-only; specs come from the submitted YAML
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

## Running the MVP

```bash
go build -o rollout-man ./cmd/rollout-man

# resolve the cases a file refers to: fetch, hash, store, parse task.toml
./rollout-man case resolve test/smoke/smoke.yaml

# expand the matrix without running anything
./rollout-man experiment create test/smoke/smoke.yaml --dry-run

# run it
export ROLLOUT_MAN_DSN='postgres:///rollout_man?sslmode=disable'
./rollout-man experiment create test/smoke/smoke.yaml --executor local
./rollout-man experiment results <experiment-id> --pass-at 0.8
```

`test/smoke/run.sh` is the end-to-end smoke test — 23 assertions covering
resolve determinism, both admission verdicts, matrix expansion, the redaction
tiers, staged batch upload, and the read model. It needs Go, a reachable
PostgreSQL, and `CAP_SYS_ADMIN`; it does **not** need a Docker daemon.

### Executors

| executor | how it runs a trial |
|---|---|
| `docker` | builds the case's `environment/Dockerfile`, runs agent and verifier in containers |
| `local` | runs the same case scripts inside a private mount namespace with `/app`, `/logs`, `/solution` and `/tests` bind-mounted from a per-trial sandbox |

`local` exists so the orchestration can be exercised without a daemon. The
absolute paths cases hardcode still resolve, and the host environment is
deliberately **not** inherited — case scripts are untrusted and their output
becomes an artifact.

### What is not built yet

- **Temporal.** The orchestration currently runs in-process: sequential steps,
  a semaphore for concurrency, and an attempt loop for retries. The durability
  the design leans on — surviving a crash mid-trial, resuming from the last
  completed step — is not there yet.
- **Placement.** One runner, no resource accounting, no queue. `--runner` is a
  label on the read model.
- **Multi-runner staging.** `per_experiment: upload` flushes this process's
  staging directory; with several runners it has to fan out per queue.

Everything else in the pipeline — resolve, CAS, admission, matrix, the main
chain, redaction, bundling, staging, upload, report, deploy, and the Postgres
read model — is implemented and exercised by the smoke test.

## Scope

The MVP is deliberately small: one team, **≤3 runners**, **≤500 trials per experiment**, no GPU.
It has to get from one YAML file to one results table; survive a runner dying without losing work;
survive a runner *restarting* without redoing work; run for a week without filling a disk; never
ship a key in an artifact; and never leave a queued trial without an ending.

Explicit non-goals: multi-tenancy, billing, Kubernetes scheduling, cross-region, autoscaling, and a
web UI (Temporal's own UI covers per-trial timelines until there is a reason to build one).

See [`docs/design-v0.4.md` §15](docs/design-v0.4.md) for the full MVP / P1 / P2 breakdown and the
phase plan with acceptance criteria.
