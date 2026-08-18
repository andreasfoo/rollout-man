// Package activities holds everything a workflow is not allowed to do:
// filesystem, containers, database, external commands.
package activities

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/temporal"

	"github.com/andreasfoo/rollout-man/internal/bundle"
	"github.com/andreasfoo/rollout-man/internal/cas"
	"github.com/andreasfoo/rollout-man/internal/casedef"
	"github.com/andreasfoo/rollout-man/internal/cmdrun"
	rexec "github.com/andreasfoo/rollout-man/internal/exec"
	"github.com/andreasfoo/rollout-man/internal/failure"
	"github.com/andreasfoo/rollout-man/internal/resolve"
	"github.com/andreasfoo/rollout-man/internal/sanitize"
	"github.com/andreasfoo/rollout-man/internal/spec"
	"github.com/andreasfoo/rollout-man/internal/store"
)

type Deps struct {
	Runner   *cmdrun.Runner
	CAS      *cas.Store
	Resolver *resolve.Resolver
	Exec     rexec.Executor
	Store    *store.DB
	WorkRoot string
	RunnerID string
}

// wrap turns a taxonomy error into an ApplicationError whose Type is the code,
// which is what the workflow's retry policy and error mapping key on.
func wrap(err error) error {
	if err == nil {
		return nil
	}
	code := failure.FromError(err)
	return temporal.NewApplicationErrorWithCause(err.Error(), string(code), err)
}

// ---------------------------------------------------------------- case ----

type ResolveCaseInput struct {
	Ref spec.CaseRef
}

type ResolveCaseOutput struct {
	SHA256   string
	Label    string
	Dir      string
	PinnedAt string
	CacheHit bool
	Config   *casedef.TaskConfig
}

func (d *Deps) ResolveCase(ctx context.Context, in ResolveCaseInput) (*ResolveCaseOutput, error) {
	cv, err := d.Resolver.Resolve(ctx, in.Ref)
	size := int64(0)
	if cv.SHA256 != "" {
		if fi, e := os.Stat(d.CAS.Path(cv.SHA256)); e == nil {
			size = fi.Size()
		}
	}
	d.Store.UpsertCaseVersion(ctx, cv.SHA256, cv.Label, cv.Source, cv.PinnedAt,
		string(cv.State), cv.Error, cv.Cfg, size)
	if err != nil {
		return nil, wrap(err)
	}
	return &ResolveCaseOutput{
		SHA256: cv.SHA256, Label: cv.Label, Dir: cv.Dir,
		PinnedAt: cv.PinnedAt, CacheHit: cv.CacheHit, Config: cv.Cfg,
	}, nil
}

type RecordAdmissionInput struct {
	SHA256  string
	Verdict string
	Result  map[string]any
}

func (d *Deps) RecordAdmission(ctx context.Context, in RecordAdmissionInput) error {
	return d.Store.SetAdmission(ctx, in.SHA256, in.Verdict, in.Result)
}

// --------------------------------------------------------------- trial ----

type TrialRef struct {
	ExpID      string
	TaskID     string
	TrialID    string
	TrialIndex int
	Attempt    int
	CaseDir    string
	CaseLabel  string
	CaseSHA    string
	AgentName  string
	AgentKind  string
	LLMSpec    *spec.LLMSpec
	Admission  bool
}

func (r TrialRef) work(root string) string {
	return filepath.Join(root, "trials", r.TrialID, fmt.Sprintf("attempt-%d", r.Attempt))
}

func (d *Deps) env(r TrialRef) (*rexec.CaseEnv, error) {
	cfg, err := casedef.Load(r.CaseDir)
	if err != nil {
		return nil, failure.Wrap(failure.HostError, "reload task.toml", err)
	}
	w := r.work(d.WorkRoot)
	if err := os.MkdirAll(w, 0o755); err != nil {
		return nil, failure.Wrap(failure.HostError, "work dir", err)
	}
	return &rexec.CaseEnv{CaseDir: r.CaseDir, WorkDir: w, Cfg: cfg}, nil
}

