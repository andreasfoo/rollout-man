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
	"encoding/json"
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

	mu      sync.Mutex
	results []Result
	done    map[string]bool
	dropped map[string]bool
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
	if err := r.writeManifest(); err != nil {
		return err
	}
	if len(r.done) > 0 {
		r.logf("resuming: %d trials already recorded", len(r.done))
	}

	cases, err := r.perCase(ctx)
	if err != nil {
		return err
	}
	trials := expand(&r.File.Experiment, cases)
	r.logf("matrix: %d trials across %d cases%s", len(trials), len(cases), deterministicNote(&r.File.Experiment))

	r.runAll(ctx, trials)

	if err := r.Finish(ctx); err != nil {
		return err
	}
	return nil
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
		Pinned  bool   `json:"pinned,omitempty"`
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
			e.Form, e.Uses, e.Pinned = "uses", c.Uses, c.SHA256 != ""
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

func (r *Runner) perCase(ctx context.Context) ([]*casesrc.Case, error) {
	ex := &r.File.Experiment
	var out []*casesrc.Case
	for _, raw := range ex.Cases {
		ref := raw.Merge(ex.CaseDefaults)
		c, err := r.Res.Resolve(ctx, ref)
		if err != nil {
			return nil, fmt.Errorf("resolve %s: %w", ref.Label(), err)
		}
		r.logf("case %s -> %s (pinned %s)", c.Label, c.SHA256[:12], c.PinnedAt)

		actx := r.actionCtx(actions.PerCase)
		actx.CaseLabel, actx.CaseDir, actx.CaseSHA = c.Label, c.Dir, c.SHA256
		actx.Probe = func(ctx context.Context, kind string, n int) ([]float64, error) {
			return r.probe(ctx, c, rexec.AgentKind(kind), n)
		}
		if err := actions.RunList(ctx, actx, ex.Pipeline.PerCase, r.Cmds); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, nil
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
			switch {
			case res.OK() && res.Dropped:
				r.logf("%s: reward %.3f (%.1fs, dropped)", t.ID, *res.Reward, res.Seconds)
			case res.OK():
				r.logf("%s: reward %.3f (%.1fs)", t.ID, *res.Reward, res.Seconds)
			default:
				r.logf("%s: %s %s", t.ID, res.Code, firstLine(res.Message))
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

func (r *Runner) actionCtx(scope actions.Scope) *actions.Ctx {
	return &actions.Ctx{
		Scope: scope, Experiment: r.File.Experiment.Name,
		RunID: filepath.Base(r.Dir), RunDir: r.Dir,
		Cmds: r.Cmds, Log: r.Log,
		Trials:  r.snapshot,
		Dropped: r.isDropped,
	}
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
