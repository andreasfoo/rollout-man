// Package actions is the pipeline's vocabulary.
//
// A pipeline is three lists of steps, one per unit of work, and a step is an
// action with inputs. That shape is not decoration: it is what lets one
// submission say "run it, scrub it, keep only the hard ones, pack them, publish
// them as a dataset" without any of those five being a special case in the
// runner.
//
// Two rules keep it from turning into a scripting language:
//
//   - Every action declares the inputs it understands, and an input it does not
//     understand is an error at load time. A misspelled key would otherwise run
//     the step with its defaults and look like success.
//   - Actions transform and decide; anything that reaches outside the machine is
//     a command (kind: Commands), which is where the pin and the env allowlist
//     already live.
package actions

import (
	"context"
	"errors"
	"fmt"
	"sort"

	"github.com/andreasfoo/rollout-man/internal/cmdrun"
	"github.com/andreasfoo/rollout-man/internal/config"
)

type Scope string

const (
	PerCase       Scope = "per_case"
	PerTrial      Scope = "per_trial"
	PerExperiment Scope = "per_experiment"
)

// Trial is what a per_trial action may look at: a copy of the facts, not the
// runner's record. An action may decide this trial's artifacts should not be
// published; nothing else about the measurement is an action's business.
type Trial struct {
	ID      string
	Case    string
	CaseSHA string
	Agent   string
	LLMSpec string
	Index   int
	Reward  *float64
	Code    string
	Seconds float64
	OutDir  string
}

// Ctx is the world one step runs in.
type Ctx struct {
	Scope      Scope
	Experiment string
	RunID      string
	RunDir     string

	// per_case
	CaseLabel string
	CaseDir   string
	CaseSHA   string

	// per_trial
	Trial   *Trial
	Secrets []string // values that must not survive into an artifact

	// Drop marks this trial's artifacts as not for publishing. The measurement
	// still stands and is still recorded: dropping decides what leaves the
	// machine, never what was observed.
	Drop bool
	// Notes are recorded on the trial's line in results.jsonl.
	Notes map[string]any

	Cmds *cmdrun.Runner
	Log  func(string, ...any)

	// Probe runs the admission probes through the ordinary execution path.
	Probe func(ctx context.Context, kind string, n int) ([]float64, error)
	// Trials lists every recorded trial, for per_experiment actions.
	Trials func() []Trial
	// Dropped reports whether a trial was dropped by a guard.
	Dropped func(trialID string) bool
	// Enter is called as each step begins, so the middle of a pipeline is
	// visible while it is happening rather than only in hindsight.
	Enter func(step string)

	// StepOutputs holds each prior step's $ROLLOUT_MAN_OUTPUT, keyed by the
	// step's label (name: if set, else uses:). A later step in the same list
	// reads ${{ steps.<label>.outputs.<key> }} in its with: values.
	StepOutputs map[string]map[string]string
}

func (c *Ctx) Note(k string, v any) {
	if c.Notes == nil {
		c.Notes = map[string]any{}
	}
	c.Notes[k] = v
}

func (c *Ctx) Logf(f string, a ...any) {
	if c.Log != nil {
		c.Log(f, a...)
	}
}

func (c *Ctx) Where() string {
	switch {
	case c.Trial != nil:
		return c.Trial.ID
	case c.CaseLabel != "":
		return c.CaseLabel
	}
	return c.Experiment
}

// Action is one step. Validate runs before the batch starts, so a bad
// submission is rejected at load time rather than three hours in.
type Action interface {
	Name() string
	Scopes() []Scope
	Validate(a config.Action) error
	Run(ctx context.Context, c *Ctx, a config.Action) error
}

var registry = map[string]Action{}

// ErrSkipCase is returned by RunList when a per_case step fails with
// on_failure: skip. The runner interprets it as "exclude this case, continue
// with the next one" rather than aborting the whole run.
var ErrSkipCase = errors.New("case skipped")

func register(a Action) { registry[a.Name()] = a }

