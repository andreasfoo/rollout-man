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

`experiments/complete.yaml` is a worked example of everything below in one
file, annotated. The smoke test loads it on every run, so it cannot rot.

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
  trials: 10                            # rollouts per *stochastic* agent

concurrency: 4
max_attempts: 2                         # retries only what could plausibly differ

# The pipeline is a list of steps, one list per unit of work. The key names
# the unit -- which is also the whole answer to "when does the upload happen".
pipeline:
  concurrency: 16                       # how wide post-processing runs

  per_case:                             # once per case
    - uses: admission                   # is this case even measurable?
      with: {oracle_min_reward: 1.0, nop_max_reward: 0.0, trials: 2}

  per_trial:                            # once per trial
    - uses: harbor                      # first step runs the trial

    - uses: redact
      with: {keys: required, ips: {traj: true, logs: false}}

    - uses: guard                       # keep only what is worth publishing
      name: only-hard
      with: {max_reward: 0.6, min_steps: 30}
      on_violation: drop

    - uses: archive
      with: {format: tar}

  per_experiment:                       # once, when the batch is done
    - uses: dataset                     # one row per trial + a card
    - uses: ship
      with: {using: ship_hf, path: dataset, dest: my-org/rollout-libaom}
```

### Actions

A step is `uses` plus its inputs. Six are built in; a `uses` that names none of
them falls back to a configured command, which is how a custom step is written —
no plugin registry, no new syntax, and it inherits the hash pin and env
allowlist that commands already have.

| action | unit | what it does |
|---|---|---|
| `admission` | per_case | refuses a case whose oracle cannot score or whose nop can |
| `redact` | per_trial | keys always, addresses only in what leaves the machine |
| `guard` | per_trial, per_experiment | asserts on `reward` / `steps` / `seconds`, or on `trials` / `shipped` / `mean_reward` |
| `archive` | per_trial | one archive per trial (`tar`, `tar.gz`, `zip`) |
| `dataset` | per_experiment | turns the run into rows plus a card |
| `ship` | per_trial, per_experiment | hands a path to a configured command |

Every action declares the inputs it understands, and one it does not is an error
**before the batch starts**. A misspelled key would otherwise run the step with
its defaults and look like success — which for `redact` means publishing keys.

Two widths, on purpose: `concurrency` bounds how many trials run at once (how
many containers fit on this machine), `pipeline.concurrency` bounds how many are
being scrubbed, packed and shipped (nothing to do with containers). The executor
slot is released before post-processing starts, so zipping a directory never
holds the scarce resource.

### Repairing a case, and what that does to its version

A `per_case` step can name a `fix:` — a command that repairs whatever the check
rejected — and `fix_attempts:` says how many check-repair-check rounds it gets.
More than one is the normal case for a repair that *converges*: an agent asked
to patch what a check flagged often gets part of the way on the first pass.

```yaml
per_case:
  - uses: check_quality
    fix: fix_quality_audit
    fix_writeback: true      # naming a repair is not consent to it editing the case
    fix_attempts: 2
```

**A repair changes the case's bytes, so it changes its version.** The hash is
recomputed after `per_case` and *that* is what gets recorded and published —
"the content hash is the version" only means something if it is the hash of what
actually ran. Recording the hash as found would publish provenance pointing at
content no trial ever saw.

```
case …/duckdb-3f0eb51: repaired in place, 1f2532b2e042 -> 7e6a9818b8e8
```

### Not re-auditing what has already been judged

A gate verdict is bound to the case's content hash and a fingerprint of the
checks — including the executor, because admission probes run *through* it and
a verdict reached one way says nothing about another. Neither of those is a run,
so the cache lives beside `runs/`, not inside one, and a fresh `--id` does not
redo an audit that has already happened.

It can only be keyed on what is written down. Anything else that decides an
outcome — an environment variable, the state of the world — is invisible to it,
so `--regate` runs the gate again regardless.

### Guards, and what dropping means

`guard` asks a different question from `admission`. Admission asks whether a case
can be measured at all; a guard asks whether *this* measurement belongs in what
you publish — "keep only the rollouts the agent found hard" is curation, not
quality control.

`on_violation: drop` keeps the measurement and publishes nothing from it. The
reward is still recorded, still in `results.jsonl`, still in the table; what
changes is that its artifacts do not leave the machine. Dropping decides what
ships, never what was observed. `fail` and `warn` are there for the other case,
where a violation means something is wrong rather than uninteresting.

### Publishing: rows, not a directory tree

`dataset` writes one row per trial (`data/trials.jsonl`) plus a card. That is the
difference between a folder on a server and something `load_dataset()` can open
and a viewer can render. The card carries the provenance the numbers are
worthless without: every case's content hash, the admission verdict, the adapter
that produced the trials.

A note on archive formats, because it is easy to believe more than is true.
`.zip` has **no builder in the `datasets` library at all** — a directory of zips
publishes as opaque blobs, readable only by addressing them explicitly
(`zip://inner::outer.zip`). `.tar` maps to the WebDataset builder, but only when
one tar holds many samples named `<key>.<ext>`; one tar per trial is an
attachment, not a shard. Either way it is the rows, not the archives, that make
this a dataset. At batch sizes where the data is the problem, one archive per
trial is also one LFS object per trial — sharding is the answer there, and
`archive` does not do it.

