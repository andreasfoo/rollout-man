// Package pipeline runs an experiment: per_case, then the trials, then
// per_experiment. Each block executes at the scale its name says.
package pipeline

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/andreasfoo/rollout-man/internal/bundle"
	"github.com/andreasfoo/rollout-man/internal/cas"
	"github.com/andreasfoo/rollout-man/internal/cmdrun"
	rexec "github.com/andreasfoo/rollout-man/internal/exec"
	"github.com/andreasfoo/rollout-man/internal/failure"
	"github.com/andreasfoo/rollout-man/internal/resolve"
	"github.com/andreasfoo/rollout-man/internal/sanitize"
	"github.com/andreasfoo/rollout-man/internal/spec"
	"github.com/andreasfoo/rollout-man/internal/store"
)

type Engine struct {
	File     *spec.File
	Runner   *cmdrun.Runner
	Res      *resolve.Resolver
	Exec     rexec.Executor
	Store    *store.DB
	WorkRoot string
	RunnerID string
	ExpID    string
	Log      func(string, ...any)
}

func (e *Engine) logf(f string, a ...any) {
	if e.Log != nil {
		e.Log(f, a...)
	}
}

type Task struct {
	ID         string
	Case       *resolve.CaseVersion
	Agent      spec.AgentRef
	Kind       rexec.AgentKind
	LLMSpec    string
	Trials     int
	Unadmitted bool
}

type trialResult struct {
	TrialID string
	Reward  float64
	Code    failure.Code
	Err     error
}

func (e *Engine) Run(ctx context.Context) error {
	ex := &e.File.Experiment

	if err := e.Store.CreateExperiment(ctx, e.ExpID, ex.Name, ex); err != nil {
		return err
	}
	for _, s := range e.File.LLMSpecs {
		if err := e.Store.UpsertLLMSpec(ctx, s.Name, s.Provider, s.BaseURL, s.Model,
			s.APIKeyEnv, s.APIKeyCmd, s.MaxConcurrent, s.Parameters); err != nil {
			return err
		}
	}

	cases, err := e.perCase(ctx)
	if err != nil {
		e.Store.FinishExperiment(ctx, e.ExpID, "FAILED")
		return err
	}

	tasks := e.expand(cases)
	total := 0
	for _, t := range tasks {
		total += t.Trials
		if err := e.Store.CreateTask(ctx, t.ID, e.ExpID, t.Case.SHA256, t.Case.Label,
			t.Agent.Name, t.LLMSpec, t.Trials); err != nil {
			return err
		}
	}
	e.logf("matrix: %d tasks / %d trials (concurrency %d)", len(tasks), total, ex.Concurrency)
	if total > 500 {
		return fmt.Errorf("MATRIX_TOO_LARGE: %d trials exceeds the MVP cap of 500", total)
	}

	e.runTrials(ctx, tasks)

	if err := e.perExperiment(ctx); err != nil {
		e.Store.FinishExperiment(ctx, e.ExpID, "ARTIFACTS_INCOMPLETE")
		return err
	}
	return e.Store.FinishExperiment(ctx, e.ExpID, "COMPLETED")
}

// ------------------------------------------------------------- per_case ---

func (e *Engine) perCase(ctx context.Context) ([]*resolve.CaseVersion, error) {
	ex := &e.File.Experiment
	resolveStep := ex.Pipeline.PerCaseStep("resolve")
	admitStep := ex.Pipeline.PerCaseStep("admission")

	var out []*resolve.CaseVersion
	for _, raw := range ex.Cases {
		ref := raw.Merge(ex.CaseDefaults)
		if resolveStep == nil {
			return nil, fmt.Errorf("pipeline.per_case has no resolve step")
		}
		cv, err := e.Res.Resolve(ctx, ref)
		size := int64(0)
		if cv.SHA256 != "" {
			if fi, serr := os.Stat(e.Res.Store.Path(cv.SHA256)); serr == nil {
				size = fi.Size()
			}
		}
		e.Store.UpsertCaseVersion(ctx, cv.SHA256, cv.Label, cv.Source, cv.PinnedAt,
			string(cv.State), cv.Error, cv.Cfg, size)
		if err != nil {
			return nil, fmt.Errorf("resolve %s: %w", ref.Label(), err)
		}
		hit := "fetched"
		if cv.CacheHit {
			hit = "cache hit"
		}
		e.logf("per_case resolve: %s -> %s (%s, pinned %s)", cv.Label, short(cv.SHA256), hit, cv.PinnedAt)
		e.Store.Event(ctx, e.ExpID, "", "CASE_RESOLVED",
			map[string]any{"case": cv.Label, "sha256": cv.SHA256, "pinned": cv.PinnedAt, "cache_hit": cv.CacheHit})

		if admitStep != nil {
			if err := e.admit(ctx, cv, admitStep); err != nil {
				return nil, err
			}
		}
		out = append(out, cv)
	}
	return out, nil
}