func (d *Deps) PrepareCase(ctx context.Context, r TrialRef) error {
	env, err := d.env(r)
	if err != nil {
		return wrap(err)
	}
	return wrap(d.Exec.Prepare(ctx, env))
}

func (d *Deps) RunAgent(ctx context.Context, r TrialRef) error {
	env, err := d.env(r)
	if err != nil {
		return wrap(err)
	}
	as := rexec.AgentSpec{Kind: rexec.AgentKind(r.AgentKind), Name: r.AgentName}
	if as.Kind == rexec.LLM {
		if c, ok := d.Runner.Cmds["agent_"+r.AgentName]; ok {
			if len(c.Run) > 0 {
				as.Command = c.Run
			} else if c.Script != "" {
				as.Command = rexec.ScriptCommand(c.Script)
			}
		}
		if r.LLMSpec != nil {
			llm, err := d.resolveLLM(ctx, *r.LLMSpec)
			if err != nil {
				return wrap(err)
			}
			as.LLM = llm
		}
	}
	// Heartbeat is the liveness proof, the cancellation channel and the
	// resume point all at once; without it a dead runner is indistinguishable
	// from a slow agent.
	stop := heartbeat(ctx, 10*time.Second)
	defer stop()
	return wrap(d.Exec.RunAgent(ctx, env, as))
}

func (d *Deps) RunVerifier(ctx context.Context, r TrialRef) (float64, error) {
	env, err := d.env(r)
	if err != nil {
		return 0, wrap(err)
	}
	stop := heartbeat(ctx, 10*time.Second)
	defer stop()
	reward, err := d.Exec.RunVerifier(ctx, env)
	return reward, wrap(err)
}

func (d *Deps) CollectArtifacts(ctx context.Context, r TrialRef) error {
	env, err := d.env(r)
	if err != nil {
		return wrap(err)
	}
	if err := d.Exec.Collect(ctx, env); err != nil {
		return wrap(err)
	}
	return nil
}

type PostProcessInput struct {
	Ref     TrialRef
	Steps   []spec.Step
	Reward  float64
	ExpName string
}

type PostProcessOutput struct {
	Staged   []string
	Uploaded []string
	Hits     sanitize.Hits
}

func (d *Deps) PostProcess(ctx context.Context, in PostProcessInput) (*PostProcessOutput, error) {
	r := in.Ref
	env, err := d.env(r)
	if err != nil {
		return nil, wrap(err)
	}
	out := env.OutDir()
	writeResultJSON(out, r, in.Reward)

	var secrets []string
	if r.LLMSpec != nil {
		if llm, err := d.resolveLLM(ctx, *r.LLMSpec); err == nil && llm.APIKey != "" {
			secrets = append(secrets, llm.APIKey)
		}
	}

	res := &PostProcessOutput{}
	for _, st := range in.Steps {
		switch st.Step {
		case "redact":
			ips := map[sanitize.Class]bool{
				sanitize.Distributable: st.IPs["traj"],
				sanitize.Debug:         st.IPs["logs"],
			}
			s := sanitize.New(secrets, st.ExtraPatterns, ips)
			files, _ := filepath.Glob(filepath.Join(out, "*"))
			for _, p := range files {
				class := sanitize.Debug
				if c, ok := distributable[filepath.Base(p)]; ok {
					class = c
				}
				h, err := s.ScrubFile(p, class)
				if err != nil {
					// never ship what could not be scrubbed
					return nil, wrap(failure.Wrap(failure.PostprocessFailed, "redact "+filepath.Base(p), err))
				}
				res.Hits.Exact += h.Exact
				res.Hits.Pattern += h.Pattern
				res.Hits.IP += h.IP
			}
		case "bundle":
			format := st.Format
			if format == "" {
				format = "tar.zst"
			}
			names := bundleNames(st.Include)
			got, err := bundle.Create(out, names, filepath.Join(out, "bundle."+format), format)
			if err != nil {
				// degrade: loose files still ship
				d.Store.Event(ctx, r.ExpID, r.TrialID, "BUNDLE_DEGRADED", map[string]any{"error": err.Error()})
				continue
			}
			res.Staged = append(res.Staged, filepath.Base(got))
		case "stage":
			names := append([]string{"result.json"}, res.Staged...)
			if err := d.stage(ctx, r, out, names); err != nil {
				return nil, wrap(err)
			}
			res.Staged = names
		case "upload":
			names := pickObjects(st.Objects, append([]string{"result.json"}, res.Staged...))
			up, err := d.upload(ctx, st, r, in.ExpName, out, names)
			if err != nil {
				return nil, wrap(err)
			}
			res.Uploaded = up
		}
	}
	return res, nil
}

