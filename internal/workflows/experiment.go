package workflows

import (
	"fmt"
	"strings"
	"time"

	"go.temporal.io/api/enums/v1"
	"go.temporal.io/sdk/workflow"

	"github.com/andreasfoo/rollout-man/internal/activities"
	"github.com/andreasfoo/rollout-man/internal/spec"
	"github.com/andreasfoo/rollout-man/internal/temporalx"
)

type ExperimentInput struct {
	ExpID       string
	Experiment  spec.Experiment
	LLMSpecs    map[string]spec.LLMSpec
	RunnerQueue string
	RunnerID    string
}

type caseState struct {
	SHA      string
	Label    string
	Dir      string
	Admitted bool
	Timeouts Timeouts
}

func ExperimentWorkflow(ctx workflow.Context, in ExperimentInput) error {
	ex := in.Experiment

	if err := workflow.ExecuteActivity(temporalx.Server(ctx), (*activities.Deps).RecordExperiment,
		activities.ExperimentStateInput{ExpID: in.ExpID, Name: ex.Name, Config: ex}).Get(ctx, nil); err != nil {
		return err
	}

	cases, err := perCase(ctx, in)
	if err != nil {
		finish(ctx, in.ExpID, "FAILED")
		return err
	}

	tasks := expand(in, cases)
	total := 0
	for _, t := range tasks {
		total += t.Trials
	}
	if total > 500 {
		finish(ctx, in.ExpID, "FAILED")
		return fmt.Errorf("MATRIX_TOO_LARGE: %d trials exceeds the MVP cap of 500", total)
	}
	workflow.GetLogger(ctx).Info("matrix expanded", "tasks", len(tasks), "trials", total)

	for _, t := range tasks {
		if err := workflow.ExecuteActivity(temporalx.Server(ctx), (*activities.Deps).RecordTask,
			activities.TaskInput{ExpID: in.ExpID, TaskID: t.ID, CaseSHA: t.Case.SHA,
				CaseLabel: t.Case.Label, Agent: t.Agent, LLMSpec: t.LLMSpec, Trials: t.Trials}).Get(ctx, nil); err != nil {
			return err
		}
	}

	runTrials(ctx, in, tasks)

	for _, t := range tasks {
		_ = workflow.ExecuteActivity(temporalx.Server(ctx), (*activities.Deps).RecordTask,
			activities.TaskInput{TaskID: t.ID, State: "COMPLETED"}).Get(ctx, nil)
	}

	if err := perExperiment(ctx, in); err != nil {
		finish(ctx, in.ExpID, "ARTIFACTS_INCOMPLETE")
		return err
	}
	finish(ctx, in.ExpID, "COMPLETED")
	return nil
}

func finish(ctx workflow.Context, id, state string) {
	dctx, cancel := workflow.NewDisconnectedContext(ctx)
	defer cancel()
	_ = workflow.ExecuteActivity(temporalx.Server(dctx), (*activities.Deps).RecordExperiment,
		activities.ExperimentStateInput{ExpID: id, State: state}).Get(dctx, nil)
}

// ------------------------------------------------------------- per_case ---

func perCase(ctx workflow.Context, in ExperimentInput) ([]caseState, error) {
	ex := in.Experiment
	var out []caseState
	for _, raw := range ex.Cases {
		ref := raw.Merge(ex.CaseDefaults)

		var rc activities.ResolveCaseOutput
		rctx := temporalx.Runner(ctx, in.RunnerQueue, 60*time.Minute, time.Minute, 2)
		if err := workflow.ExecuteActivity(rctx, (*activities.Deps).ResolveCase,
			activities.ResolveCaseInput{Ref: ref}).Get(ctx, &rc); err != nil {
			return nil, fmt.Errorf("resolve %s: %w", ref.Label(), err)
		}
		cs := caseState{SHA: rc.SHA256, Label: rc.Label, Dir: rc.Dir, Timeouts: Timeouts{
			Agent: rc.Config.AgentTimeout, Verifier: rc.Config.VerifierTimeout, Build: rc.Config.BuildTimeout,
		}}
		workflow.GetLogger(ctx).Info("case resolved",
			"case", cs.Label, "sha", short(cs.SHA), "pinned", rc.PinnedAt, "cache_hit", rc.CacheHit)

		st := ex.Pipeline.PerCaseStep("admission")
		if st == nil {
			cs.Admitted = true
			out = append(out, cs)
			continue
		}
		admitted, err := admit(ctx, in, cs, st)
		if err != nil {
			return nil, err
		}
		cs.Admitted = admitted
		out = append(out, cs)
	}
	return out, nil
}