func (e *Engine) admit(ctx context.Context, cv *resolve.CaseVersion, st *spec.Step) error {
	crit := st.Criteria
	if crit == nil {
		crit = &spec.Criteria{Trials: 2}
		crit.Oracle.MinReward = 1.0
		crit.Nop.MaxReward = 0.0
	}
	if crit.Trials <= 0 {
		crit.Trials = 2
	}
	require := st.Require
	if require == "" {
		require = "admitted"
	}
	if require == "any" && !st.AutoAdmit {
		cv.State = resolve.Ready
		e.logf("per_case admission: %s SKIPPED (require: any) -- results will be marked UNADMITTED", cv.Label)
		return nil
	}

	type probe struct {
		kind rexec.AgentKind
		want string
		ok   func(float64) bool
	}
	probes := []probe{
		{rexec.Oracle, fmt.Sprintf(">= %.2f", crit.Oracle.MinReward), func(r float64) bool { return r >= crit.Oracle.MinReward-1e-9 }},
		{rexec.Nop, fmt.Sprintf("<= %.2f", crit.Nop.MaxReward), func(r float64) bool { return r <= crit.Nop.MaxReward+1e-9 }},
	}

	result := map[string]any{"criteria": crit}
	verdict := resolve.Admitted
	for _, p := range probes {
		var rewards []float64
		for i := 1; i <= crit.Trials; i++ {
			tr := e.runTrial(ctx, &Task{
				ID:   "admit-" + short(cv.SHA256) + "-" + string(p.kind),
				Case: cv, Agent: spec.AgentRef{Name: string(p.kind)}, Kind: p.kind, Trials: crit.Trials,
			}, i, true)
			if tr.Err != nil {
				// a trial that could not produce a measurement is not a verdict
				return fmt.Errorf("admission %s for %s could not run: %w", p.kind, cv.Label, tr.Err)
			}
			rewards = append(rewards, tr.Reward)
		}
		pass := true
		for _, r := range rewards {
			if !p.ok(r) {
				pass = false
			}
		}
		result[string(p.kind)] = map[string]any{"rewards": rewards, "want": p.want, "pass": pass}
		e.logf("per_case admission: %s %s rewards=%v want %s -> %s",
			cv.Label, p.kind, rewards, p.want, passWord(pass))
		if !pass {
			verdict = resolve.Rejected
		}
	}

	cv.State = verdict
	e.Store.SetAdmission(ctx, cv.SHA256, string(verdict), result)
	e.Store.Event(ctx, e.ExpID, "", "CASE_ADMISSION", map[string]any{"case": cv.Label, "verdict": verdict})
	if verdict == resolve.Rejected && require == "admitted" {
		return fmt.Errorf("CASE_NOT_ADMITTED: %s failed the admission gate", cv.Label)
	}
	return nil
}

// --------------------------------------------------------------- matrix ---

