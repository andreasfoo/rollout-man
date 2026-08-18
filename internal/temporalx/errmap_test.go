package temporalx

import (
	"testing"
	"time"

	"go.temporal.io/api/enums/v1"
	"go.temporal.io/sdk/temporal"

	"github.com/andreasfoo/rollout-man/internal/failure"
)

// The same Temporal timeout means different things on different steps. Getting
// this wrong records infrastructure trouble as agent failure, which is a silent
// data-quality bug rather than an outage.
func TestTimeoutMapping(t *testing.T) {
	cases := []struct {
		name string
		step Step
		tt   enums.TimeoutType
		want failure.Code
	}{
		{"agent ran out its own clock", StepRunAgent, enums.TIMEOUT_TYPE_START_TO_CLOSE, failure.AgentTimeout},
		{"runner stopped heartbeating", StepRunAgent, enums.TIMEOUT_TYPE_HEARTBEAT, failure.HostError},
		{"nothing picked the task up", StepRunAgent, enums.TIMEOUT_TYPE_SCHEDULE_TO_START, failure.HostError},
		{"verifier overran", StepVerify, enums.TIMEOUT_TYPE_START_TO_CLOSE, failure.VerifierTimeout},
		{"build overran", StepPrepare, enums.TIMEOUT_TYPE_START_TO_CLOSE, failure.EnvironmentTimeout},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := temporal.NewTimeoutError(c.tt, nil)
			if got := FromError(c.step, err); got != c.want {
				t.Fatalf("got %s, want %s", got, c.want)
			}
		})
	}
}

func TestApplicationErrorKeepsItsCode(t *testing.T) {
	err := temporal.NewApplicationError("nope", string(failure.AgentExitNonzero))
	if got := FromError(StepRunAgent, err); got != failure.AgentExitNonzero {
		t.Fatalf("got %s", got)
	}
}

// Nothing unrecognised may become an AGENT code.
func TestUnknownIsNeverBlamedOnTheAgent(t *testing.T) {
	got := FromError(StepRunAgent, errString("something else entirely"))
	if got.Category() == failure.Agent {
		t.Fatalf("unknown error was attributed to the agent as %s", got)
	}
	if got != failure.InternalError {
		t.Fatalf("got %s, want INTERNAL_ERROR", got)
	}
}

type errString string

func (e errString) Error() string { return string(e) }

var _ = time.Second
