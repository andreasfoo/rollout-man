// Package workflows holds the deterministic orchestration. No I/O here.
package workflows

import (
	"time"

	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"

	"github.com/andreasfoo/rollout-man/internal/activities"
	"github.com/andreasfoo/rollout-man/internal/failure"
	"github.com/andreasfoo/rollout-man/internal/spec"
	"github.com/andreasfoo/rollout-man/internal/temporalx"
)

type TrialInput struct {
	Ref              activities.TrialRef
	ExpName          string
	PerTrial         []spec.Step
	MaxTotalAttempts int
	RunnerQueue      string
	Timeouts         Timeouts

	// carried across continue-as-new so a resumed run does not restart at
	// attempt 1 and lose its avoid-same-runner record
	ResumeAttempt int
	Exclude       []string
}

type Timeouts struct {
	Agent    time.Duration
	Verifier time.Duration
	Build    time.Duration
}

type TrialResult struct {
	TrialID string
	Reward  float64
	Code    string
}

// TrialWorkflow is the durable unit: an attempt loop around a pinned step
// chain, with compensation that still runs after cancellation.
func TrialWorkflow(ctx workflow.Context, in TrialInput) (TrialResult, error) {
	attempt := in.ResumeAttempt
	if attempt < 1 {
		attempt = 1
	}
	exclude := in.Exclude
	maxAttempts := in.MaxTotalAttempts
	if maxAttempts < 1 {
		maxAttempts = 1
	}

	var last error
	var lastCode failure.Code
	for ; attempt <= maxAttempts; attempt++ {
		res, code, err := runAttempt(ctx, in, attempt)
		if err == nil {
			return res, nil
		}
		last, lastCode = err, code
		recordTerminal(ctx, in, attempt, "FAILED", 0, code, err.Error())

		if code == failure.Cancelled || !code.Retryable() || attempt == maxAttempts {
			break
		}
		if code.ExcludesRunner() {
			exclude = append(exclude, in.RunnerQueue)
		}
		if err := workflow.Sleep(ctx, backoff(attempt)); err != nil {
			break
		}
	}
	return TrialResult{TrialID: in.Ref.TrialID, Code: string(lastCode)}, last
}

func runAttempt(ctx workflow.Context, in TrialInput, attempt int) (TrialResult, failure.Code, error) {
	ref := in.Ref
	ref.Attempt = attempt
	q := in.RunnerQueue
	start := workflow.Now(ctx)

	// Compensation is registered before anything is acquired: the cancellation
	// path leaves through the same door, and a defer added later would be
	// skipped exactly when it matters.
	defer func() {
		dctx, cancel := workflow.NewDisconnectedContext(ctx)
		defer cancel()
		_ = workflow.ExecuteActivity(temporalx.Cleanup(dctx, q),
			(*activities.Deps).CleanupTrial, ref).Get(dctx, nil)
	}()

	recordTerminal(ctx, in, attempt, "RUNNING", 0, "", "")

	step := func(s temporalx.Step, name any, arg any, out any, to, hb time.Duration, max int32) (failure.Code, error) {
		c := temporalx.Runner(ctx, q, to, hb, max)
		if err := workflow.ExecuteActivity(c, name, arg).Get(ctx, out); err != nil {
			return temporalx.FromError(s, err), err
		}
		return "", nil
	}

	if code, err := step(temporalx.StepPrepare, (*activities.Deps).PrepareCase, ref, nil,
		in.Timeouts.Build, time.Minute, 1); err != nil {
		return TrialResult{}, code, err
	}
	// RunAgent gets more than one in-place attempt on purpose: a heartbeat
	// timeout must be able to re-deliver to the same queue, which is the only
	// way the container-takeover path is ever reached. Real agent failures are
	// in NonRetryableErrorTypes and still fall straight through.
	if code, err := step(temporalx.StepRunAgent, (*activities.Deps).RunAgent, ref, nil,
		in.Timeouts.Agent, time.Minute, 3); err != nil {
		return TrialResult{}, code, err
	}
	var reward float64
	if code, err := step(temporalx.StepVerify, (*activities.Deps).RunVerifier, ref, &reward,
		in.Timeouts.Verifier, time.Minute, 2); err != nil {
		return TrialResult{}, code, err
	}
	if code, err := step(temporalx.StepCollect, (*activities.Deps).CollectArtifacts, ref, nil,
		15*time.Minute, 30*time.Second, 3); err != nil {
		return TrialResult{}, code, err
	}

	var pp activities.PostProcessOutput
	if code, err := step(temporalx.StepPost, (*activities.Deps).PostProcess,
		activities.PostProcessInput{Ref: ref, Steps: in.PerTrial, Reward: reward, ExpName: in.ExpName},
		&pp, 30*time.Minute, time.Minute, 3); err != nil {
		return TrialResult{}, code, err
	}

	recordTerminal(ctx, in, attempt, "COMPLETED", reward, "",
		workflow.Now(ctx).Sub(start).String())
	return TrialResult{TrialID: ref.TrialID, Reward: reward}, "", nil
}

// recordTerminal writes the read model. Terminal writes go through a
// disconnected context: on cancellation the workflow context is already dead,
// and a trial that silently stays RUNNING in Postgres is worse than no row.
func recordTerminal(ctx workflow.Context, in TrialInput, attempt int, state string, reward float64, code failure.Code, msg string) {
	ref := in.Ref
	ref.Attempt = attempt
	c := ctx
	if state != "RUNNING" {
		dctx, cancel := workflow.NewDisconnectedContext(ctx)
		defer cancel()
		c = dctx
	}
	_ = workflow.ExecuteActivity(temporalx.Server(c), (*activities.Deps).RecordTrialState,
		activities.TrialStateInput{
			Ref: ref, State: state, Reward: reward,
			Code: string(code), Category: string(code.Category()), Message: msg,
		}).Get(c, nil)
}

func backoff(attempt int) time.Duration {
	d := time.Duration(1<<uint(attempt-1)) * 15 * time.Second
	if d > 10*time.Minute {
		d = 10 * time.Minute
	}
	return d
}

var _ = temporal.NewApplicationError