func (e *Engine) expand(cases []*resolve.CaseVersion) []*Task {
	ex := &e.File.Experiment
	var tasks []*Task
	for _, cv := range cases {
		for _, a := range ex.Matrix.Agents {
			kind := rexec.LLM
			switch a.Name {
			case "oracle":
				kind = rexec.Oracle
			case "nop":
				kind = rexec.Nop
			}
			if kind != rexec.LLM {
				// builtins take no llm_spec and are not multiplied by it
				tasks = append(tasks, &Task{
					ID:   taskID(e.ExpID, cv, a.Name, ""),
					Case: cv, Agent: a, Kind: kind, Trials: ex.Matrix.Trials,
					Unadmitted: cv.State != resolve.Admitted,
				})
				continue
			}
			specs := ex.Matrix.LLMSpecs
			if a.LLMSpec != "" {
				specs = []string{a.LLMSpec}
			}
			if len(specs) == 0 {
				specs = []string{""}
			}
			for _, s := range specs {
				tasks = append(tasks, &Task{
					ID:   taskID(e.ExpID, cv, a.Name, s),
					Case: cv, Agent: a, Kind: kind, LLMSpec: s, Trials: ex.Matrix.Trials,
					Unadmitted: cv.State != resolve.Admitted,
				})
			}
		}
	}
	return tasks
}

// ------------------------------------------------------------- trials -----

func (e *Engine) runTrials(ctx context.Context, tasks []*Task) {
	sem := make(chan struct{}, e.File.Experiment.Concurrency)
	var wg sync.WaitGroup
	for _, t := range tasks {
		e.Store.SetTaskState(ctx, t.ID, "RUNNING")
		for i := 1; i <= t.Trials; i++ {
			wg.Add(1)
			go func(t *Task, i int) {
				defer wg.Done()
				sem <- struct{}{}
				defer func() { <-sem }()
				e.runTrial(ctx, t, i, false)
			}(t, i)
		}
	}
	wg.Wait()
	for _, t := range tasks {
		e.Store.SetTaskState(ctx, t.ID, "COMPLETED")
	}
}

func (e *Engine) runTrial(ctx context.Context, t *Task, index int, admission bool) trialResult {
	trialID := fmt.Sprintf("%s-%d", t.ID, index)
	maxAttempts := e.File.Experiment.Retry.MaxTotalAttempts
	var last trialResult
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		res := e.attempt(ctx, t, trialID, index, attempt, admission)
		last = res
		if res.Err == nil {
			return res
		}
		if !res.Code.Retryable() || attempt == maxAttempts {
			break
		}
		e.logf("trial %s attempt %d failed (%s), retrying", trialID, attempt, res.Code)
		time.Sleep(time.Duration(attempt) * time.Second)
	}
	return last
}

func (e *Engine) attempt(ctx context.Context, t *Task, trialID string, index, attempt int, admission bool) trialResult {
	work := filepath.Join(e.WorkRoot, "trials", trialID, fmt.Sprintf("attempt-%d", attempt))
	if err := os.MkdirAll(work, 0o755); err != nil {
		return trialResult{TrialID: trialID, Code: failure.HostError, Err: err}
	}
	env := &rexec.CaseEnv{CaseDir: t.Case.Dir, WorkDir: work, Cfg: t.Case.Cfg}

	if !admission {
		e.Store.StartTrial(ctx, trialID, t.ID, index, attempt, e.RunnerID)
	}
	start := time.Now()

	fail := func(err error) trialResult {
		code := failure.FromError(err)
		if !admission {
			e.Store.FailTrial(ctx, trialID, string(code.Category()), string(code), err.Error(),
				map[string]any{"duration_s": time.Since(start).Seconds()})
			e.Store.Event(ctx, e.ExpID, trialID, "TRIAL_FAILED", map[string]any{"code": code})
		}
		e.Exec.Cleanup(ctx, env)
		return trialResult{TrialID: trialID, Code: code, Err: err}
	}

	agentSpec := rexec.AgentSpec{Kind: t.Kind, Name: t.Agent.Name}
	if t.Kind == rexec.LLM {
		// A real agent is just another configured command: agent_<name>.
		if c, ok := e.File.Commands.Cmds["agent_"+t.Agent.Name]; ok {
			if len(c.Run) > 0 {
				agentSpec.Command = c.Run
			} else if c.Script != "" {
				agentSpec.Command = rexec.ScriptCommand(c.Script)
			}
		}
	}
	var secrets []string
	if t.Kind == rexec.LLM && t.LLMSpec != "" {
		llm, err := e.resolveLLM(ctx, t.LLMSpec)
		if err != nil {
			return fail(err)
		}
		agentSpec.LLM = llm
		if llm.APIKey != "" {
			secrets = append(secrets, llm.APIKey)
		}
	}

	if err := e.Exec.Prepare(ctx, env); err != nil {
		return fail(err)
	}
	if err := e.Exec.RunAgent(ctx, env, agentSpec); err != nil {
		return fail(err)
	}
	reward, err := e.Exec.RunVerifier(ctx, env)
	if err != nil {
		return fail(err)
	}
	if err := e.Exec.Collect(ctx, env); err != nil {
		return fail(err)
	}

	writeResultJSON(env.OutDir(), trialID, t, reward)

	if err := e.perTrial(ctx, t, trialID, attempt, env, secrets, admission); err != nil {
		return fail(err)
	}

	if !admission {
		e.Store.CompleteTrial(ctx, trialID, reward, map[string]any{"duration_s": time.Since(start).Seconds()})
		e.Store.Event(ctx, e.ExpID, trialID, "TRIAL_COMPLETED", map[string]any{"reward": reward})
	}
	e.Exec.Cleanup(ctx, env)
	e.logf("trial %s: COMPLETED reward=%.3f (%s)", trialID, reward, time.Since(start).Round(time.Millisecond))
	return trialResult{TrialID: trialID, Reward: reward}
}

