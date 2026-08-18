// Package spec parses the multi-document submission file:
// kind: Commands | LLMSpec | Experiment.
package spec

import (
	"fmt"
	"os"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

type Command struct {
	Run    []string `yaml:"run"`
	Script string   `yaml:"script"`
}

func (c Command) Empty() bool { return len(c.Run) == 0 && strings.TrimSpace(c.Script) == "" }

type Commands struct {
	Timeout     Duration
	MaxAttempts int
	Cmds        map[string]Command
}

// UnmarshalYAML walks the mapping itself: every mapping-valued key is a
// command, the few scalar keys are settings.
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
		default:
			if val.Kind != yaml.MappingNode {
				return fmt.Errorf("command %q must be a mapping with run: or script:", key)
			}
			var cmd Command
			if err := val.Decode(&cmd); err != nil {
				return fmt.Errorf("command %q: %w", key, err)
			}
			if cmd.Empty() {
				return fmt.Errorf("command %q has neither run nor script", key)
			}
			c.Cmds[key] = cmd
		}
	}
	return nil
}

type LLMSpec struct {
	Name          string         `yaml:"name"`
	Provider      string         `yaml:"provider"`
	BaseURL       string         `yaml:"base_url"`
	Model         string         `yaml:"model"`
	APIKeyEnv     string         `yaml:"api_key_env"`
	APIKeyCmd     []string       `yaml:"api_key_cmd"`
	MaxConcurrent int            `yaml:"max_concurrent"`
	Parameters    map[string]any `yaml:"parameters"`
}

type CaseRef struct {
	Source string `yaml:"source"` // git | object | local
	Repo   string `yaml:"repo"`
	Ref    string `yaml:"ref"`
	Path   string `yaml:"path"`
	Key    string `yaml:"key"`
	SHA256 string `yaml:"sha256"`
	Fetch  string `yaml:"fetch"`
}

// Merge fills empty fields from defaults.
func (c CaseRef) Merge(d CaseRef) CaseRef {
	pick := func(a, b string) string {
		if a != "" {
			return a
		}
		return b
	}
	return CaseRef{
		Source: pick(c.Source, d.Source),
		Repo:   pick(c.Repo, d.Repo),
		Ref:    pick(c.Ref, d.Ref),
		Path:   pick(c.Path, d.Path),
		Key:    pick(c.Key, d.Key),
		SHA256: pick(c.SHA256, d.SHA256),
		Fetch:  pick(c.Fetch, d.Fetch),
	}
}

// Label is the human-readable identifier used for grouping in results.
func (c CaseRef) Label() string {
	switch {
	case c.Path != "":
		return c.Path
	case c.Key != "":
		return c.Key
	default:
		return c.SHA256
	}
}

type AgentRef struct {
	Name       string         `yaml:"name"`
	LLMSpec    string         `yaml:"llm_spec"`
	Parameters map[string]any `yaml:"parameters"`
}

// UnmarshalYAML accepts both "- name: x" and the shorthand "- x".
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

type Criteria struct {
	Oracle struct {
		MinReward float64 `yaml:"min_reward"`
	} `yaml:"oracle"`
	Nop struct {
		MaxReward float64 `yaml:"max_reward"`
	} `yaml:"nop"`
	Trials int `yaml:"trials"`
}

type Step struct {
	Step string `yaml:"step"`

	// resolve
	OnUnchanged string `yaml:"on_unchanged"`

	// admission
	Require   string    `yaml:"require"`
	AutoAdmit bool      `yaml:"auto_admit"`
	Criteria  *Criteria `yaml:"criteria"`

	// redact
	Keys          string          `yaml:"keys"`
	IPs           map[string]bool `yaml:"ips"`
	ExtraPatterns []string        `yaml:"extra_patterns"`

	// bundle
	Format  string   `yaml:"format"`
	Include []string `yaml:"include"`

	// stage
	MaxPending string `yaml:"max_pending"`
	OnCancel   string `yaml:"on_cancel"`

	// upload
	Using   string   `yaml:"using"`
	Dest    string   `yaml:"dest"`
	Objects []string `yaml:"objects"`

	// report / deploy
	Formats   []string `yaml:"formats"`
	Run       []string `yaml:"run"`
	OnFailure string   `yaml:"on_failure"`
}

