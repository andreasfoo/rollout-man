// Package run is the orchestration: expand the matrix, gate on admission, run
// each trial, append what happened, ship the artifacts.
//
// The trial list is a pure function of the resolved cases and the matrix, so
// the same file always produces the same trials with the same ids. That is what
// makes a run deterministic -- and it is also what makes resuming trivial: a
// trial whose id is already in results.jsonl has already happened.
package run

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/andreasfoo/rollout-man/internal/actions"
	"github.com/andreasfoo/rollout-man/internal/casesrc"
	"github.com/andreasfoo/rollout-man/internal/cmdrun"
	"github.com/andreasfoo/rollout-man/internal/config"
	rexec "github.com/andreasfoo/rollout-man/internal/exec"
	"github.com/andreasfoo/rollout-man/internal/fail"
	"github.com/andreasfoo/rollout-man/internal/progress"
	"github.com/andreasfoo/rollout-man/internal/redact"
)

// Result is one line of results.jsonl: everything known about one trial.
type Result struct {
	TrialID   string       `json:"trial_id"`
	Case      string       `json:"case"`
	CaseSHA   string       `json:"case_sha256"`
	Agent     string       `json:"agent"`
	LLMSpec   string       `json:"llm_spec,omitempty"`
	Index     int          `json:"index"`
	Attempts  int          `json:"attempts"`
	Reward    *float64     `json:"reward,omitempty"`
	Code      fail.Code    `json:"failure_code,omitempty"`
	Category  string       `json:"failure_category,omitempty"`
	Message   string       `json:"failure_message,omitempty"`
	Redaction *redact.Hits `json:"redaction,omitempty"`
	Seconds   float64      `json:"seconds"`
	At        string       `json:"at"`

	// Dropped means a guard decided this trial's artifacts should not be
	// published. The measurement above still stands: dropping is about what
	// leaves the machine, never about what was observed.
	Dropped bool           `json:"dropped,omitempty"`
	Notes   map[string]any `json:"notes,omitempty"`
}

func (r *Result) note(k string, v any) {
	if r.Notes == nil {
		r.Notes = map[string]any{}
	}
	r.Notes[k] = v
}

func (r Result) OK() bool { return r.Reward != nil }

type trial struct {
	ID      string
	Case    *casesrc.Case
	Agent   string
	Kind    rexec.AgentKind
	LLMSpec string
	Index   int
}

type Runner struct {
	File *config.File
	Cmds *cmdrun.Runner
	Exec rexec.Executor
	Res  *casesrc.Resolver
	Dir  string // runs/<run-id>
	Log  func(string, ...any)
	// Regate ignores a cached per_case verdict and runs the gate again. The
	// cache is keyed by what is written down -- the case's bytes and the
	// checks' configuration -- and cannot see everything that decides an
	// outcome, so there has to be a way to say "ask again".
	Regate bool

	mu       sync.Mutex
	results  []Result
	done     map[string]bool
	dropped  map[string]bool
	progress *progress.Tracker

	// gateCache remembers which cases have already been through per_case
	// (quality audit, admission, ...) and what it decided.
	// Unlike results.jsonl this covers the per_case gate itself: admission
	// probes and adapters like acc-quality-audit are not trials, so without
	// this a rerun under the same --id would redo the slowest part of the
	// pipeline for every case even though only ship failed downstream.
	gateCache map[string]gateCacheEntry
	casesSeen int
	wrote     map[string]bool
}

func (r *Runner) logf(f string, a ...any) {
	if r.Log != nil {
		r.Log(f, a...)
	}
}

func (r *Runner) Run(ctx context.Context) error {
	if err := os.MkdirAll(r.Dir, 0o755); err != nil {
		return err
	}
	if err := r.Validate(); err != nil {
		return err
	}
	if err := r.loadDone(); err != nil {
		return err
	}
	r.loadGateCache()
	if err := r.writeManifest(); err != nil {
		return err
	}
	if len(r.done) > 0 {
		r.logf("resuming: %d trials already recorded", len(r.done))
	}

	// The closing report is written on every exit path, not only the happy
	// one: a batch that stopped halfway is the one whose record matters most,
	// and it is also the one nobody is around to ask for.
	var runErr error
	defer func() { r.closeOut(ctx, runErr) }()

	cases, err := r.perCase(ctx)
	if err != nil {
		runErr = err
		return err
	}
	trials := expand(&r.File.Experiment, cases)
	r.logf("matrix: %d trials across %d cases%s", len(trials), len(cases), deterministicNote(&r.File.Experiment))

	labels := make([]string, len(trials))
	for i, t := range trials {
		labels[i] = t.Case.Label
	}
	r.progress = progress.New(r.Dir, r.File.Experiment.Name, filepath.Base(r.Dir), labels)
	var resumed []string
	for _, t := range trials {
		if r.done[t.ID] {
			resumed = append(resumed, t.Case.Label)
		}
	}
	r.progress.Resumed(resumed, r.dropped)

	stop := r.heartbeat(ctx)
	r.runAll(ctx, trials)
	stop()
	r.progress.Close()

	if err := r.Finish(ctx); err != nil {
		runErr = err
		return err
	}
	return nil
}