// ------------------------------------------------------------ per_trial ---

var artifactClass = map[string]sanitize.Class{
	"traj.jsonl":   sanitize.Distributable,
	"result.json":  sanitize.Distributable,
	"agent.log":    sanitize.Debug,
	"verifier.log": sanitize.Debug,
	"stdout.log":   sanitize.Debug,
	"stderr.log":   sanitize.Debug,
}

func artifactKind(name string) string {
	switch {
	case strings.HasPrefix(name, "bundle."):
		return "bundle"
	case name == "traj.jsonl":
		return "traj"
	case name == "result.json":
		return "result"
	default:
		return "log"
	}
}

func (e *Engine) perTrial(ctx context.Context, t *Task, trialID string, attempt int, env *rexec.CaseEnv, secrets []string, admission bool) error {
	ex := &e.File.Experiment
	out := env.OutDir()

	// redact -- mandatory for keys, tiered for IPs
	if st := ex.Pipeline.PerTrialStep("redact"); st != nil {
		ips := map[sanitize.Class]bool{
			sanitize.Distributable: st.IPs["traj"],
			sanitize.Debug:         st.IPs["logs"],
		}
		s := sanitize.New(secrets, st.ExtraPatterns, ips)
		names, _ := filepath.Glob(filepath.Join(out, "*"))
		var total sanitize.Hits
		for _, p := range names {
			base := filepath.Base(p)
			class, ok := artifactClass[base]
			if !ok {
				class = sanitize.Debug
			}
			h, err := s.ScrubFile(p, class)
			if err != nil {
				// scrubbing must block the upload -- shipping an unscrubbed
				// trajectory cannot be undone
				return failure.Wrap(failure.PostprocessFailed, "redact "+base, err)
			}
			total.Exact += h.Exact
			total.Pattern += h.Pattern
			total.IP += h.IP
		}
		if !admission {
			e.Store.Event(ctx, e.ExpID, trialID, "REDACTED", total)
		}
		if total.Exact > 0 {
			e.logf("trial %s: redact hit %d exact secret occurrences in artifacts", trialID, total.Exact)
		}
	}

	// bundle -- failure degrades to shipping loose files
	bundleName := ""
	if st := ex.Pipeline.PerTrialStep("bundle"); st != nil {
		format := st.Format
		if format == "" {
			format = "tar.zst"
		}
		var names []string
		for _, inc := range st.Include {
			switch inc {
			case "traj":
				names = append(names, "traj.jsonl")
			case "result":
				names = append(names, "result.json")
			case "logs":
				names = append(names, "agent.log", "verifier.log", "stdout.log", "stderr.log")
			}
		}
		if len(names) == 0 {
			names = []string{"traj.jsonl", "result.json", "agent.log", "verifier.log"}
		}
		dst := filepath.Join(out, "bundle."+format)
		got, err := bundle.Create(out, names, dst, format)
		if err != nil {
			e.logf("trial %s: bundle failed (%v), degrading to loose files", trialID, err)
			e.Store.Event(ctx, e.ExpID, trialID, "BUNDLE_DEGRADED", map[string]any{"error": err.Error()})
		} else {
			bundleName = filepath.Base(got)
		}
	}

	objects := []string{"result.json"}
	if bundleName != "" {
		objects = append(objects, bundleName)
	}

	if st := ex.Pipeline.PerTrialStep("upload"); st != nil {
		return e.uploadObjects(ctx, st, t, trialID, attempt, out, pickObjects(st.Objects, objects, bundleName), admission)
	}
	if st := ex.Pipeline.PerTrialStep("stage"); st != nil {
		return e.stage(ctx, t, trialID, attempt, out, objects, admission)
	}
	return nil
}

