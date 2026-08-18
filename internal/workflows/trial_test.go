package workflows

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/mock"
	"go.temporal.io/api/enums/v1"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/testsuite"

	"github.com/andreasfoo/rollout-man/internal/activities"
	"github.com/andreasfoo/rollout-man/internal/failure"
)

func newEnv(t *testing.T) *testsuite.TestWorkflowEnvironment {
	var s testsuite.WorkflowTestSuite
	env := s.NewTestWorkflowEnvironment()
	env.RegisterWorkflow(TrialWorkflow)
	var d *activities.Deps
	env.RegisterActivity(d.PrepareCase)
	env.RegisterActivity(d.RunAgent)
	env.RegisterActivity(d.RunVerifier)
	env.RegisterActivity(d.CollectArtifacts)
	env.RegisterActivity(d.PostProcess)
	env.RegisterActivity(d.CleanupTrial)
	env.RegisterActivity(d.RecordTrialState)
	return env
}

func input(maxAttempts int) TrialInput {
	return TrialInput{
		Ref:              activities.TrialRef{TrialID: "t-1", TaskID: "task-1", TrialIndex: 1},
		MaxTotalAttempts: maxAttempts,
		RunnerQueue:      "runner.test",
		Timeouts:         Timeouts{Agent: time.Minute, Verifier: time.Minute, Build: time.Minute},
	}
}

func TestHappyPath(t *testing.T) {
	env := newEnv(t)
	var d *activities.Deps
	env.OnActivity(d.RecordTrialState, mock.Anything, mock.Anything).Return(nil)
	env.OnActivity(d.PrepareCase, mock.Anything, mock.Anything).Return(nil)
	env.OnActivity(d.RunAgent, mock.Anything, mock.Anything).Return(nil)
	env.OnActivity(d.RunVerifier, mock.Anything, mock.Anything).Return(0.75, nil)
	env.OnActivity(d.CollectArtifacts, mock.Anything, mock.Anything).Return(nil)
	env.OnActivity(d.PostProcess, mock.Anything, mock.Anything).Return(&activities.PostProcessOutput{}, nil)
	env.OnActivity(d.CleanupTrial, mock.Anything, mock.Anything).Return(nil)

	env.ExecuteWorkflow(TrialWorkflow, input(1))
	if !env.IsWorkflowCompleted() || env.GetWorkflowError() != nil {
		t.Fatalf("workflow did not complete: %v", env.GetWorkflowError())
	}
	var res TrialResult
	env.GetWorkflowResult(&res)
	if res.Reward != 0.75 {
		t.Fatalf("reward = %v, want 0.75", res.Reward)
	}
}

// An agent that fails on its own merits is a measurement, not an incident: it
// must not be retried onto another machine.
func TestAgentFailureIsNotRetried(t *testing.T) {
	env := newEnv(t)
	var d *activities.Deps
	env.OnActivity(d.RecordTrialState, mock.Anything, mock.Anything).Return(nil)
	env.OnActivity(d.PrepareCase, mock.Anything, mock.Anything).Return(nil)
	calls := 0
	env.OnActivity(d.RunAgent, mock.Anything, mock.Anything).Return(
		func(_ context.Context, _ activities.TrialRef) error {
			calls++
			return temporal.NewApplicationError("agent exited 1", string(failure.AgentExitNonzero))
		})
	env.OnActivity(d.CleanupTrial, mock.Anything, mock.Anything).Return(nil)

	env.ExecuteWorkflow(TrialWorkflow, input(3))
	if env.GetWorkflowError() == nil {
		t.Fatal("expected the trial to fail")
	}
	if calls != 1 {
		t.Fatalf("RunAgent ran %d times; an agent-side failure must not be retried", calls)
	}
}

// Infrastructure trouble is the opposite: it should come back around.
func TestInfraFailureIsRetried(t *testing.T) {
	env := newEnv(t)
	var d *activities.Deps
	env.OnActivity(d.RecordTrialState, mock.Anything, mock.Anything).Return(nil)
	prepares := 0
	env.OnActivity(d.PrepareCase, mock.Anything, mock.Anything).Return(
		func(_ context.Context, _ activities.TrialRef) error {
			prepares++
			if prepares < 2 {
				return temporal.NewApplicationError("docker went away", string(failure.DockerError))
			}
			return nil
		})
	env.OnActivity(d.RunAgent, mock.Anything, mock.Anything).Return(nil)
	env.OnActivity(d.RunVerifier, mock.Anything, mock.Anything).Return(1.0, nil)
	env.OnActivity(d.CollectArtifacts, mock.Anything, mock.Anything).Return(nil)
	env.OnActivity(d.PostProcess, mock.Anything, mock.Anything).Return(&activities.PostProcessOutput{}, nil)
	env.OnActivity(d.CleanupTrial, mock.Anything, mock.Anything).Return(nil)

	env.ExecuteWorkflow(TrialWorkflow, input(3))
	if err := env.GetWorkflowError(); err != nil {
		t.Fatalf("workflow should have recovered: %v", err)
	}
	if prepares < 2 {
		t.Fatalf("PrepareCase ran %d times; an infrastructure failure must be retried", prepares)
	}
}

// Cleanup runs on a disconnected context, so it still happens when the step
// chain fails -- otherwise a failed trial leaves its container behind.
func TestCleanupRunsOnFailure(t *testing.T) {
	env := newEnv(t)
	var d *activities.Deps
	env.OnActivity(d.RecordTrialState, mock.Anything, mock.Anything).Return(nil)
	env.OnActivity(d.PrepareCase, mock.Anything, mock.Anything).Return(nil)
	env.OnActivity(d.RunAgent, mock.Anything, mock.Anything).Return(
		temporal.NewApplicationError("boom", string(failure.AgentCrash)))
	cleaned := 0
	env.OnActivity(d.CleanupTrial, mock.Anything, mock.Anything).Return(
		func(_ context.Context, _ activities.TrialRef) error { cleaned++; return nil })

	env.ExecuteWorkflow(TrialWorkflow, input(1))
	if cleaned == 0 {
		t.Fatal("cleanup did not run on the failure path")
	}
}

var _ = enums.TIMEOUT_TYPE_HEARTBEAT
