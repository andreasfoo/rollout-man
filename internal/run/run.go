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
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

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
	r.logf("matrix: %d trials across %d cases", len(trials), len(cases))

	r.runAll(ctx, trials)

	if r.File.Experiment.Pipeline.PerExperiment.Ship != nil {
		if err := r.Ship(ctx); err != nil {
			return err
		}
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

		if a := ex.Pipeline.PerCase.Admission; a != nil {
			if err := r.admit(ctx, c, a); err != nil {
				return nil, err
			}
		}
		out = append(out, c)
	}
	return out, nil
}

// admit refuses to measure a case whose oracle cannot score and whose nop can.
// A broken case produces numbers that look exactly like a weak agent, so the
// gate is the only place that difference can still be seen.
func (r *Runner) admit(ctx context.Context, c *casesrc.Case, a *config.Admission) error {
	if a.Require == "any" {
		r.logf("case %s: admission skipped (require: any)", c.Label)
		return nil
	}
	probes := []struct {
		kind rexec.AgentKind
		ok   func(float64) bool
		want string
	}{
		{rexec.Oracle, func(v float64) bool { return v >= a.OracleMin-1e-9 },
			fmt.Sprintf(">= %.2f", a.OracleMin)},
		{rexec.Nop, func(v float64) bool { return v <= a.NopMax+1e-9 },
			fmt.Sprintf("<= %.2f", a.NopMax)},
	}
	for _, p := range probes {
		var got []float64
		for i := 1; i <= a.Trials; i++ {
			t := &trial{ID: fmt.Sprintf("admit-%s-%s-%d", c.SHA256[:12], p.kind, i),
				Case: c, Agent: string(p.kind), Kind: p.kind, Index: i}
			res := r.once(ctx, t)
			if !res.OK() {
				// a probe that could not run is not a verdict
				// res.Message already carries the code.
				return fmt.Errorf("admission %s for %s could not run: %s",
					p.kind, c.Label, res.Message)
			}
			got = append(got, *res.Reward)
		}
		pass := true
		for _, v := range got {
			if !p.ok(v) {
				pass = false
			}
		}
		r.logf("case %s: %s %v want %s -> %s", c.Label, p.kind, got, p.want, verdict(pass))
		if !pass {
			return fmt.Errorf("CASE_NOT_ADMITTED: %s (%s scored %v, wanted %s)",
				c.Label, p.kind, got, p.want)
		}
	}
	return nil
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
			for _, s := range specs {
				for i := 1; i <= ex.Matrix.Trials; i++ {
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

func trialID(c *casesrc.Case, agent, spec string, i int) string {
	id := slug(c.Label) + "-" + slug(agent)
	if spec != "" {
		id += "-" + slug(spec)
	}
	return fmt.Sprintf("%s-%d", id, i)
}

// --------------------------------------------------------------- trials ---

func (r *Runner) runAll(ctx context.Context, trials []*trial) {
	sem := make(chan struct{}, r.File.Experiment.Concurrency)
	var wg sync.WaitGroup
	for _, t := range trials {
		if r.done[t.ID] {
			continue
		}
		wg.Add(1)
		go func(t *trial) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			if ctx.Err() != nil {
				return
			}
			res := r.attempts(ctx, t)
			r.append(res)
			if res.OK() {
				r.logf("%s: reward %.3f (%.1fs)", t.ID, *res.Reward, res.Seconds)
			} else {
				r.logf("%s: %s %s", t.ID, res.Code, firstLine(res.Message))
			}
		}(t)
	}
	wg.Wait()
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
	var secrets []string
	// A failed trial is the one you most need to read, and its logs are the
	// ones most likely to hold a key: scrub on every exit, not just the happy
	// one.
	finish := func(err error) Result {
		if rc := r.File.Experiment.Pipeline.PerTrial.Redact; rc != nil {
			if hits, serr := r.scrub(env.OutDir(), rc, secrets); serr == nil {
				res.Redaction = &hits
			}
		}
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
			if llm.APIKey != "" {
				secrets = append(secrets, llm.APIKey)
			}
		}
	}

	reward, err := r.Exec.Trial(ctx, env, as)
	if err != nil {
		return finish(err)
	}
	writeResultJSON(env.OutDir(), t, reward)

	if rc := r.File.Experiment.Pipeline.PerTrial.Redact; rc != nil {
		hits, err := r.scrub(env.OutDir(), rc, secrets)
		if err != nil {
			return finish(err)
		}
		res.Redaction = &hits
	}

	res.Reward = &reward
	res.Seconds = time.Since(start).Seconds()
	return res
}

// ------------------------------------------------------------ per_trial ---

var distributable = map[string]redact.Class{
	"traj.jsonl":  redact.Distributable,
	"result.json": redact.Distributable,
}

// scrub is mandatory for keys and tiered for IPs: what ships out gets its
// addresses removed, what you debug with keeps them. A scrub that fails blocks
// the artifacts entirely -- shipping an unscrubbed trajectory cannot be undone.
func (r *Runner) scrub(dir string, rc *config.Redact, secrets []string) (redact.Hits, error) {
	s := redact.New(secrets, rc.Extra, map[redact.Class]bool{
		redact.Distributable: rc.IPs["traj"],
		redact.Debug:         rc.IPs["logs"],
	})
	var total redact.Hits
	// Walk, not glob: an adapter is free to leave a directory of trajectories,
	// and the one thing that must not happen is artifacts shipping unscrubbed
	// because they were one level deeper than we looked.
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		class := redact.Debug
		if c, ok := distributable[d.Name()]; ok {
			class = c
		}
		h, serr := s.ScrubFile(p, class)
		if serr != nil {
			rel, _ := filepath.Rel(dir, p)
			return fail.Wrap(fail.RedactFailed, "scrub "+rel, serr)
		}
		total.Exact += h.Exact
		total.Pattern += h.Pattern
		total.IP += h.IP
		return nil
	})
	if err != nil {
		return total, err
	}
	return total, nil
}

// ------------------------------------------------------- per_experiment ---

// ship hands the whole run directory to a command. What "shipping" means is the
// operator's business, so it is one configured command and nothing more.
func (r *Runner) Ship(ctx context.Context) error {
	s := r.File.Experiment.Pipeline.PerExperiment.Ship
	if s == nil {
		return fail.New(fail.Host, "the experiment declares no pipeline.per_experiment.ship")
	}
	using := s.Using
	if using == "" {
		using = "ship"
	}
	if !r.Cmds.Has(using) {
		return fail.New(fail.Host, "pipeline.per_experiment.ship needs a command named "+using)
	}
	dest := strings.ReplaceAll(s.Dest, "{{.Experiment}}", r.File.Experiment.Name)
	dest = strings.ReplaceAll(dest, "{{.RunID}}", filepath.Base(r.Dir))
	if _, err := r.Cmds.Run(ctx, using, map[string]string{
		"LocalPath": r.Dir, "Key": dest,
	}); err != nil {
		return fail.Wrap(fail.Host, "ship", err)
	}
	r.logf("shipped %s -> %s", r.Dir, dest)
	return nil
}

// ---------------------------------------------------------------- state ---

func (r *Runner) resultsPath() string { return filepath.Join(r.Dir, "results.jsonl") }

func (r *Runner) loadDone() error {
	r.done = map[string]bool{}
	res, err := Load(r.Dir)
	if err != nil {
		return err
	}
	for _, x := range res {
		r.done[x.TrialID] = true
		r.results = append(r.results, x)
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