// closeOut writes the run's closing report. The report is the last link of
// every pipeline, not an ordinary step in one: a batch that stopped at the
// gate, or whose ship step failed, is exactly the batch whose record matters
// most, and it is also the one nobody is around to ask for.
//
// It reuses the submission's own report configuration when it declared one, so
// the closing report is the report that was asked for -- same destination,
// same format, same pass@ -- rather than a default written over it.
func (r *Runner) closeOut(ctx context.Context, runErr error) {
	a := config.Action{Uses: "report"}
	for _, s := range r.File.Experiment.Pipeline.PerExperiment {
		if s.Uses == "report" {
			a = s
			break
		}
	}
	dest := a.Str("dest", "report.md")
	if !filepath.IsAbs(dest) {
		dest = filepath.Join(r.Dir, dest)
	}

	// Two ways to stop halfway. An error says so itself; a Ctrl-C does not --
	// the trials it interrupted are recorded as host errors, honestly enough,
	// but nothing in the table says the batch never got to the end.
	outcome := ""
	switch {
	case runErr != nil:
		outcome = firstLine(runErr.Error())
	case ctx.Err() != nil:
		outcome = "interrupted before every trial ran"
	}

	r.mu.Lock()
	already := r.wrote[dest]
	r.mu.Unlock()
	// A report the declared step already wrote is left alone -- unless the run
	// stopped after writing it, in which case that file says the batch finished
	// and it did not. Then it is written again, with the outcome named.
	if already && outcome == "" {
		return
	}

	c := r.actionCtx(actions.PerExperiment)
	c.Outcome = outcome
	act, err := actions.Resolve(a, r.Cmds)
	if err != nil {
		return
	}
	// Ctrl-C cancels the run, and the run is what the report is about: it must
	// still be written after the context that carried the work is gone.
	if err := act.Run(context.WithoutCancel(ctx), c, a); err != nil {
		r.logf("closing report: %v", err)
	}
}

// writeManifest records what this run was actually made of: which commands
// were in play, where their code lives, and its hash. A result you cannot trace
// back to the code that produced it is a number without a provenance -- and the
// hash is also what makes "the adapter changed under us" visible afterwards
// rather than only at the moment it happened.
func (r *Runner) writeManifest() error {
	type cmd struct {
		Name    string `json:"name"`
		Form    string `json:"form"` // uses | run | script
		Uses    string `json:"uses,omitempty"`
		SHA256  string `json:"sha256,omitempty"`
		Problem string `json:"problem,omitempty"`
	}
	m := struct {
		Experiment string `json:"experiment"`
		RunID      string `json:"run_id"`
		Executor   string `json:"executor"`
		Trusted    bool   `json:"commands_from_trusted_file"`
		At         string `json:"at"`
		Commands   []cmd  `json:"commands"`
	}{
		Experiment: r.File.Experiment.Name,
		RunID:      filepath.Base(r.Dir),
		Trusted:    r.File.Commands.Trusted,
		At:         time.Now().UTC().Format(time.RFC3339),
	}
	if r.Exec != nil {
		m.Executor = r.Exec.Name()
	}
	for _, name := range sortedCmdNames(r.File.Commands.Cmds) {
		c := r.File.Commands.Cmds[name]
		e := cmd{Name: name, Form: "script"}
		switch {
		case c.Uses != "":
			e.Form, e.Uses = "uses", c.Uses
			sum, err := cmdrun.Pin(c)
			if err != nil {
				e.Problem = err.Error()
			} else {
				e.SHA256 = sum
			}
		case len(c.Run) > 0:
			e.Form = "run"
		}
		m.Commands = append(m.Commands, e)
	}
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(r.Dir, "manifest.json"), append(b, '\n'), 0o644)
}