func Names() []string {
	out := make([]string, 0, len(registry))
	for k := range registry {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Resolve turns one configured step into something runnable. A `uses` that is
// not a built-in falls back to a configured command: that is how a custom step
// is written -- no plugin registry, no new syntax, and it inherits the pin and
// the env allowlist that commands already have.
func Resolve(a config.Action, cmds *cmdrun.Runner) (Action, error) {
	if act, ok := registry[a.Uses]; ok {
		return act, nil
	}
	if cmds != nil && cmds.Has(a.Uses) {
		return command{cmd: a.Uses}, nil
	}
	return nil, fmt.Errorf("unknown action %q: not a built-in %v and not a configured command",
		a.Uses, Names())
}

// ValidateList checks a unit before anything runs. `from` is the index the
// list starts at in the submission, so a reported position points at the line
// the author actually wrote.
func ValidateList(scope Scope, list []config.Action, from int, cmds *cmdrun.Runner) error {
	for off, a := range list {
		i := off + from
		act, err := Resolve(a, cmds)
		if err != nil {
			return fmt.Errorf("pipeline.%s[%d]: %w", scope, i, err)
		}
		if !allows(act, scope) {
			return fmt.Errorf("pipeline.%s[%d]: %q does not run %s (it runs %v)",
				scope, i, a.Uses, scope, act.Scopes())
		}
		if err := act.Validate(a); err != nil {
			return fmt.Errorf("pipeline.%s[%d] (%s): %w", scope, i, a.Label(), err)
		}
		if a.Fix != "" {
			if cmds == nil || !cmds.Has(a.Fix) {
				return fmt.Errorf("pipeline.%s[%d] (%s): fix: %q is not a configured command",
					scope, i, a.Label(), a.Fix)
			}
		}
	}
	return nil
}

func allows(a Action, s Scope) bool {
	for _, x := range a.Scopes() {
		if x == s {
			return true
		}
	}
	return false
}

// RunList executes a unit's steps in order. A step that fails stops the unit,
// unless it declared on_failure: warn.
func RunList(ctx context.Context, c *Ctx, list []config.Action, cmds *cmdrun.Runner) error {
	for _, a := range list {
		// A drop is a publication decision. Once a guard has made it, later
		// post-processing must not scrub, archive, or ship a rejected trial.
		// Put bookkeeping that must see both verdicts before the guard.
		if c.Scope == PerTrial && c.Drop {
			c.Logf("%s: skip %s (already dropped)", c.Where(), a.Label())
			continue
		}
		act, err := Resolve(a, cmds)
		if err != nil {
			return err
		}
		if c.Enter != nil {
			c.Enter(a.Label())
		}
		err = act.Run(ctx, c, a)
		if err != nil && a.Fix != "" {
			if !a.FixWriteback {
				// A fix: without fix_writeback: true is declared but inert --
				// naming a repair command is not the same as consenting to it
				// mutating the case directory. This is intentionally silent
				// at the info level (would spam every failing case) but shows
				// up in the failure message so it isn't a total mystery.
				c.Logf("%s: %s failed (fix %s not applied: fix_writeback is false): %v",
					c.Where(), a.Label(), a.Fix, err)
			} else {
				c.Logf("%s: %s failed, attempting fix (%s): %v", c.Where(), a.Label(), a.Fix, err)
				if ferr := runFix(ctx, c, a.Fix); ferr != nil {
					c.Logf("%s: fix %s itself failed: %v", c.Where(), a.Fix, ferr)
				} else {
					c.Logf("%s: fix %s applied, retrying %s", c.Where(), a.Fix, a.Label())
					err = act.Run(ctx, c, a)
				}
			}
		}
		if err != nil {
			if a.Warns() {
				c.Logf("%s: %s failed (on_failure: warn): %v", c.Where(), a.Label(), err)
				continue
			}
			if a.Skips() {
				c.Logf("%s: %s failed (on_failure: skip): %v", c.Where(), a.Label(), err)
				return ErrSkipCase
			}
			return fmt.Errorf("%s: %w", a.Label(), err)
		}
	}
	return nil
}

// runFix runs the command named by a step's fix: with the same case context
// the step itself sees, so a fix adapter can read and rewrite the same
// CASE_DIR the failing check just rejected. It gets no with: of its own --
// the fix is a fixed remedy for a fixed check, not another configurable step.
func runFix(ctx context.Context, c *Ctx, name string) error {
	vars := map[string]string{
		"Experiment": c.Experiment, "RunId": c.RunID, "RunDir": c.RunDir,
		"LocalPath": c.RunDir,
	}
	if c.CaseDir != "" {
		vars["CaseDir"], vars["CaseLabel"], vars["CaseSha"] = c.CaseDir, c.CaseLabel, c.CaseSHA
	}
	_, err := c.Cmds.Run(ctx, name, vars)
	return err
}

func unknown(a config.Action, known ...string) error {
	if u := a.Unknown(known...); len(u) > 0 {
		return fmt.Errorf("unknown input(s) %v; this action takes %v", u, known)
	}
	return nil
}