Dedup, quality filtering and cross-batch statistics are deliberately **not**
actions. That work belongs to `datasets` / `datatrove` running over the whole
corpus, not to an eval runner over one batch. What cannot move downstream is
redaction and guards: a key that reaches a hub is in the git history, in LFS, in
every mirror and in the viewer's cache, and `map()` runs on a dataset that is
already published.

### `trials` counts rollouts, not repetitions

`trials: 10` exists because agents are stochastic: the same agent on the same
case produces a distribution, and one sample of it is not a measurement.

The built-ins are not stochastic. `oracle` runs the case's own `solve.sh` and
`nop` does nothing, so ten rollouts of either produce ten identical numbers and
ten times the container cost. **They run once, however high `trials` goes**, and
the run says so rather than quietly doing something the file did not ask for:

```
matrix: 8 trials across 2 cases (oracle, nop ran once: deterministic)
```

A per-agent `trials:` overrides it, for the rare case where you mean it.

And if what you want from `oracle` is the guarantee that the case scores 1.0 —
that is the **admission gate's** job, and the gate already runs it. Listing
`oracle` in the matrix as well only makes sense when you want its *trail*: the
reference rollout, as a worked example of what a correct one looks like.

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

### Knowing where a batch is

A pipeline of five steps over hour-long trials used to say nothing between
"started" and "finished". Three questions came out of that silence — how much is
there, how far along is it, and what is happening right now — and all three are
answered while it runs:

```
09:11:02  case 2/2 test/cases/libaom-cc9e46cb-bug-6491968-t4 -> 3ab3f15bb196 (pinned git)
09:11:04  matrix: 34 trials across 2 cases
09:11:19  6/34 done · 4 running (harbor×2, archive×1, ship×1) · 1 dropped
09:11:19     libaom 4/17 · spidermonkey 2/17
09:11:19     slowest: libaom…-claude-code-sonnet-7 in harbor for 4m12s
09:11:23  [7/34] libaom…-claude-code-sonnet-3: reward 0.420 (18.3s)
```

The heartbeat names **which step** each in-flight trial is in, so a long wait is
distinguishable from a stuck one — that is what the `slowest:` line is for.

The same state is written to `progress.json` as it changes, so another terminal
can read it without parsing the log:

```bash
rollout-man status runs/nightly                    # works mid-run
rollout-man status runs/nightly --case libaom      # follow one case
```

```
libaom-hf  (running)
  6/34 trials done · 1 dropped

  Case                                          done       of  dropped   failed
  test/cases/libaom-cc9e46cb-bug-6491968-t4        4       17        1        0

  in flight
    …-claude-code-sonnet-7                        harbor       4m12s
    …-claude-code-sonnet-9                        archive         3s
```

It is written by rename, so a reader never catches a half-written file — which
matters precisely because the expected use is one process reading while another
writes.

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

Commands are shared with `include:`, so a submission references the library
rather than restating it:

```yaml
kind: Commands
include: [commands.yaml]     # relative to this file
harbor:                      # only what differs
  uses: adapters/my-harbor.sh
```

Definitions in the including file win. This opens no door that was not already
open: without `--commands` a submission's commands are trusted anyway, and with
it the submission's `Commands` document is refused outright, so the only
includes that ever run are the operator's.

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

## Shipping a batch