func sortedCmdNames(m map[string]config.Command) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// ------------------------------------------------------------- per_case ---

// gateCacheEntry is one case's outcome from a previous run of per_case, keyed
// so that it is only ever reused when both the case and the checks are
// provably the same as when it was recorded.
type gateCacheEntry struct {
	// Fingerprint covers the per_case pipeline config itself (uses:, with:,
	// on_failure:, and every command it can resolve to, including sha256
	// pins and inline scripts). A changed check invalidates every entry that
	// used it, not just the one that happens to be re-hashed by hand.
	Fingerprint string `json:"fingerprint"`
	// Admitted is false when this case was rejected (on_failure: skip) last
	// time; a rejection is exactly as cacheable as an admission; it is still
	// the same expensive audit landing on the same answer.
	Admitted bool `json:"admitted"`
}

// caseCounter numbers cases as they are gated. The gate can be the slowest
// part of a batch -- an audit and a repair per case -- so "how many are there
// and how far in are we" has to be answerable during it, not only after.
func (r *Runner) caseCounter() string {
	total := len(r.File.Experiment.Cases)
	if total <= 1 {
		return ""
	}
	r.mu.Lock()
	r.casesSeen++
	n := r.casesSeen
	r.mu.Unlock()
	return fmt.Sprintf("%d/%d ", n, total)
}

// gateCachePath sits beside the runs, not inside one. A gate verdict is bound
// to the case's content hash and the checks' fingerprint -- neither of which
// has anything to do with which run asked. Keeping it per run directory meant
// it only ever helped a resume of the same --id, while every fresh batch redid
// the audits it was written to avoid.
func (r *Runner) gateCachePath() string {
	return filepath.Join(filepath.Dir(r.Dir), "gate-cache.json")
}

func (r *Runner) loadGateCache() {
	r.gateCache = map[string]gateCacheEntry{}
	b, err := os.ReadFile(r.gateCachePath())
	if err != nil {
		return
	}
	_ = json.Unmarshal(b, &r.gateCache)
}

func (r *Runner) saveGateCache() {
	r.mu.Lock()
	defer r.mu.Unlock()
	b, err := json.MarshalIndent(r.gateCache, "", "  ")
	if err != nil {
		return
	}
	// By rename, like progress.json: a crash mid-write would otherwise leave a
	// truncated cache that silently re-gates every case on the next run.
	tmp := r.gateCachePath() + ".tmp"
	if os.WriteFile(tmp, b, 0o644) == nil {
		_ = os.Rename(tmp, r.gateCachePath())
	}
}

// perCaseFingerprint hashes everything that changing would make a cached
// per_case verdict wrong: the step list itself and, for every step that
// resolves to a configured command, that command's script/uses-file pin.
// A case's own content is not part of this -- it is folded in separately, per
// case, via its SHA256.
func perCaseFingerprint(ex *config.Experiment, cmds map[string]config.Command, executor string) string {
	h := sha256.New()
	enc := json.NewEncoder(h)
	_ = enc.Encode(ex.Pipeline.PerCase)
	// The execution chain is part of the verdict. Admission probes run through
	// the executor, and passing the gate is supposed to prove the whole chain
	// works here -- so a verdict reached through one executor says nothing
	// about another. Both the resolved name (--executor can override the
	// file) and the step itself go in.
	_ = enc.Encode(executor)
	seen := map[string]bool{}
	steps := append([]config.Action{}, ex.Pipeline.PerCase...)
	if x, err := ex.Pipeline.Executor(); err == nil {
		_ = enc.Encode(x)
		steps = append(steps, x)
	}
	for _, a := range steps {
		for _, uses := range []string{a.Uses, a.Fix} {
			if uses == "" || seen[uses] {
				continue
			}
			seen[uses] = true
			if cmd, ok := cmds[uses]; ok {
				_ = enc.Encode(cmd)
			}
		}
	}
	return hex.EncodeToString(h.Sum(nil))
}

