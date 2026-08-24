// Package config parses the submission file: kind: Commands | LLMSpec |
// Experiment, one file, several YAML documents.
package config

import (
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Command is one external step. Three forms, in increasing order of how much
// you can say about what actually ran:
//
//	run:     argv, rendered from {{.Vars}}
//	script:  inline shell
//	uses:    a script file, optionally pinned by sha256
//
// `uses` is the one to reach for when the command is a real adapter: the code
// lives in a file you can review, and the pin makes "the thing that ran is the
// thing I reviewed" checkable instead of assumed.
type Command struct {
	Run    []string `yaml:"run"`
	Script string   `yaml:"script"`
	Uses   string   `yaml:"uses"`
	SHA256 string   `yaml:"sha256"`
	// Env names this command may read from the host, when inherit_env is off.
	Env []string `yaml:"env"`
}

func (c Command) Empty() bool {
	return len(c.Run) == 0 && strings.TrimSpace(c.Script) == "" && c.Uses == ""
}

type Commands struct {
	Timeout     Duration
	MaxAttempts int
	// InheritEnv decides whether commands see the host environment. Default
	// true, because an operator running their own submissions on their own
	// machine is the common case. A shared runner should set it false and let
	// each command declare the names it needs -- an adapter has no business
	// reading the credentials of the systems it is not talking to.
	InheritEnv *bool
	Cmds       map[string]Command

	// Trusted marks commands that came from the runner's own config rather
	// than from a submission. Set by LoadCommands, never by YAML.
	Trusted bool `yaml:"-"`
}

func (c Commands) Inherits() bool { return c.InheritEnv == nil || *c.InheritEnv }

// UnmarshalYAML walks the mapping: every mapping-valued key is a command, the
// few scalar keys are settings.
func (c *Commands) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind != yaml.MappingNode {
		return fmt.Errorf("kind Commands must be a mapping")
	}
	c.Cmds = map[string]Command{}
	for i := 0; i+1 < len(n.Content); i += 2 {
		key, val := n.Content[i].Value, n.Content[i+1]
		switch key {
		case "kind":
		case "timeout":
			if err := c.Timeout.UnmarshalYAML(val); err != nil {
				return err
			}
		case "max_attempts":
			if err := val.Decode(&c.MaxAttempts); err != nil {
				return err
			}
		case "inherit_env":
			var b bool
			if err := val.Decode(&b); err != nil {
				return err
			}
			c.InheritEnv = &b
		default:
			if val.Kind != yaml.MappingNode {
				return fmt.Errorf("command %q must be a mapping with run:, script: or uses:", key)
			}
			var cmd Command
			if err := val.Decode(&cmd); err != nil {
				return fmt.Errorf("command %q: %w", key, err)
			}
			if cmd.Empty() {
				return fmt.Errorf("command %q has none of run, script or uses", key)
			}
			c.Cmds[key] = cmd
		}
	}
	return nil
}

type LLMSpec struct {
	Name      string         `yaml:"name"`
	Provider  string         `yaml:"provider"`
	BaseURL   string         `yaml:"base_url"`
	Model     string         `yaml:"model"`
	APIKeyEnv string         `yaml:"api_key_env"`
	APIKeyCmd []string       `yaml:"api_key_cmd"`
	Params    map[string]any `yaml:"parameters"`
}

type CaseRef struct {
	Source string `yaml:"source"` // local | git | hf
	Repo   string `yaml:"repo"`
	Ref    string `yaml:"ref"`
	Path   string `yaml:"path"`
	SHA256 string `yaml:"sha256"`
	Fetch  string `yaml:"fetch"`
}

func (c CaseRef) Merge(d CaseRef) CaseRef {
	pick := func(a, b string) string {
		if a != "" {
			return a
		}
		return b
	}
	return CaseRef{
		Source: pick(c.Source, d.Source), Repo: pick(c.Repo, d.Repo),
		Ref: pick(c.Ref, d.Ref), Path: pick(c.Path, d.Path),
		SHA256: pick(c.SHA256, d.SHA256), Fetch: pick(c.Fetch, d.Fetch),
	}
}