type Pipeline struct {
	PerCase       []Step `yaml:"per_case"`
	PerTrial      []Step `yaml:"per_trial"`
	PerExperiment []Step `yaml:"per_experiment"`
}

func (p Pipeline) find(steps []Step, name string) *Step {
	for i := range steps {
		if steps[i].Step == name {
			return &steps[i]
		}
	}
	return nil
}

func (p Pipeline) PerCaseStep(n string) *Step       { return p.find(p.PerCase, n) }
func (p Pipeline) PerTrialStep(n string) *Step      { return p.find(p.PerTrial, n) }
func (p Pipeline) PerExperimentStep(n string) *Step { return p.find(p.PerExperiment, n) }

type RetryPolicy struct {
	MaxTotalAttempts int      `yaml:"max_total_attempts"`
	RetryOn          []string `yaml:"retry_on"`
}

type Experiment struct {
	Name         string      `yaml:"name"`
	CaseDefaults CaseRef     `yaml:"case_defaults"`
	Cases        []CaseRef   `yaml:"cases"`
	Matrix       Matrix      `yaml:"matrix"`
	Concurrency  int         `yaml:"concurrency"`
	Priority     string      `yaml:"priority"`
	QueueTimeout Duration    `yaml:"queue_timeout"`
	Pipeline     Pipeline    `yaml:"pipeline"`
	Retry        RetryPolicy `yaml:"retry_policy"`
}

type File struct {
	Commands   Commands
	LLMSpecs   map[string]LLMSpec
	Experiment Experiment
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

func Load(path string) (*File, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	f := &File{LLMSpecs: map[string]LLMSpec{}}
	dec := yaml.NewDecoder(strings.NewReader(string(raw)))
	seenExperiment := false
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
			var c Commands
			if err := node.Decode(&c); err != nil {
				return nil, fmt.Errorf("kind Commands: %w", err)
			}
			f.Commands = c
		case "LLMSpec":
			var s LLMSpec
			if err := node.Decode(&s); err != nil {
				return nil, fmt.Errorf("kind LLMSpec: %w", err)
			}
			if s.Name == "" {
				return nil, fmt.Errorf("LLMSpec without name")
			}
			f.LLMSpecs[s.Name] = s
		case "Experiment":
			if seenExperiment {
				return nil, fmt.Errorf("more than one kind: Experiment in %s", path)
			}
			var e Experiment
			if err := node.Decode(&e); err != nil {
				return nil, fmt.Errorf("kind Experiment: %w", err)
			}
			f.Experiment = e
			seenExperiment = true
		case "":
			return nil, fmt.Errorf("document without kind in %s", path)
		default:
			return nil, fmt.Errorf("unknown kind %q", probe.Kind)
		}
	}
	if !seenExperiment {
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
	if e.Retry.MaxTotalAttempts <= 0 {
		e.Retry.MaxTotalAttempts = 1
	}
	for _, a := range e.Matrix.Agents {
		if a.LLMSpec != "" {
			if _, ok := f.LLMSpecs[a.LLMSpec]; !ok {
				return fmt.Errorf("agent %s references unknown llm_spec %q", a.Name, a.LLMSpec)
			}
		}
	}
	for _, n := range e.Matrix.LLMSpecs {
		if _, ok := f.LLMSpecs[n]; !ok {
			return fmt.Errorf("matrix references unknown llm_spec %q", n)
		}
	}
	if s := e.Pipeline.PerTrialStep("redact"); s != nil && s.Keys != "" && s.Keys != "required" {
		return fmt.Errorf("pipeline.per_trial.redact.keys must be \"required\", got %q", s.Keys)
	}
	return nil
}
