// Package config parses the submission file: kind: Commands | LLMSpec |
// Experiment, one file, several YAML documents.
package config

import (
	"fmt"
	"os"
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
	Source string `yaml:"source"` // git | local
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
}

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

type Admission struct {
	Require   string  `yaml:"require"` // admitted (default) | any
	OracleMin float64 `yaml:"oracle_min_reward"`
	NopMax    float64 `yaml:"nop_max_reward"`
	Trials    int     `yaml:"trials"`
}

type Redact struct {
	Keys  string          `yaml:"keys"` // must be "required"
	IPs   map[string]bool `yaml:"ips"`  // traj: true, logs: false
	Extra []string        `yaml:"extra_patterns"`
}

type Ship struct {
	Using string `yaml:"using"` // a command name
	Dest  string `yaml:"dest"`
}

// Pipeline keeps the three scopes from the design, with one setting each. The
// key still names the unit: per_case runs once per case, per_trial once per
// trial, per_experiment once at the end.
type Pipeline struct {
	PerCase struct {
		Admission *Admission `yaml:"admission"`
	} `yaml:"per_case"`
	PerTrial struct {
		Redact *Redact `yaml:"redact"`
	} `yaml:"per_trial"`
	PerExperiment struct {
		Ship *Ship `yaml:"ship"`
	} `yaml:"per_experiment"`
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
	if a := e.Pipeline.PerCase.Admission; a != nil {
		if a.Trials <= 0 {
			a.Trials = 2
		}
		if a.Require == "" {
			a.Require = "admitted"
		}
	}
	if r := e.Pipeline.PerTrial.Redact; r != nil && r.Keys != "" && r.Keys != "required" {
		return fmt.Errorf(`pipeline.per_trial.redact.keys must be "required", got %q`, r.Keys)
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