func (c CaseRef) Label() string {
	if c.Path != "" {
		return c.Path
	}
	return c.SHA256
}

type AgentRef struct {
	Name    string `yaml:"name"`
	LLMSpec string `yaml:"llm_spec"`
	// Trials overrides matrix.trials for this agent. Rarely needed: the
	// default already distinguishes the two kinds of agent (see Rollouts).
	Trials *int `yaml:"trials"`
}

// Rollouts says how many times to run this agent per case.
//
// matrix.trials exists because agents are stochastic -- the same agent on the
// same case produces a distribution, and one sample of it is not a
// measurement. The built-ins are not stochastic: `oracle` runs the case's own
// solve script and `nop` does nothing, so running either eight times produces
// eight identical numbers and eight times the container cost. They get one.
//
// If what you want from oracle is the guarantee that the case scores 1.0, that
// is the admission gate's job, not the matrix's -- and the gate already runs it.
func (a AgentRef) Rollouts(matrixTrials int) int {
	if a.Trials != nil {
		return *a.Trials
	}
	if a.Deterministic() {
		return 1
	}
	return matrixTrials
}

func (a AgentRef) Deterministic() bool { return a.Name == "oracle" || a.Name == "nop" }

func (a *AgentRef) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind == yaml.ScalarNode {
		a.Name = n.Value
		return nil
	}
	type raw AgentRef
	var r raw
	if err := n.Decode(&r); err != nil {
		return err
	}
	*a = AgentRef(r)
	return nil
}

type Matrix struct {
	Agents   []AgentRef `yaml:"agents"`
	LLMSpecs []string   `yaml:"llm_specs"`
	Trials   int        `yaml:"trials"`
}

// Action is one step in a pipeline. The shape is the one people already know
// from CI: a name, its inputs, and what a failure means.
//
//   - uses: redact
//     with: {keys: required}
//     on_failure: fail
//
// `uses` names either a built-in action or a command from kind: Commands, so a
// custom step needs no new syntax -- it is a command like every other external
// thing this system touches, pin and all.
type Action struct {
	Uses string         `yaml:"uses"`
	Name string         `yaml:"name"` // optional label for the log
	With map[string]any `yaml:"with"`
	// OnFailure decides what a failing step means for the unit it runs on:
	// "fail" (default) stops that unit, "warn" records it and continues.
	OnFailure string `yaml:"on_failure"`
	// OnViolation is the guard's verdict: "fail" (default), "warn", or "drop"
	// -- keep the measurement, publish nothing from it.
	OnViolation string `yaml:"on_violation"`
}

// UnmarshalYAML rejects keys this struct does not have. Plain struct decoding
// ignores them, which means a step written with `on_violation:` one level too
// high runs with its default and looks like it worked -- the exact failure the
// `with:` checks exist to prevent, one level up.
func (a *Action) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind != yaml.MappingNode {
		return fmt.Errorf("a pipeline step must be a mapping with uses:")
	}
	type plain Action
	for i := 0; i+1 < len(n.Content); i += 2 {
		switch k := n.Content[i].Value; k {
		case "uses", "name", "with", "on_failure", "on_violation":
		default:
			return fmt.Errorf("unknown key %q in a pipeline step; "+
				"a step takes uses, name, with, on_failure, on_violation", k)
		}
	}
	return n.Decode((*plain)(a))
}

func (a Action) Label() string {
	if a.Name != "" {
		return a.Name
	}
	return a.Uses
}

func (a Action) Warns() bool { return a.OnFailure == "warn" }

func (a Action) Has(k string) bool { _, ok := a.With[k]; return ok }

func (a Action) Str(k, def string) string {
	if s, ok := a.With[k].(string); ok {
		return s
	}
	return def
}

func (a Action) Num(k string, def float64) float64 {
	switch v := a.With[k].(type) {
	case float64:
		return v
	case int:
		return float64(v)
	}
	return def
}