// perCase runs every case through the gate concurrently, bounded by
// pipeline.per_case_concurrency (default 1: serial, the behavior every
// existing experiment.yaml was written against). Results land in a
// per-index slot rather than an appended slice so that the output order is
// the case order regardless of which goroutine finishes first -- a batch's
// admitted list should not depend on scheduling luck. The first error
// cancels a shared context so cases still queued behind the semaphore do
// not start slow work (a quality-audit subagent, a harbor probe) whose
// answer will be thrown away.
func (r *Runner) perCase(ctx context.Context) ([]*casesrc.Case, error) {
	ex := &r.File.Experiment
	cases, err := casesrc.Expand(ex.Cases, ex.CaseDefaults, r.File.Near)
	if err != nil {
		return nil, err
	}
	ex.Cases = cases
	n := len(ex.Cases)
	admitted := make([]*casesrc.Case, n)
	errs := make([]error, n)

	gctx, cancel := context.WithCancel(ctx)
	defer cancel()
	sem := make(chan struct{}, ex.Pipeline.PerCaseConcurrency)
	var wg sync.WaitGroup
	for i, raw := range ex.Cases {
		wg.Add(1)
		go func(i int, raw config.CaseRef) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			if gctx.Err() != nil {
				return
			}

			c, isAdmitted, err := r.GateOne(gctx, raw)
			if err != nil {
				errs[i] = err
				cancel() // a hard failure aborts every case still queued behind sem
				return
			}
			if isAdmitted {
				admitted[i] = c
			}
		}(i, raw)
	}
	wg.Wait()

	for _, err := range errs {
		if err != nil {
			return nil, err
		}
	}
	var out []*casesrc.Case
	for _, c := range admitted {
		if c != nil {
			out = append(out, c)
		}
	}
	return out, nil
}

// GateOne resolves one case and runs it through pipeline.per_case (quality
// audit, admission, ...), using and updating the persistent gate cache
// exactly as perCase's own loop does for every case in an experiment. This is
// the shared entry point for a full run's per_case stage and for `watch`,
// which gates cases one at a time as it discovers them.
//
// admitted is false when the case was cleanly rejected (an on_failure: skip
// step); err is non-nil only for a hard failure -- resolve failing, or a
// per_case step failing without on_failure: skip.
func (r *Runner) GateOne(ctx context.Context, raw config.CaseRef) (c *casesrc.Case, admitted bool, err error) {
	ex := &r.File.Experiment
	fp := perCaseFingerprint(ex, r.File.Commands.Cmds, r.executorName())

	// Run() calls loadGateCache before perCase's goroutines start, so this is
	// only ever nil for a caller (watch) that never went through Run -- and
	// watch calls GateOne one case at a time, so this lazy init races with
	// nothing.
	if r.gateCache == nil {
		r.loadGateCache()
	}

	ref := raw.Merge(ex.CaseDefaults)
	c, err = r.Res.Resolve(ctx, ref)
	if err != nil {
		return nil, false, fmt.Errorf("resolve %s: %w", ref.Label(), err)
	}
	r.logf("case %s%s -> %s (pinned %s)", r.caseCounter(), c.Label, c.SHA256[:12], c.PinnedAt)

	key := c.SHA256
	r.mu.Lock()
	cached, ok := r.gateCache[key]
	r.mu.Unlock()
	if ok && cached.Fingerprint == fp && !r.Regate {
		r.logf("case %s: per_case gate cached (%s)", c.Label,
			map[bool]string{true: "admitted", false: "rejected"}[cached.Admitted])
		return c, cached.Admitted, nil
	}

	actx := r.actionCtx(actions.PerCase)
	actx.CaseLabel, actx.CaseDir, actx.CaseSHA = c.Label, c.Dir, c.SHA256
	actx.Probe = func(ctx context.Context, kind string, n int) ([]float64, error) {
		return r.probe(ctx, c, rexec.AgentKind(kind), n)
	}
	isAdmitted := true
	gateErr := actions.RunList(ctx, actx, ex.Pipeline.PerCase, r.Cmds)

	// A fix step edits the case in place, so the bytes that will be measured
	// are not necessarily the bytes that were hashed above. Re-hash before
	// anything records a version: "the content hash is the version" only means
	// something if it is the hash of what actually ran. Recording the
	// pre-repair hash would publish provenance pointing at bytes no trial ever
	// saw -- and it would also mean this case's gate result could never be
	// found again, because the next run hashes the repaired directory.
	if sha, herr := casesrc.HashDir(c.Dir); herr == nil && sha != c.SHA256 {
		r.logf("case %s: repaired in place, %s -> %s", c.Label, c.SHA256[:12], sha[:12])
		c.SHA256 = sha
	}

	if gateErr != nil {
		if !errors.Is(gateErr, actions.ErrSkipCase) {
			return c, false, gateErr
		}
		r.logf("case %s: skipped by per_case check (on_failure: skip)", c.Label)
		isAdmitted = false
	}
	entry := gateCacheEntry{Fingerprint: fp, Admitted: isAdmitted}
	r.mu.Lock()
	// Under both hashes: the repaired bytes so the next run finds this verdict
	// without redoing the audit, and the original so restoring the directory
	// does not re-run a repair that has already been judged.
	r.gateCache[key] = entry
	r.gateCache[c.SHA256] = entry
	r.mu.Unlock()
	r.saveGateCache()
	return c, isAdmitted, nil
}