func pickObjects(want, have []string, bundleName string) []string {
	if len(want) == 0 {
		return have
	}
	var out []string
	for _, w := range want {
		switch w {
		case "bundle":
			if bundleName != "" {
				out = append(out, bundleName)
			}
		case "result":
			out = append(out, "result.json")
		default:
			out = append(out, w)
		}
	}
	return out
}

func (e *Engine) uploadObjects(ctx context.Context, st *spec.Step, t *Task, trialID string, attempt int, dir string, names []string, admission bool) error {
	using := st.Using
	if using == "" {
		using = "storage_upload"
	}
	dest := e.renderDest(st.Dest, t, trialID)
	for _, n := range names {
		p := filepath.Join(dir, n)
		fi, err := os.Stat(p)
		if err != nil {
			continue
		}
		key := dest + n
		if _, err := e.Runner.Run(ctx, using, map[string]string{"Key": key, "LocalPath": p}); err != nil {
			return failure.Wrap(failure.ObjectStoreError, "upload "+n, err)
		}
		sha, _ := cas.FileSHA256(p)
		if !admission {
			e.Store.AddArtifact(ctx, trialID, attempt, artifactKind(n), n, key, fi.Size(), sha, nil)
		}
	}
	return nil
}

// stage holds artifacts on this runner until per_experiment ships them.
func (e *Engine) stage(ctx context.Context, t *Task, trialID string, attempt int, dir string, names []string, admission bool) error {
	sd := filepath.Join(e.stagingRoot(), trialID)
	if err := os.MkdirAll(sd, 0o755); err != nil {
		return failure.Wrap(failure.HostError, "staging dir", err)
	}
	for _, n := range names {
		src := filepath.Join(dir, n)
		fi, err := os.Stat(src)
		if err != nil {
			continue
		}
		b, err := os.ReadFile(src)
		if err != nil {
			return failure.Wrap(failure.HostError, "stage "+n, err)
		}
		if err := os.WriteFile(filepath.Join(sd, n), b, 0o644); err != nil {
			return failure.Wrap(failure.HostError, "stage "+n, err)
		}
		sha, _ := cas.FileSHA256(src)
		if !admission {
			e.Store.AddArtifact(ctx, trialID, attempt, artifactKind(n), n, "staged:"+trialID+"/"+n, fi.Size(), sha, nil)
		}
	}
	return nil
}

func (e *Engine) stagingRoot() string { return filepath.Join(e.WorkRoot, "staging", e.ExpID) }

// ------------------------------------------------------- per_experiment ---

func (e *Engine) perExperiment(ctx context.Context) error {
	ex := &e.File.Experiment
	for _, st := range ex.Pipeline.PerExperiment {
		switch st.Step {
		case "upload":
			if err := e.flushStaging(ctx, &st); err != nil {
				return err
			}
		case "report":
			if err := e.report(ctx, &st); err != nil {
				return err
			}
		case "deploy":
			if err := e.deploy(ctx, &st); err != nil {
				return err
			}
		default:
			e.logf("per_experiment: skipping unknown step %q", st.Step)
		}
	}
	return nil
}

