// Package fail is the failure taxonomy.
//
// It exists for exactly one reason: to keep "we could not measure" out of the
// agent's numbers. Anything finer than that is reporting detail, so the list is
// short on purpose.
package fail

import (
	"errors"
	"fmt"
)

type Category string

const (
	Agent    Category = "AGENT"    // the agent's own doing: a measurement
	Env      Category = "ENV"      // the case environment could not be built or run
	Infra    Category = "INFRA"    // our machinery broke
	Verifier Category = "VERIFIER" // scoring itself failed
)

type Code string

const (
	AgentTimeout Code = "AGENT_TIMEOUT"  // ran out its own clock
	AgentFailed  Code = "AGENT_FAILED"   // crashed or exited non-zero
	EnvFailed    Code = "ENV_FAILED"     // image build / container start / env timeout
	Host         Code = "HOST_ERROR"     // disk, docker daemon, our own bugs
	VerifierBad  Code = "VERIFIER_ERROR" // verifier crashed, timed out, or produced no usable reward
	RedactFailed Code = "REDACT_FAILED"  // artifacts could not be scrubbed, so they must not ship
)

var categoryOf = map[Code]Category{
	AgentTimeout: Agent, AgentFailed: Agent,
	EnvFailed: Env, Host: Infra, RedactFailed: Infra, VerifierBad: Verifier,
}

func (c Code) Category() Category { return categoryOf[c] }

// CountsAgainstAgent reports whether a trial with this outcome belongs in the
// denominator when judging an agent. Infrastructure trouble does not.
func (c Code) CountsAgainstAgent() bool { return c.Category() == Agent }

// Retryable reports whether running the same trial again could plausibly
// succeed. An agent that failed on its own merits will fail again.
func (c Code) Retryable() bool {
	return c == Host || c == VerifierBad
}

type Error struct {
	Code Code
	Msg  string
	Err  error
}

func (e *Error) Error() string {
	if e.Err != nil {
		return fmt.Sprintf("%s: %s: %v", e.Code, e.Msg, e.Err)
	}
	return fmt.Sprintf("%s: %s", e.Code, e.Msg)
}
func (e *Error) Unwrap() error { return e.Err }

func New(c Code, msg string) error             { return &Error{Code: c, Msg: msg} }
func Wrap(c Code, msg string, err error) error { return &Error{Code: c, Msg: msg, Err: err} }

// Of extracts the code. Anything unrecognised becomes HOST_ERROR -- never an
// agent code, because blaming the agent for our own trouble corrupts the
// measurement while leaving nothing to notice.
func Of(err error) Code {
	if err == nil {
		return ""
	}
	var fe *Error
	if errors.As(err, &fe) {
		return fe.Code
	}
	return Host
}