// probe runs an admission probe through the ordinary execution path. Going the
// long way round is the point: passing the gate also proves the whole chain
// works on this machine.
func (r *Runner) probe(ctx context.Context, c *casesrc.Case, kind rexec.AgentKind, n int) ([]float64, error) {
	var got []float64
	for i := 1; i <= n; i++ {
		t := &trial{ID: fmt.Sprintf("admit-%s-%s-%d", c.SHA256[:12], kind, i),
			Case: c, Agent: string(kind), Kind: kind, Index: i}
		res := r.once(ctx, t)
		if !res.OK() {
			// res.Message already carries the code.
			return nil, fmt.Errorf("%s", res.Message)
		}
		got = append(got, *res.Reward)
	}
	return got, nil
}

// --------------------------------------------------------------- matrix ---

func expand(ex *config.Experiment, cases []*casesrc.Case) []*trial {
	var out []*trial
	for _, c := range cases {
		for _, a := range ex.Matrix.Agents {
			kind := rexec.LLM
			switch a.Name {
			case "oracle":
				kind = rexec.Oracle
			case "nop":
				kind = rexec.Nop
			}
			specs := []string{""}
			if kind == rexec.LLM {
				specs = ex.Matrix.LLMSpecs
				if a.LLMSpec != "" {
					specs = []string{a.LLMSpec}
				}
				if len(specs) == 0 {
					specs = []string{""}
				}
			}
			n := a.Rollouts(ex.Matrix.Trials)
			for _, s := range specs {
				for i := 1; i <= n; i++ {
					out = append(out, &trial{
						ID: trialID(c, a.Name, s, i), Case: c,
						Agent: a.Name, Kind: kind, LLMSpec: s, Index: i,
					})
				}
			}
		}
	}
	return out
}

// deterministicNote says out loud when an agent got fewer rollouts than
// matrix.trials asked for. Quietly running something a different number of
// times than the file says is worse than the cost it saves.
func deterministicNote(ex *config.Experiment) string {
	var names []string
	for _, a := range ex.Matrix.Agents {
		if a.Deterministic() && a.Trials == nil && ex.Matrix.Trials > 1 {
			names = append(names, a.Name)
		}
	}
	if len(names) == 0 {
		return ""
	}
	return fmt.Sprintf(" (%s ran once: deterministic)", strings.Join(names, ", "))
}

func trialID(c *casesrc.Case, agent, spec string, i int) string {
	id := slug(c.Label) + "-" + slug(agent)
	if spec != "" {
		id += "-" + slug(spec)
	}
	return fmt.Sprintf("%s-%d", id, i)
}

// heartbeat speaks while trials are in flight. Without it the log goes quiet
// for as long as the slowest step takes, which on a real case is an hour --
// and a batch that says nothing is indistinguishable from one that is stuck.
func (r *Runner) heartbeat(ctx context.Context) func() {
	every := 15 * time.Second
	done := make(chan struct{})
	go func() {
		tick := time.NewTicker(every)
		defer tick.Stop()
		for {
			select {
			case <-done:
				return
			case <-ctx.Done():
				return
			case now := <-tick.C:
				for _, line := range r.progress.Snapshot().Lines(time.Minute, now) {
					r.logf("%s", line)
				}
			}
		}
	}()
	return func() { close(done) }
}

// --------------------------------------------------------------- trials ---