func admit(ctx workflow.Context, in ExperimentInput, cs caseState, st *spec.Step) (bool, error) {
	require := st.Require
	if require == "" {
		require = "admitted"
	}
	if require == "any" && !st.AutoAdmit {
		workflow.GetLogger(ctx).Warn("admission skipped; results will be marked UNADMITTED", "case", cs.Label)
		return false, nil
	}
	crit := st.Criteria
	if crit == nil {
		crit = &spec.Criteria{Trials: 2}
		crit.Oracle.MinReward = 1.0
	}
	if crit.Trials <= 0 {
		crit.Trials = 2
	}

	type probe struct {
		kind string
		ok   func(float64) bool
		want string
	}
	probes := []probe{
		{"oracle", func(r float64) bool { return r >= crit.Oracle.MinReward-1e-9 },
			fmt.Sprintf(">= %.2f", crit.Oracle.MinReward)},
		{"nop", func(r float64) bool { return r <= crit.Nop.MaxReward+1e-9 },
			fmt.Sprintf("<= %.2f", crit.Nop.MaxReward)},
	}

	result := map[string]any{}
	verdict := true
	for _, p := range probes {
		var rewards []float64
		for i := 1; i <= crit.Trials; i++ {
			id := fmt.Sprintf("%s-admit-%s-%s-%d", in.ExpID, short(cs.SHA), p.kind, i)
			res, err := runChild(ctx, in, TrialInput{
				Ref: activities.TrialRef{
					ExpID: in.ExpID, TaskID: id, TrialID: id, TrialIndex: i,
					CaseDir: cs.Dir, CaseLabel: cs.Label, CaseSHA: cs.SHA,
					AgentName: p.kind, AgentKind: p.kind, Admission: true,
				},
				ExpName: in.Experiment.Name, PerTrial: nil,
				MaxTotalAttempts: 1, RunnerQueue: in.RunnerQueue, Timeouts: cs.Timeouts,
			}, id)
			if err != nil {
				// a probe that could not run is not a verdict
				return false, fmt.Errorf("admission %s for %s could not run: %w", p.kind, cs.Label, err)
			}
			rewards = append(rewards, res.Reward)
		}
		pass := true
		for _, r := range rewards {
			if !p.ok(r) {
				pass = false
			}
		}
		result[p.kind] = map[string]any{"rewards": rewards, "want": p.want, "pass": pass}
		workflow.GetLogger(ctx).Info("admission probe",
			"case", cs.Label, "probe", p.kind, "rewards", rewards, "want", p.want, "pass", pass)
		if !pass {
			verdict = false
		}
	}

	state := "ADMITTED"
	if !verdict {
		state = "REJECTED"
	}
	_ = workflow.ExecuteActivity(temporalx.Server(ctx), (*activities.Deps).RecordAdmission,
		activities.RecordAdmissionInput{SHA256: cs.SHA, Verdict: state, Result: result}).Get(ctx, nil)

	if !verdict && require == "admitted" {
		return false, fmt.Errorf("CASE_NOT_ADMITTED: %s failed the admission gate", cs.Label)
	}
	return verdict, nil
}

// --------------------------------------------------------------- matrix ---

type task struct {
	ID      string
	Case    caseState
	Agent   string
	Kind    string
	LLMSpec string
	Trials  int
}

func expand(in ExperimentInput, cases []caseState) []task {
	ex := in.Experiment
	var out []task
	for _, cs := range cases {
		for _, a := range ex.Matrix.Agents {
			kind := "llm"
			if a.Name == "oracle" || a.Name == "nop" {
				kind = a.Name
			}
			if kind != "llm" {
				out = append(out, task{ID: taskID(in.ExpID, cs.Label, a.Name, ""),
					Case: cs, Agent: a.Name, Kind: kind, Trials: ex.Matrix.Trials})
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
				out = append(out, task{ID: taskID(in.ExpID, cs.Label, a.Name, s),
					Case: cs, Agent: a.Name, Kind: kind, LLMSpec: s, Trials: ex.Matrix.Trials})
			}
		}
	}
	return out
}

// --------------------------------------------------------------- trials ---

