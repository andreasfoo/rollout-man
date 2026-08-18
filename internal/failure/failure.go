// Package failure is the failure taxonomy and the single place errors are
// classified. Nothing may default to an AGENT code.
package failure

import (
	"errors"
	"fmt"
)

type Category string

const (
	Agent          Category = "AGENT"
	Environment    Category = "ENVIRONMENT"
	Infrastructure Category = "INFRASTRUCTURE"
	Verifier       Category = "VERIFIER"
	System         Category = "SYSTEM"
)

type Code string

const (
	AgentTimeout       Code = "AGENT_TIMEOUT"
	AgentCrash         Code = "AGENT_CRASH"
	AgentExitNonzero   Code = "AGENT_EXIT_NONZERO"
	ImageBuildFailed   Code = "IMAGE_BUILD_FAILED"
	ContainerStart     Code = "CONTAINER_START_FAILED"
	EnvironmentTimeout Code = "ENVIRONMENT_TIMEOUT"
	DockerError        Code = "DOCKER_ERROR"
	ObjectStoreError   Code = "OBJECT_STORE_ERROR"
	HostError          Code = "HOST_ERROR"
	VerifierError      Code = "VERIFIER_ERROR"
	VerifierTimeout    Code = "VERIFIER_TIMEOUT"
	InvalidReward      Code = "INVALID_REWARD"
	Cancelled          Code = "CANCELLED"
	Unplaced           Code = "UNPLACED"
	PostprocessFailed  Code = "POSTPROCESS_FAILED"
	InternalError      Code = "INTERNAL_ERROR"
	Unknown            Code = "UNKNOWN"
)

var categoryOf = map[Code]Category{
	AgentTimeout: Agent, AgentCrash: Agent, AgentExitNonzero: Agent,
	ImageBuildFailed: Environment, ContainerStart: Environment, EnvironmentTimeout: Environment,
	DockerError: Infrastructure, ObjectStoreError: Infrastructure, HostError: Infrastructure,
	VerifierError: Verifier, VerifierTimeout: Verifier, InvalidReward: Verifier,
	Cancelled: System, Unplaced: System, PostprocessFailed: System,
	InternalError: System, Unknown: System,
}

// retryable on a different runner.
var reRunnable = map[Code]bool{
	DockerError: true, HostError: true, ContainerStart: true,
	VerifierError: true, InternalError: true,
}

// excludesRunner reports whether the runner should be avoided on retry.
// Object-store failures are deliberately excluded: the backend is shared, so
// moving to another machine changes nothing.
var excludes = map[Code]bool{DockerError: true, HostError: true}

func (c Code) Category() Category { return categoryOf[c] }
func (c Code) Retryable() bool    { return reRunnable[c] }
func (c Code) ExcludesRunner() bool {
	return excludes[c]
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

func Wrap(code Code, msg string, err error) error { return &Error{Code: code, Msg: msg, Err: err} }
func New(code Code, msg string) error             { return &Error{Code: code, Msg: msg} }

// FromError extracts the taxonomy code. Anything unrecognised becomes
// SYSTEM/INTERNAL_ERROR -- never an AGENT code, because misattributing
// infrastructure trouble to the agent silently corrupts the measurement.
func FromError(err error) Code {
	if err == nil {
		return ""
	}
	var fe *Error
	if errors.As(err, &fe) {
		return fe.Code
	}
	return InternalError
}