// runAll uses two separate widths on purpose. Running a trial is bounded by how
// many containers the machine can hold; processing one afterwards -- scrub,
// guard, pack -- is bounded by nothing of the sort. Holding a container slot
// while zipping a directory wastes the scarce resource on the cheap work, so
// the executor slot is released before the post-trial steps begin.
func (r *Runner) runAll(ctx context.Context, trials []*trial) {
	exec := make(chan struct{}, r.File.Experiment.Concurrency)
	post := make(chan struct{}, r.File.Experiment.Pipeline.Concurrency)
	var wg sync.WaitGroup
	for _, t := range trials {
		if r.done[t.ID] {
			continue
		}
		wg.Add(1)
		go func(t *trial) {
			defer wg.Done()
			if ctx.Err() != nil {
				return
			}
			exec <- struct{}{}
			r.progress.Step(t.ID, t.Case.Label, r.executorName())
			res := r.attempts(ctx, t)
			<-exec

			post <- struct{}{}
			outDir := filepath.Join(r.Dir, "trials", t.ID, "out")
			if err := r.postTrial(ctx, t, &res, outDir, r.secretsFor(ctx, t)); err != nil {
				if res.OK() {
					// A post step that fails on a measured trial is a real
					// failure: it is what stands between the artifacts and
					// whoever receives them.
					code := fail.Of(err)
					res.Reward = nil
					res.Code, res.Category, res.Message = code, string(code.Category()), err.Error()
				} else {
					r.logf("%s: post-trial steps on a failed trial: %v", t.ID, err)
				}
			}
			<-post

			r.append(res)
			r.progress.Finish(t.ID, t.Case.Label, res.OK(), res.Dropped)
			snap := r.progress.Snapshot()
			at := fmt.Sprintf("[%d/%d]", snap.Done, snap.Total)
			switch {
			case res.OK() && res.Dropped:
				r.logf("%s %s: reward %.3f (%.1fs, dropped)", at, t.ID, *res.Reward, res.Seconds)
			case res.OK():
				r.logf("%s %s: reward %.3f (%.1fs)", at, t.ID, *res.Reward, res.Seconds)
			default:
				r.logf("%s %s: %s %s", at, t.ID, res.Code, firstLine(res.Message))
			}
		}(t)
	}
	wg.Wait()
}

// secretsFor re-resolves the values that must not survive into an artifact.
// Resolving again rather than threading them out of the executor keeps the key
// out of every intermediate struct it would otherwise pass through.
func (r *Runner) secretsFor(ctx context.Context, t *trial) []string {
	if t.Kind != rexec.LLM || t.LLMSpec == "" {
		return nil
	}
	llm, err := r.resolveLLM(ctx, t.LLMSpec)
	if err != nil || llm.APIKey == "" {
		return nil
	}
	return []string{llm.APIKey}
}

// attempts retries only what could plausibly succeed on a second run. An agent
// that failed on its own merits is a result, not an incident.
func (r *Runner) attempts(ctx context.Context, t *trial) Result {
	var res Result
	max := r.File.Experiment.MaxAttempts
	for n := 1; n <= max; n++ {
		res = r.once(ctx, t)
		res.Attempts = n
		if res.OK() || !res.Code.Retryable() || n == max || ctx.Err() != nil {
			break
		}
		r.logf("%s: %s, retrying (%d/%d)", t.ID, res.Code, n+1, max)
	}
	return res
}

func (r *Runner) once(ctx context.Context, t *trial) Result {
	start := time.Now()
	res := Result{
		TrialID: t.ID, Case: t.Case.Label, CaseSHA: t.Case.SHA256,
		Agent: t.Agent, LLMSpec: t.LLMSpec, Index: t.Index, Attempts: 1,
		At: time.Now().UTC().Format(time.RFC3339),
	}
	env := &rexec.CaseEnv{
		CaseDir: t.Case.Dir,
		WorkDir: filepath.Join(r.Dir, "trials", t.ID),
		Cfg:     t.Case.Config,
	}
	finish := func(err error) Result {
		code := fail.Of(err)
		res.Code, res.Category, res.Message = code, string(code.Category()), err.Error()
		res.Seconds = time.Since(start).Seconds()
		return res
	}

	as := rexec.AgentSpec{Kind: t.Kind, Name: t.Agent}
	if t.Kind == rexec.LLM {
		if c, ok := r.File.Commands.Cmds["agent_"+t.Agent]; ok {
			as.Command = commandArgv(c)
		}
		if t.LLMSpec != "" {
			llm, err := r.resolveLLM(ctx, t.LLMSpec)
			if err != nil {
				return finish(err)
			}
			as.LLM = llm
		}
	}

	reward, err := r.Exec.Trial(ctx, env, as)
	if err != nil {
		return finish(err)
	}
	writeResultJSON(env.OutDir(), t, reward)
	res.Reward = &reward
	res.Seconds = time.Since(start).Seconds()
	return res
}