func (a Action) Bool(k string, def bool) bool {
	if b, ok := a.With[k].(bool); ok {
		return b
	}
	return def
}

func (a Action) Strs(k string) []string {
	raw, ok := a.With[k].([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(raw))
	for _, v := range raw {
		if s, ok := v.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

func (a Action) Flags(k string) map[string]bool {
	m, _ := a.With[k].(map[string]any)
	out := map[string]bool{}
	for k, v := range m {
		if b, ok := v.(bool); ok {
			out[k] = b
		}
	}
	return out
}

// Unknown reports inputs this action was not expecting. A typo in `with:` is
// otherwise invisible: the step runs, quietly does the default thing, and the
// batch looks fine until someone reads the artifacts three days later.
func (a Action) Unknown(known ...string) []string {
	set := map[string]bool{}
	for _, k := range known {
		set[k] = true
	}
	var out []string
	for k := range a.With {
		if !set[k] {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

// Pipeline is three lists, one per unit of work. The key names the unit, and
// that is also the whole answer to "when does the upload happen": ship under
// per_trial runs per trial, ship under per_experiment runs once when the batch
// is done. Position is timing; there is no separate switch.
type Pipeline struct {
	// Concurrency bounds post-trial processing. It is deliberately not the
	// same number as Experiment.Concurrency: that one answers "how many
	// containers fit on this machine", this one answers "how many trials can
	// be scrubbed, packed and shipped at once", and at batch sizes where the
	// data is the problem those are different questions.
	Concurrency int `yaml:"concurrency"`

	PerCase       []Action `yaml:"per_case"`
	PerTrial      []Action `yaml:"per_trial"`
	PerExperiment []Action `yaml:"per_experiment"`
}

// Executor is the action that runs the trial. It must be the first entry in
// per_trial, because nothing can post-process a trial that has not run.
func (p Pipeline) Executor() (Action, error) {
	if len(p.PerTrial) == 0 {
		return Action{}, fmt.Errorf(
			"pipeline.per_trial is empty: it must begin with the action that runs the trial, e.g. `- uses: harbor`")
	}
	return p.PerTrial[0], nil
}

// PostTrial is everything after the executor.
func (p Pipeline) PostTrial() []Action {
	if len(p.PerTrial) == 0 {
		return nil
	}
	return p.PerTrial[1:]
}

type Experiment struct {
	Name         string    `yaml:"name"`
	CaseDefaults CaseRef   `yaml:"case_defaults"`
	Cases        []CaseRef `yaml:"cases"`
	Matrix       Matrix    `yaml:"matrix"`
	Concurrency  int       `yaml:"concurrency"`
	Pipeline     Pipeline  `yaml:"pipeline"`
	MaxAttempts  int       `yaml:"max_attempts"`
}

type File struct {
	Commands   Commands
	LLMSpecs   map[string]LLMSpec
	Experiment Experiment

	// DeclaresCommands records that the submission carried its own kind:
	// Commands document. With a trusted commands file in play that is a
	// refusal, not something to merge or quietly drop.
	DeclaresCommands bool `yaml:"-"`
}

type Duration time.Duration

func (d *Duration) UnmarshalYAML(n *yaml.Node) error {
	if n.Value == "" {
		return nil
	}
	v, err := time.ParseDuration(n.Value)
	if err != nil {
		return fmt.Errorf("bad duration %q: %w", n.Value, err)
	}
	*d = Duration(v)
	return nil
}

func (d Duration) D() time.Duration { return time.Duration(d) }

// LoadCommands reads a commands-only file that belongs to the runner, not to
// the submission. Pointing at one is what separates "who decides what a step
// runs" from "who decides which steps run" -- without it, anyone who can hand
// you a submission can run code on your machine, because a submission may
// define its own commands.
func LoadCommands(path string) (Commands, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return Commands{}, err
	}
	var out Commands
	dec := yaml.NewDecoder(strings.NewReader(string(raw)))
	found := false
	for {
		var node yaml.Node
		if err := dec.Decode(&node); err != nil {
			break
		}
		var probe struct {
			Kind string `yaml:"kind"`
		}
		if err := node.Decode(&probe); err != nil {
			return Commands{}, err
		}
		if probe.Kind != "Commands" {
			return Commands{}, fmt.Errorf("%s: a commands file holds only kind: Commands, found %q", path, probe.Kind)
		}
		if found {
			return Commands{}, fmt.Errorf("%s: more than one kind: Commands", path)
		}
		if err := node.Decode(&out); err != nil {
			return Commands{}, err
		}
		found = true
	}
	if !found {
		return Commands{}, fmt.Errorf("%s: no kind: Commands", path)
	}
	out.Trusted = true
	return out, nil
}

func Load(path string) (*File, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	f := &File{LLMSpecs: map[string]LLMSpec{}}
	dec := yaml.NewDecoder(strings.NewReader(string(raw)))
	seen := false
	for {
		var node yaml.Node
		if err := dec.Decode(&node); err != nil {
			break
		}
		var probe struct {
			Kind string `yaml:"kind"`
		}
		if err := node.Decode(&probe); err != nil {
			return nil, err
		}
		switch probe.Kind {
		case "Commands":
			if err := node.Decode(&f.Commands); err != nil {
				return nil, err
			}
			f.DeclaresCommands = true
		case "LLMSpec":
			var s LLMSpec
			if err := node.Decode(&s); err != nil {
				return nil, fmt.Errorf("kind LLMSpec: %w", err)
			}
			if s.Name == "" {
				return nil, fmt.Errorf("LLMSpec without a name")
			}
			f.LLMSpecs[s.Name] = s
		case "Experiment":
			if seen {
				return nil, fmt.Errorf("more than one kind: Experiment in %s", path)
			}
			if err := node.Decode(&f.Experiment); err != nil {
				return nil, fmt.Errorf("kind Experiment: %w", err)
			}
			seen = true
		case "":
			return nil, fmt.Errorf("document without kind in %s", path)
		default:
			return nil, fmt.Errorf("unknown kind %q", probe.Kind)
		}
	}
	if !seen {
		return nil, fmt.Errorf("no kind: Experiment in %s", path)
	}
	return f, f.validate()
}

func (f *File) validate() error {
	e := &f.Experiment
	if e.Name == "" {
		return fmt.Errorf("experiment has no name")
	}
	if len(e.Cases) == 0 {
		return fmt.Errorf("experiment has no cases")
	}
	if e.Matrix.Trials <= 0 {
		e.Matrix.Trials = 1
	}
	if e.Concurrency <= 0 {
		e.Concurrency = 1
	}
	if e.MaxAttempts <= 0 {
		e.MaxAttempts = 1
	}
	if e.Pipeline.Concurrency <= 0 {
		e.Pipeline.Concurrency = e.Concurrency
	}
	if _, err := e.Pipeline.Executor(); err != nil {
		return err
	}
	units := []struct {
		name string
		list []Action
	}{
		{"per_case", e.Pipeline.PerCase},
		{"per_trial", e.Pipeline.PerTrial},
		{"per_experiment", e.Pipeline.PerExperiment},
	}
	for _, u := range units {
		for i, a := range u.list {
			if a.Uses == "" {
				return fmt.Errorf("pipeline.%s[%d] has no uses:", u.name, i)
			}
			if a.OnFailure != "" && a.OnFailure != "fail" && a.OnFailure != "warn" {
				return fmt.Errorf("pipeline.%s[%d] (%s): on_failure must be fail or warn, got %q",
					u.name, i, a.Label(), a.OnFailure)
			}
		}
	}
	for _, n := range e.Matrix.LLMSpecs {
		if _, ok := f.LLMSpecs[n]; !ok {
			return fmt.Errorf("matrix references unknown llm_spec %q", n)
		}
	}
	for _, a := range e.Matrix.Agents {
		if a.LLMSpec != "" {
			if _, ok := f.LLMSpecs[a.LLMSpec]; !ok {
				return fmt.Errorf("agent %s references unknown llm_spec %q", a.Name, a.LLMSpec)
			}
		}
	}
	return nil
}