`pipeline.per_experiment.ship` runs **once, after every trial in the batch has
finished** — that is what the key name means, and it is the whole answer to
"when does the upload happen". It gets the run directory and a destination, and
what shipping means is the command's business:

```yaml
pipeline:
  per_experiment:
    ship:
      using: ship
      dest: "test/cases/jobs/{{.Experiment}}/{{.RunID}}"
```

`adapters/ship-github.sh` commits the trails into this repo under that path —
each trial's own agent output plus `results.jsonl`, not the whole run directory.
Admission probes are the gate's evidence, not the batch's product, so they stay
behind. `experiments/libaom-trails.yaml` is a complete worked example: one local
case, `oracle` and `claude-code`, trails back into `test/cases/jobs/`.

Nothing about the upload is manual, and no credential belongs to rollout-man:
the command uses whatever git identity and remote auth the machine already has.

## Commands, and keeping them safe

Every external system — running a trial, cloning a case, shipping a batch — is a
command you configure. A command has three forms:

```yaml
harbor:
  run: ["harbor", "run", "--case", "{{.CaseDir}}", "..."]   # argv

ship:
  script: |                                                  # inline shell
    ...

trial:
  uses: adapters/harbor.sh                                   # a file, pinned
  sha256: d822f0cf2ea4...
```

`uses:` is the one that makes commands safer, and it does so through three
things that Actions-style plugins also rely on — none of which is the plugin
packaging itself:

- **The code lives in a file you can review**, not inline in a submission. It is
  a normal script under `adapters/`, diffable and testable on its own.
- **`sha256:` pins it.** If the file on disk stops matching the hash, the run is
  *refused* — not warned about. A control nobody has to notice is not a control.
  Every run also writes a `manifest.json` recording which command ran and its
  hash, so a result traces back to the exact code that produced it.
- **`inherit_env: false` plus `env:`** hands each command only the variables it
  declares. A `ship` command that talks to git does not also get the model
  provider's key; the trial adapter does not get your git credentials.

And the trust boundary itself: `--commands <file>` takes the commands from a
file **the submission cannot override**. A submission that ships its own
`kind: Commands` is refused, not merged. The submitter chooses which steps run
(`using: ship`); the operator chooses what a step *is*. See
`commands.example.yaml`.

## Adapters are executable files, not shell scripts

`uses:` names an **executable**, and the contract is *environment variables in,
files out*. Nothing in it says shell — the runner executes the file directly and
lets its `#!` line decide what runs it, so an adapter can be Python, a compiled
binary, or anything else that can read `$OUT_DIR`.

```yaml
harbor:  {uses: adapters/harbor.sh,     sha256: d822f0cf...}
notify:  {uses: tools/notify.py}
publish: {uses: bin/publish-linux-amd64}
```

The ones shipped here are `sh` because they are thin wrappers around `harbor`,
`hf`, `rclone` and `git` — a few flags and an exit code, which is what shell is
actually good at. That is a choice about those six files, not about the format.

**On portability**, three things are worth separating:

- **What breaks across Linux and macOS is rarely the shell — it is the
  utilities.** `readlink -f`, `sha256sum`, `stat -c`, `date -d`, `base64 -w`,
  `grep -P`, `sed -i` with no argument: GNU spellings that fail or quietly do
  something else on BSD userland. The smoke test greps for them, so an adapter
  cannot pick one up unnoticed. (The shipped adapters use none.)
- **Adapters are glue to one machine's tooling anyway.** `ship-rclone` needs a
  configured `rclone.conf`; `harbor` needs `harbor` and a Docker daemon. The
  script's language is far from the tightest coupling it has.
- **Parts of this are Linux-only regardless of language.** The `local` executor
  and `test/smoke/fake-harbor.sh` use `unshare --mount`; mount namespaces have
  no macOS equivalent. The runner targets Linux, and saying so plainly is better
  than implying the script language is what stands in the way.

## Credentials

rollout-man manages none. `api_key_env` and `api_key_cmd` say *where to find* a
key; the value is read on the machine that needs it and never stored. Storage
and VCS credentials belong to whatever command you configured — `rclone.conf`,
`~/.ssh`, `gh auth`. There is nothing here to rotate or revoke.

## Tests

```bash
go test ./internal/...
test/smoke/run.sh        # 135 assertions: the whole pipeline end to end
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