// postTrial runs everything in per_trial after the executor: scrub, guard,
// pack, whatever the submission asked for. It is where a trial stops being a
// number and becomes an artifact somebody else can read.
func (r *Runner) postTrial(ctx context.Context, t *trial, res *Result, outDir string, secrets []string) error {
	steps := r.File.Experiment.Pipeline.PostTrial()
	if len(steps) == 0 {
		return nil
	}
	c := r.actionCtx(actions.PerTrial)
	c.Secrets = secrets
	c.Enter = func(step string) { r.progress.Step(t.ID, t.Case.Label, step) }
	c.Trial = &actions.Trial{
		ID: t.ID, Case: t.Case.Label, CaseSHA: t.Case.SHA256, Agent: t.Agent,
		LLMSpec: t.LLMSpec, Index: t.Index, Reward: res.Reward, Code: string(res.Code),
		Seconds: res.Seconds, OutDir: outDir,
	}
	err := actions.RunList(ctx, c, steps, r.Cmds)
	if c.Drop {
		res.Dropped = true
		r.markDropped(t.ID)
	}
	for k, v := range c.Notes {
		if k == "redaction" {
			if h, ok := v.(redact.Hits); ok {
				res.Redaction = &h
				continue
			}
		}
		res.note(k, v)
	}
	return err
}

// ------------------------------------------------------------- actions ---

// executorName is what the first per_trial step is called, which is what a
// trial is doing for most of its life.
func (r *Runner) executorName() string {
	if a, err := r.File.Experiment.Pipeline.Executor(); err == nil {
		return a.Label()
	}
	return "run"
}

func (r *Runner) actionCtx(scope actions.Scope) *actions.Ctx {
	return &actions.Ctx{
		Scope: scope, Experiment: r.File.Experiment.Name,
		RunID: filepath.Base(r.Dir), RunDir: r.Dir,
		Cmds: r.Cmds, Log: r.Log,
		Trials:  r.snapshot,
		Dropped: r.isDropped,
		Wrote:   r.markWrote,
	}
}

func (r *Runner) markWrote(path string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.wrote == nil {
		r.wrote = map[string]bool{}
	}
	r.wrote[path] = true
}

func (r *Runner) markDropped(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.dropped == nil {
		r.dropped = map[string]bool{}
	}
	r.dropped[id] = true
}

func (r *Runner) isDropped(id string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.dropped[id]
}