func (d *Deps) CleanupTrial(ctx context.Context, r TrialRef) error {
	env, err := d.env(r)
	if err != nil {
		return nil // nothing to clean
	}
	_ = d.Exec.Cleanup(ctx, env)
	return nil
}

// -------------------------------------------------------- read model ------

type TrialStateInput struct {
	Ref      TrialRef
	State    string
	Reward   float64
	Code     string
	Category string
	Message  string
	Duration float64
}

func (d *Deps) RecordTrialState(ctx context.Context, in TrialStateInput) error {
	r := in.Ref
	if r.Admission {
		return nil
	}
	switch in.State {
	case "RUNNING":
		return d.Store.StartTrial(ctx, r.TrialID, r.TaskID, r.TrialIndex, r.Attempt, d.RunnerID)
	case "COMPLETED":
		d.Store.Event(ctx, r.ExpID, r.TrialID, "TRIAL_COMPLETED", map[string]any{"reward": in.Reward})
		return d.Store.CompleteTrial(ctx, r.TrialID, in.Reward, map[string]any{"duration_s": in.Duration})
	default:
		d.Store.Event(ctx, r.ExpID, r.TrialID, "TRIAL_FAILED", map[string]any{"code": in.Code})
		return d.Store.FailTrial(ctx, r.TrialID, in.Category, in.Code, in.Message,
			map[string]any{"duration_s": in.Duration})
	}
}

type TaskInput struct {
	ExpID, TaskID, CaseSHA, CaseLabel, Agent, LLMSpec string
	Trials                                            int
	State                                             string
}

func (d *Deps) RecordTask(ctx context.Context, in TaskInput) error {
	if in.State == "" {
		return d.Store.CreateTask(ctx, in.TaskID, in.ExpID, in.CaseSHA, in.CaseLabel,
			in.Agent, in.LLMSpec, in.Trials)
	}
	return d.Store.SetTaskState(ctx, in.TaskID, in.State)
}

type ExperimentStateInput struct {
	ExpID, Name, State string
	Config             any
}

func (d *Deps) RecordExperiment(ctx context.Context, in ExperimentStateInput) error {
	if in.State == "" {
		return d.Store.CreateExperiment(ctx, in.ExpID, in.Name, in.Config)
	}
	return d.Store.FinishExperiment(ctx, in.ExpID, in.State)
}

func (d *Deps) EmitEvent(ctx context.Context, in map[string]any) error {
	return d.Store.Event(ctx, str(in["experiment_id"]), str(in["trial_id"]), str(in["type"]), in["payload"])
}

// --------------------------------------------------- per_experiment -------

type FlushInput struct {
	ExpID, ExpName string
	Step           spec.Step
}

