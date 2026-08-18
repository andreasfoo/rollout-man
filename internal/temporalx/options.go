package temporalx

import (
	"time"

	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"
)

const (
	QueueOrchestrator = "orchestrator"
	RunnerQueuePrefix = "runner."
)

func RunnerQueue(id string) string { return RunnerQueuePrefix + id }

// Server returns options for a server-side activity.
func Server(ctx workflow.Context) workflow.Context {
	return workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		TaskQueue:              QueueOrchestrator,
		StartToCloseTimeout:    2 * time.Minute,
		ScheduleToStartTimeout: 5 * time.Minute,
		RetryPolicy:            &temporal.RetryPolicy{MaximumAttempts: 3},
	})
}

// Runner returns options for an activity pinned to one machine.
//
// ScheduleToStartTimeout is mandatory here: it is what replaces the hand-written
// assignment lease. Without it an activity aimed at a dead worker sits in
// Scheduled forever, holding its reservation and never failing over.
func Runner(ctx workflow.Context, queue string, startToClose, heartbeat time.Duration, maxAttempts int32) workflow.Context {
	return workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		TaskQueue:              queue,
		StartToCloseTimeout:    startToClose,
		HeartbeatTimeout:       heartbeat,
		ScheduleToStartTimeout: 5 * time.Minute,
		RetryPolicy: &temporal.RetryPolicy{
			MaximumAttempts:        maxAttempts,
			NonRetryableErrorTypes: NonRetryable(),
		},
	})
}

// Cleanup is deliberately impatient. A missing runner is the most common reason
// cleanup fails, and a blocking compensation turns every failover into a
// multi-minute stall.
func Cleanup(ctx workflow.Context, queue string) workflow.Context {
	return workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		TaskQueue:              queue,
		StartToCloseTimeout:    60 * time.Second,
		ScheduleToStartTimeout: 30 * time.Second,
		RetryPolicy:            &temporal.RetryPolicy{MaximumAttempts: 1},
	})
}