// snapshot is what a per_experiment action sees: every trial the run recorded,
// including the ones resumed from a previous attempt.
func (r *Runner) snapshot() []actions.Trial {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]actions.Trial, 0, len(r.results))
	for _, res := range r.results {
		out = append(out, actions.Trial{
			ID: res.TrialID, Case: res.Case, CaseSHA: res.CaseSHA, Agent: res.Agent,
			LLMSpec: res.LLMSpec, Index: res.Index, Reward: res.Reward,
			Code: string(res.Code), Seconds: res.Seconds,
			OutDir: filepath.Join(r.Dir, "trials", res.TrialID, "out"),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// ------------------------------------------------------- per_experiment ---

// Finish runs the per_experiment steps: whatever turns a directory of trials
// into something worth handing to someone else.
func (r *Runner) Finish(ctx context.Context) error {
	steps := r.File.Experiment.Pipeline.PerExperiment
	if len(steps) == 0 {
		return nil
	}
	return actions.RunList(ctx, r.actionCtx(actions.PerExperiment), steps, r.Cmds)
}

func (r *Runner) resultsPath() string { return filepath.Join(r.Dir, "results.jsonl") }

// Validate checks every pipeline step before the batch starts. A submission
// with a misspelled input should be rejected now, not three hours in when the
// only step that would have scrubbed the keys turns out to be a no-op.
func (r *Runner) Validate() error {
	p := &r.File.Experiment.Pipeline
	// per_trial[0] is the executor, which --executor may override, so it is not
	// resolved here. Everything after it starts at index 1, and says so.
	for _, u := range []struct {
		scope actions.Scope
		list  []config.Action
		from  int
	}{
		{actions.PerCase, p.PerCase, 0},
		{actions.PerTrial, p.PostTrial(), 1},
		{actions.PerExperiment, p.PerExperiment, 0},
	} {
		if err := actions.ValidateList(u.scope, u.list, u.from, r.Cmds); err != nil {
			return err
		}
	}
	return nil
}

// Restore reloads a finished run so its per_experiment steps can be run again
// against what is already on disk.
func (r *Runner) Restore() error {
	if err := r.Validate(); err != nil {
		return err
	}
	return r.loadDone()
}

func (r *Runner) loadDone() error {
	r.done = map[string]bool{}
	res, err := Load(r.Dir)
	if err != nil {
		return err
	}
	for _, x := range res {
		r.done[x.TrialID] = true
		r.results = append(r.results, x)
		if x.Dropped {
			r.markDropped(x.TrialID)
		}
	}
	return nil
}

// append is the only write of record: one line, flushed, so a crash loses at
// most the trial that was in flight.
func (r *Runner) append(res Result) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.results = append(r.results, res)
	f, err := os.OpenFile(r.resultsPath(), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	b, _ := json.Marshal(res)
	f.Write(append(b, '\n'))
	f.Sync()
}

// Load reads a run's results.
func Load(dir string) ([]Result, error) {
	b, err := os.ReadFile(filepath.Join(dir, "results.jsonl"))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []Result
	for _, line := range strings.Split(string(b), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var r Result
		if err := json.Unmarshal([]byte(line), &r); err != nil {
			return nil, fmt.Errorf("corrupt results.jsonl: %w", err)
		}
		out = append(out, r)
	}
	return out, nil
}

// ---------------------------------------------------------------- utils ---

func (r *Runner) resolveLLM(ctx context.Context, name string) (*rexec.LLMEnv, error) {
	s, ok := r.File.LLMSpecs[name]
	if !ok {
		return nil, fail.New(fail.Host, "unknown llm_spec "+name)
	}
	env := &rexec.LLMEnv{Provider: s.Provider, BaseURL: s.BaseURL, Model: s.Model}
	switch {
	case s.APIKeyEnv != "":
		env.APIKey = os.Getenv(s.APIKeyEnv)
		if env.APIKey == "" {
			// Not HOST_ERROR: an unset variable is a setup mistake, and
			// HOST_ERROR is retryable -- retrying would burn every attempt
			// waiting for an environment that is not going to change.
			return nil, fail.New(fail.EnvFailed,
				"llm_spec "+name+": "+s.APIKeyEnv+" is not set here")
		}
	case len(s.APIKeyCmd) > 0:
		out, err := rexec.Command(ctx, s.APIKeyCmd).Output()
		if err != nil {
			return nil, fail.Wrap(fail.EnvFailed, "llm_spec "+name+": api_key_cmd", err)
		}
		env.APIKey = strings.TrimSpace(string(out))
		if env.APIKey == "" {
			return nil, fail.New(fail.EnvFailed,
				"llm_spec "+name+": api_key_cmd produced an empty key")
		}
	}
	return env, nil
}

func commandArgv(c config.Command) []string {
	switch {
	case c.Uses != "":
		return []string{c.Uses}
	case len(c.Run) > 0:
		return c.Run
	}
	return rexec.ScriptCommand(c.Script)
}

func writeResultJSON(dir string, t *trial, reward float64) {
	b, _ := json.MarshalIndent(map[string]any{
		"trial_id": t.ID, "case": t.Case.Label, "case_sha256": t.Case.SHA256,
		"agent": t.Agent, "llm_spec": t.LLMSpec, "reward": reward,
	}, "", "  ")
	os.WriteFile(filepath.Join(dir, "result.json"), b, 0o644)
}

func slug(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		} else {
			b.WriteByte('-')
		}
	}
	return strings.Trim(b.String(), "-")
}

func verdict(ok bool) string {
	if ok {
		return "PASS"
	}
	return "FAIL"
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

// Sorted returns results in a stable order for reporting.
func Sorted(in []Result) []Result {
	out := append([]Result(nil), in...)
	sort.Slice(out, func(i, j int) bool {
		if out[i].Agent != out[j].Agent {
			return out[i].Agent < out[j].Agent
		}
		if out[i].LLMSpec != out[j].LLMSpec {
			return out[i].LLMSpec < out[j].LLMSpec
		}
		return out[i].TrialID < out[j].TrialID
	})
	return out
}