func (d *Deps) FlushStaging(ctx context.Context, in FlushInput) (int, error) {
	root := filepath.Join(d.WorkRoot, "staging", in.ExpID)
	if _, err := os.Stat(root); err != nil {
		return 0, nil
	}
	var names []string
	filepath.Walk(root, func(p string, fi os.FileInfo, err error) error {
		if err != nil || fi.IsDir() {
			return err
		}
		rel, _ := filepath.Rel(root, p)
		names = append(names, rel)
		return nil
	})
	if len(names) == 0 {
		return 0, nil
	}
	tmp := filepath.Join(d.WorkRoot, "tmp")
	os.MkdirAll(tmp, 0o755)
	arc := filepath.Join(tmp, in.ExpID+"-"+d.RunnerID+".tar.gz")
	if _, err := bundle.Create(root, names, arc, "tar.gz"); err != nil {
		return 0, wrap(failure.Wrap(failure.PostprocessFailed, "pack staged artifacts", err))
	}
	using := in.Step.Using
	if using == "" {
		using = "storage_upload"
	}
	key := renderDest(in.Step.Dest, in.ExpName, in.ExpID, "", "") + filepath.Base(arc)
	if _, err := d.Runner.Run(ctx, using, map[string]string{"Key": key, "LocalPath": arc}); err != nil {
		return 0, wrap(failure.Wrap(failure.ObjectStoreError, "flush staged artifacts", err))
	}
	d.Store.Event(ctx, in.ExpID, "", "STAGING_FLUSHED",
		map[string]any{"runner": d.RunnerID, "files": len(names), "key": key})
	return len(names), nil
}

func (d *Deps) Report(ctx context.Context, in FlushInput) (string, error) {
	rows, err := d.Store.Results(ctx, in.ExpID)
	if err != nil {
		return "", wrap(err)
	}
	dir := filepath.Join(d.WorkRoot, "reports", in.ExpID)
	os.MkdirAll(dir, 0o755)
	b, _ := json.MarshalIndent(rows, "", "  ")
	p := filepath.Join(dir, "results.json")
	if err := os.WriteFile(p, b, 0o644); err != nil {
		return "", wrap(err)
	}
	if in.Step.Dest != "" && d.Runner.Has("storage_upload") {
		key := renderDest(in.Step.Dest, in.ExpName, in.ExpID, "", "") + "results.json"
		if _, err := d.Runner.Run(ctx, "storage_upload", map[string]string{"Key": key, "LocalPath": p}); err != nil {
			return "", wrap(failure.Wrap(failure.ObjectStoreError, "upload report", err))
		}
	}
	return dir, nil
}

func (d *Deps) Deploy(ctx context.Context, in FlushInput) error {
	if len(in.Step.Run) == 0 {
		return nil
	}
	argv := make([]string, len(in.Step.Run))
	for i, a := range in.Step.Run {
		argv[i] = renderVars(a, in.ExpName, in.ExpID, filepath.Join(d.WorkRoot, "reports", in.ExpID))
	}
	cmd := rexec.Command(ctx, argv)
	if out, err := cmd.CombinedOutput(); err != nil {
		if in.Step.OnFailure == "fail" {
			return wrap(failure.Wrap(failure.InternalError, "deploy: "+string(out), err))
		}
		d.Store.Event(ctx, in.ExpID, "", "DEPLOY_FAILED", map[string]any{"error": err.Error()})
	}
	return nil
}

// ---------------------------------------------------------------- util ----

func heartbeat(ctx context.Context, every time.Duration) func() {
	done := make(chan struct{})
	go func() {
		t := time.NewTicker(every)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-ctx.Done():
				return
			case <-t.C:
				activity.RecordHeartbeat(ctx, time.Now().Unix())
			}
		}
	}()
	return func() { close(done) }
}

var distributable = map[string]sanitize.Class{
	"traj.jsonl":  sanitize.Distributable,
	"result.json": sanitize.Distributable,
}

