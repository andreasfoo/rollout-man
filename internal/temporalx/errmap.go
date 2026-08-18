// Package temporalx holds the glue between the taxonomy and Temporal:
// error classification, activity options, and id conventions.
package temporalx

import (
	"errors"

	"go.temporal.io/api/enums/v1"
	"go.temporal.io/sdk/temporal"

	"github.com/andreasfoo/rollout-man/internal/failure"
)

// Step names the activity a failure came from. The same Temporal timeout means
// different things depending on which step timed out, so the caller has to say.
type Step string

const (
	StepResolve  Step = "ResolveCase"
	StepPrepare  Step = "PrepareCase"
	StepRunAgent Step = "RunAgent"
	StepVerify   Step = "RunVerifier"
	StepCollect  Step = "CollectArtifacts"
	StepPost     Step = "PostProcess"
	StepCleanup  Step = "CleanupTrial"
	StepServer   Step = "ServerActivity"
)

// FromError maps a Temporal activity error onto the taxonomy.
//
// The subtle part is timeouts. RunAgent's StartToClose IS the agent timeout, so
// it means the agent ran out its own clock -- a capability result. A heartbeat
// timeout on the same activity means the runner went away, and a
// schedule-to-start timeout means nothing picked the work up. Collapsing them
// blames the agent for infrastructure trouble, which corrupts the measurement
// while every dashboard stays green.
func FromError(step Step, err error) failure.Code {
	if err == nil {
		return ""
	}
	var canceled *temporal.CanceledError
	if errors.As(err, &canceled) {
		return failure.Cancelled
	}
	// An application error carries the code the activity already decided on.
	var app *temporal.ApplicationError
	if errors.As(err, &app) {
		if c := failure.Code(app.Type()); c.Category() != "" {
			return c
		}
	}
	var to *temporal.TimeoutError
	if errors.As(err, &to) {
		return fromTimeout(step, to.TimeoutType())
	}
	return failure.InternalError
}

func fromTimeout(step Step, t enums.TimeoutType) failure.Code {
	switch t {
	case enums.TIMEOUT_TYPE_HEARTBEAT:
		// the worker stopped proving it was alive
		return failure.HostError
	case enums.TIMEOUT_TYPE_SCHEDULE_TO_START:
		// nothing polled the queue: the runner is gone or saturated. Not a
		// reason to blacklist the runner -- it may simply be busy.
		return failure.HostError
	}
	switch step {
	case StepRunAgent:
		return failure.AgentTimeout
	case StepVerify:
		return failure.VerifierTimeout
	case StepPrepare:
		return failure.EnvironmentTimeout
	default:
		return failure.HostError
	}
}

// NonRetryable lists the codes an activity must never retry in place: they are
// facts about the run, not transient trouble.
func NonRetryable() []string {
	return []string{
		string(failure.AgentTimeout), string(failure.AgentCrash), string(failure.AgentExitNonzero),
		string(failure.ImageBuildFailed), string(failure.EnvironmentTimeout),
		string(failure.InvalidReward), string(failure.PostprocessFailed),
		string(failure.Cancelled), string(failure.Unplaced),
	}
}