// flushStaging packs everything this runner staged into one archive and ships
// it with a single command invocation.
func (e *Engine) flushStaging(ctx context.Context, st *spec.Step) error {
	root := e.stagingRoot()
	if _, err := os.Stat(root); err != nil {
		e.logf("per_experiment upload: nothing staged (per_trial uploaded directly)")
		return nil
	}
	tmp := filepath.Join(e.WorkRoot, "tmp")
	os.MkdirAll(tmp, 0o755)
	arc := filepath.Join(tmp, e.ExpID+"-"+e.RunnerID+".tar.gz")

	var names []string
	filepath.Walk(root, func(p string, fi os.FileInfo, err error) error {
		if err != nil || fi.IsDir() {
			return err
		}
		rel, _ := filepath.Rel(root, p)
		names = append(names, rel)
		return nil
	})
	sort.Strings(names)
	if len(names) == 0 {
		return nil
	}
	if _, err := bundle.Create(root, names, arc, "tar.gz"); err != nil {
		return failure.Wrap(failure.PostprocessFailed, "pack staged artifacts", err)
	}
	using := st.Using
	if using == "" {
		using = "storage_upload"
	}
	key := e.renderDest(st.Dest, nil, "") + filepath.Base(arc)
	if _, err := e.Runner.Run(ctx, using, map[string]string{"Key": key, "LocalPath": arc}); err != nil {
		return failure.Wrap(failure.ObjectStoreError, "flush staged artifacts", err)
	}
	e.logf("per_experiment upload: %d staged files from %s shipped in 1 call -> %s",
		len(names), e.RunnerID, key)
	e.Store.Event(ctx, e.ExpID, "", "STAGING_FLUSHED",
		map[string]any{"runner": e.RunnerID, "files": len(names), "key": key})
	return nil
}

func (e *Engine) report(ctx context.Context, st *spec.Step) error {
	rows, err := e.Store.Results(ctx, e.ExpID)
	if err != nil {
		return err
	}
	dir := filepath.Join(e.WorkRoot, "reports", e.ExpID)
	os.MkdirAll(dir, 0o755)
	jsonPath := filepath.Join(dir, "results.json")
	b, _ := json.MarshalIndent(rows, "", "  ")
	if err := os.WriteFile(jsonPath, b, 0o644); err != nil {
		return err
	}
	csvPath := filepath.Join(dir, "results.csv")
	var sb strings.Builder
	sb.WriteString("agent,llm_spec,completed,reward_mean,reward_median,failures\n")
	for _, r := range rows {
		mean, med := stats(r.Rewards)
		sb.WriteString(fmt.Sprintf("%s,%s,%d,%.4f,%.4f,%d\n",
			r.Agent, r.LLMSpec, r.Completed, mean, med, sum(r.FailCounts)))
	}
	os.WriteFile(csvPath, []byte(sb.String()), 0o644)

	if e.Runner.Has("storage_upload") && st.Dest != "" {
		dest := e.renderDest(st.Dest, nil, "")
		for _, p := range []string{jsonPath, csvPath} {
			if _, err := e.Runner.Run(ctx, "storage_upload",
				map[string]string{"Key": dest + filepath.Base(p), "LocalPath": p}); err != nil {
				return failure.Wrap(failure.ObjectStoreError, "upload report", err)
			}
		}
	}
	e.logf("per_experiment report: %s", dir)
	return nil
}

func (e *Engine) deploy(ctx context.Context, st *spec.Step) error {
	if len(st.Run) == 0 {
		return nil
	}
	argv := make([]string, 0, len(st.Run))
	for _, a := range st.Run {
		a = strings.ReplaceAll(a, "{{.ExperimentID}}", e.ExpID)
		a = strings.ReplaceAll(a, "{{.ExperimentName}}", e.File.Experiment.Name)
		a = strings.ReplaceAll(a, "{{.ReportPath}}", filepath.Join(e.WorkRoot, "reports", e.ExpID))
		a = strings.ReplaceAll(a, "{{.ReportURL}}", "")
		argv = append(argv, a)
	}
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		if st.OnFailure == "fail" {
			return failure.Wrap(failure.InternalError, "deploy", err)
		}
		e.logf("per_experiment deploy: failed (on_failure=warn): %v", err)
		e.Store.Event(ctx, e.ExpID, "", "DEPLOY_FAILED", map[string]any{"error": err.Error()})
		return nil
	}
	e.logf("per_experiment deploy: ok%s", firstLine(string(out)))
	return nil
}