func bundleNames(include []string) []string {
	var names []string
	for _, inc := range include {
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
	return names
}

func pickObjects(want, have []string) []string {
	if len(want) == 0 {
		return have
	}
	var out []string
	for _, w := range want {
		switch w {
		case "bundle":
			for _, h := range have {
				if len(h) > 7 && h[:7] == "bundle." {
					out = append(out, h)
				}
			}
		case "result":
			out = append(out, "result.json")
		default:
			out = append(out, w)
		}
	}
	return out
}

func (d *Deps) stage(ctx context.Context, r TrialRef, dir string, names []string) error {
	sd := filepath.Join(d.WorkRoot, "staging", r.ExpID, r.TrialID)
	if err := os.MkdirAll(sd, 0o755); err != nil {
		return failure.Wrap(failure.HostError, "staging dir", err)
	}
	for _, n := range names {
		b, err := os.ReadFile(filepath.Join(dir, n))
		if err != nil {
			continue
		}
		if err := os.WriteFile(filepath.Join(sd, n), b, 0o644); err != nil {
			return failure.Wrap(failure.HostError, "stage "+n, err)
		}
		if !r.Admission {
			sha, _ := cas.FileSHA256(filepath.Join(dir, n))
			d.Store.AddArtifact(ctx, r.TrialID, r.Attempt, kindOf(n), n,
				"staged:"+r.TrialID+"/"+n, int64(len(b)), sha, nil)
		}
	}
	return nil
}

func (d *Deps) upload(ctx context.Context, st spec.Step, r TrialRef, expName, dir string, names []string) ([]string, error) {
	using := st.Using
	if using == "" {
		using = "storage_upload"
	}
	dest := renderDest(st.Dest, expName, r.ExpID, r.CaseLabel, r.TrialID)
	var done []string
	for _, n := range names {
		p := filepath.Join(dir, n)
		fi, err := os.Stat(p)
		if err != nil {
			continue
		}
		key := dest + n
		if _, err := d.Runner.Run(ctx, using, map[string]string{"Key": key, "LocalPath": p}); err != nil {
			return nil, failure.Wrap(failure.ObjectStoreError, "upload "+n, err)
		}
		if !r.Admission {
			sha, _ := cas.FileSHA256(p)
			d.Store.AddArtifact(ctx, r.TrialID, r.Attempt, kindOf(n), n, key, fi.Size(), sha, nil)
		}
		done = append(done, n)
	}
	return done, nil
}

func kindOf(name string) string {
	switch {
	case len(name) > 7 && name[:7] == "bundle.":
		return "bundle"
	case name == "traj.jsonl":
		return "traj"
	case name == "result.json":
		return "result"
	default:
		return "log"
	}
}

func (d *Deps) resolveLLM(ctx context.Context, s spec.LLMSpec) (*rexec.LLMEnv, error) {
	env := &rexec.LLMEnv{BaseURL: s.BaseURL, Model: s.Model}
	switch {
	case s.APIKeyEnv != "":
		env.APIKey = os.Getenv(s.APIKeyEnv)
		if env.APIKey == "" {
			return nil, failure.New(failure.InternalError,
				"llm_spec "+s.Name+": "+s.APIKeyEnv+" is not set on this runner")
		}
	case len(s.APIKeyCmd) > 0:
		out, err := rexec.Command(ctx, s.APIKeyCmd).Output()
		if err != nil {
			return nil, failure.Wrap(failure.InternalError, "llm_spec "+s.Name+": api_key_cmd", err)
		}
		env.APIKey = trimSpace(string(out))
	}
	return env, nil
}

func writeResultJSON(dir string, r TrialRef, reward float64) {
	b, _ := json.MarshalIndent(map[string]any{
		"trial_id": r.TrialID, "case": r.CaseLabel, "case_sha256": r.CaseSHA,
		"agent": r.AgentName, "reward": reward,
	}, "", "  ")
	os.WriteFile(filepath.Join(dir, "result.json"), b, 0o644)
}

func renderDest(dest, expName, expID, casePath, trialID string) string {
	if dest == "" {
		dest = "evals/" + expName + "/"
	}
	d := renderVars(dest, expName, expID, "")
	d = replaceAll(d, "{{.CasePath}}", casePath)
	d = replaceAll(d, "{{.TrialID}}", trialID)
	if len(d) == 0 || d[len(d)-1] != '/' {
		d += "/"
	}
	return d
}

func renderVars(s, expName, expID, reportPath string) string {
	s = replaceAll(s, "{{.ExperimentName}}", expName)
	s = replaceAll(s, "{{.ExperimentID}}", expID)
	s = replaceAll(s, "{{.ReportPath}}", reportPath)
	s = replaceAll(s, "{{.ReportURL}}", "")
	return s
}

func replaceAll(s, old, new string) string { return strings.ReplaceAll(s, old, new) }

func trimSpace(s string) string { return strings.TrimSpace(s) }

func str(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}