func runTrials(ctx workflow.Context, in ExperimentInput, tasks []task) {
	limit := in.Experiment.Concurrency
	if limit < 1 {
		limit = 1
	}
	inFlight := 0
	for _, t := range tasks {
		for i := 1; i <= t.Trials; i++ {
			// concurrency counts trials in flight, queued or executing
			if err := workflow.Await(ctx, func() bool { return inFlight < limit }); err != nil {
				return
			}
			inFlight++
			t, i := t, i
			workflow.Go(ctx, func(gctx workflow.Context) {
				defer func() { inFlight-- }()
				id := fmt.Sprintf("%s-%d", t.ID, i)
				ti := TrialInput{
					Ref: activities.TrialRef{
						ExpID: in.ExpID, TaskID: t.ID, TrialID: id, TrialIndex: i,
						CaseDir: t.Case.Dir, CaseLabel: t.Case.Label, CaseSHA: t.Case.SHA,
						AgentName: t.Agent, AgentKind: t.Kind, LLMSpec: specOf(in, t.LLMSpec),
					},
					ExpName: in.Experiment.Name, PerTrial: in.Experiment.Pipeline.PerTrial,
					MaxTotalAttempts: in.Experiment.Retry.MaxTotalAttempts,
					RunnerQueue:      in.RunnerQueue, Timeouts: t.Case.Timeouts,
				}
				if _, err := runChild(gctx, in, ti, id); err != nil {
					workflow.GetLogger(gctx).Info("trial finished with failure", "trial", id, "err", err)
				}
			})
		}
	}
	_ = workflow.Await(ctx, func() bool { return inFlight == 0 })
}

// runChild starts a TrialWorkflow and distinguishes "never started" from
// "ran and failed": a child that cannot start leaves no row behind, so it has
// to be reported here rather than looking like a failed trial.
func runChild(ctx workflow.Context, in ExperimentInput, ti TrialInput, id string) (TrialResult, error) {
	cctx := workflow.WithChildOptions(ctx, workflow.ChildWorkflowOptions{
		WorkflowID:            id,
		TaskQueue:             temporalx.QueueOrchestrator,
		ParentClosePolicy:     enums.PARENT_CLOSE_POLICY_REQUEST_CANCEL,
		WorkflowIDReusePolicy: enums.WORKFLOW_ID_REUSE_POLICY_REJECT_DUPLICATE,
	})
	f := workflow.ExecuteChildWorkflow(cctx, TrialWorkflow, ti)
	if err := f.GetChildWorkflowExecution().Get(ctx, nil); err != nil {
		dctx, cancel := workflow.NewDisconnectedContext(ctx)
		defer cancel()
		_ = workflow.ExecuteActivity(temporalx.Server(dctx), (*activities.Deps).RecordTrialState,
			activities.TrialStateInput{Ref: ti.Ref, State: "FAILED",
				Code: "INTERNAL_ERROR", Category: "SYSTEM",
				Message: "child workflow failed to start: " + err.Error()}).Get(dctx, nil)
		return TrialResult{}, err
	}
	var res TrialResult
	err := f.Get(ctx, &res)
	return res, err
}

// -------------------------------------------------------- per_experiment ---

func perExperiment(ctx workflow.Context, in ExperimentInput) error {
	for _, st := range in.Experiment.Pipeline.PerExperiment {
		fi := activities.FlushInput{ExpID: in.ExpID, ExpName: in.Experiment.Name, Step: st}
		switch st.Step {
		case "upload":
			var n int
			c := temporalx.Runner(ctx, in.RunnerQueue, 30*time.Minute, time.Minute, 3)
			if err := workflow.ExecuteActivity(c, (*activities.Deps).FlushStaging, fi).Get(ctx, &n); err != nil {
				return err
			}
			workflow.GetLogger(ctx).Info("staged artifacts flushed", "files", n, "runner", in.RunnerID)
		case "report":
			var dir string
			if err := workflow.ExecuteActivity(temporalx.Server(ctx),
				(*activities.Deps).Report, fi).Get(ctx, &dir); err != nil {
				return err
			}
			workflow.GetLogger(ctx).Info("report written", "dir", dir)
		case "deploy":
			if err := workflow.ExecuteActivity(temporalx.Server(ctx),
				(*activities.Deps).Deploy, fi).Get(ctx, nil); err != nil {
				return err
			}
		}
	}
	return nil
}

// ---------------------------------------------------------------- utils ---

func specOf(in ExperimentInput, name string) *spec.LLMSpec {
	if name == "" {
		return nil
	}
	if s, ok := in.LLMSpecs[name]; ok {
		return &s
	}
	return nil
}

func taskID(expID, caseLabel, agent, llmSpec string) string {
	id := expID + "-" + slug(caseLabel) + "-" + slug(agent)
	if llmSpec != "" {
		id += "-" + slug(llmSpec)
	}
	return id
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

func short(s string) string {
	if len(s) > 12 {
		return s[:12]
	}
	return s
}