// ---------------------------------------------------------------- utils ---

func (e *Engine) resolveLLM(ctx context.Context, name string) (*rexec.LLMEnv, error) {
	s, ok := e.File.LLMSpecs[name]
	if !ok {
		return nil, failure.New(failure.InternalError, "unknown llm_spec "+name)
	}
	env := &rexec.LLMEnv{BaseURL: s.BaseURL, Model: s.Model}
	switch {
	case s.APIKeyEnv != "":
		env.APIKey = os.Getenv(s.APIKeyEnv)
		if env.APIKey == "" {
			return nil, failure.New(failure.InternalError,
				"llm_spec "+name+": environment variable "+s.APIKeyEnv+" is not set on this runner")
		}
	case len(s.APIKeyCmd) > 0:
		out, err := exec.CommandContext(ctx, s.APIKeyCmd[0], s.APIKeyCmd[1:]...).Output()
		if err != nil {
			return nil, failure.Wrap(failure.InternalError, "llm_spec "+name+": api_key_cmd", err)
		}
		env.APIKey = strings.TrimSpace(string(out))
	}
	return env, nil
}

func (e *Engine) renderDest(dest string, t *Task, trialID string) string {
	if dest == "" {
		dest = "evals/" + e.File.Experiment.Name + "/"
	}
	rep := strings.NewReplacer(
		"{{.ExperimentName}}", e.File.Experiment.Name,
		"{{.ExperimentID}}", e.ExpID,
		"{{.TrialID}}", trialID,
		"{{.CasePath}}", casePath(t),
	)
	d := rep.Replace(dest)
	if !strings.HasSuffix(d, "/") {
		d += "/"
	}
	return d
}

func casePath(t *Task) string {
	if t == nil || t.Case == nil {
		return ""
	}
	return t.Case.Label
}

func writeResultJSON(dir, trialID string, t *Task, reward float64) {
	b, _ := json.MarshalIndent(map[string]any{
		"trial_id":    trialID,
		"case":        t.Case.Label,
		"case_sha256": t.Case.SHA256,
		"agent":       t.Agent.Name,
		"llm_spec":    t.LLMSpec,
		"reward":      reward,
	}, "", "  ")
	os.WriteFile(filepath.Join(dir, "result.json"), b, 0o644)
}

// taskID must be scoped to the run: a stable id across runs collides on
// insert and silently attaches the new run's trials to the old experiment.
func taskID(expID string, cv *resolve.CaseVersion, agent, llmSpec string) string {
	id := expID + "-" + slug(cv.Label) + "-" + slug(agent)
	if llmSpec != "" {
		id += "-" + slug(llmSpec)
	}
	return id
}

func slug(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	return strings.Trim(b.String(), "-")
}

func short(s string) string {
	if len(s) > 12 {
		return s[:12]
	}
	return s
}

func passWord(b bool) string {
	if b {
		return "PASS"
	}
	return "FAIL"
}

func sum(m map[string]int) int {
	n := 0
	for _, v := range m {
		n += v
	}
	return n
}

func stats(v []float64) (mean, median float64) {
	if len(v) == 0 {
		return 0, 0
	}
	s := append([]float64(nil), v...)
	sort.Float64s(s)
	for _, x := range s {
		mean += x
	}
	mean /= float64(len(s))
	if len(s)%2 == 1 {
		median = s[len(s)/2]
	} else {
		median = (s[len(s)/2-1] + s[len(s)/2]) / 2
	}
	if math.IsNaN(mean) {
		mean = 0
	}
	return
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	return ": " + s
}
