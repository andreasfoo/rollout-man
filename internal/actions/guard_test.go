package actions

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/andreasfoo/rollout-man/internal/cmdrun"
	"github.com/andreasfoo/rollout-man/internal/config"
)

func TestGuardStrictMax(t *testing.T) {
	g := guardAction{}
	for _, tc := range []struct {
		name     string
		reward   float64
		wantDrop bool
	}{
		{"below threshold accepted", 0.59, false},
		{"threshold rejected", 0.60, true},
		{"above threshold rejected", 1.00, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := tc.reward
			c := &Ctx{Scope: PerTrial, Trial: &Trial{Reward: &r}}
			a := config.Action{Uses: "guard", OnViolation: "drop", With: map[string]any{
				"max_reward_exclusive": 0.6,
			}}
			if err := g.Run(context.Background(), c, a); err != nil {
				t.Fatal(err)
			}
			if c.Drop != tc.wantDrop {
				t.Fatalf("drop=%v, want %v", c.Drop, tc.wantDrop)
			}
		})
	}
}

type countingAction struct{ n *int }

func (countingAction) Name() string                                     { return "counting-test" }
func (countingAction) Scopes() []Scope                                  { return []Scope{PerTrial} }
func (countingAction) Validate(config.Action) error                     { return nil }
func (a countingAction) Run(context.Context, *Ctx, config.Action) error { *a.n++; return nil }

func TestRunListSkipsAfterDrop(t *testing.T) {
	n := 0
	register(countingAction{n: &n})
	c := &Ctx{Scope: PerTrial, Drop: true}
	if err := RunList(context.Background(), c, []config.Action{{Uses: "counting-test"}}, nil); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("post-drop action ran %d times", n)
	}
}

type failingAction struct{}

func (failingAction) Name() string                 { return "failing-test" }
func (failingAction) Scopes() []Scope              { return []Scope{PerTrial} }
func (failingAction) Validate(config.Action) error { return nil }
func (failingAction) Run(context.Context, *Ctx, config.Action) error {
	return errors.New("gate rejected")
}

// TestRunListSkipNotesLabel: a failing on_failure: skip step returns the
// sentinel, records which step skipped, and runs nothing after it.
func TestRunListSkipNotesLabel(t *testing.T) {
	register(failingAction{})
	n := 0
	register(countingAction{n: &n})
	c := &Ctx{Scope: PerTrial}
	err := RunList(context.Background(), c, []config.Action{
		{Uses: "failing-test", OnFailure: "skip"},
		{Uses: "counting-test"},
	}, nil)
	if !errors.Is(err, ErrSkipCase) {
		t.Fatalf("err=%v, want ErrSkipCase", err)
	}
	if by, _ := c.Notes["skipped_by"].(string); by != "failing-test" {
		t.Fatalf("skipped_by=%q, want failing-test", by)
	}
	if n != 0 {
		t.Fatal("a step after the skipping gate ran")
	}
}

// TestShipForwardsWithAsEnv: a ship step's with: entries reach the shipping
// command as env vars (HF_REVISION, HF_TASK_PATH from the submission -- the
// settings an adapter must not bake in), with {{steps.*.outputs.*}} expanded.
// The command is a real one (printf its env), because the env the runner
// assembles is the thing under test.
func TestShipForwardsWithAsEnv(t *testing.T) {
	dir := t.TempDir()
	envfile := filepath.Join(dir, "env.txt")
	cmds := config.Commands{Cmds: map[string]config.Command{
		"capturing": {Run: []string{"sh", "-c", "/usr/bin/env > " + envfile}},
	}}
	c := &Ctx{Scope: PerTrial, RunDir: dir,
		Cmds: cmdrun.New(cmds, nil), Log: func(string, ...any) {},
		StepOutputs: map[string]map[string]string{
			"materialize": {"materialized_dir": dir},
		}}
	a := config.Action{Uses: "ship", With: map[string]any{
		"using":        "capturing",
		"dest":         "tinglydev/cyber-xianjin",
		"path":         "{{steps.materialize.outputs.materialized_dir}}",
		"hf_revision":  "week3",
		"hf_task_path": "task/week3",
	}}
	if err := (shipAction{}).Run(context.Background(), c, a); err != nil {
		t.Fatal(err)
	}
	env := map[string]string{}
	for _, line := range strings.Split(string(mustRead(t, envfile)), "\n") {
		if k, v, ok := strings.Cut(line, "="); ok {
			env[k] = v
		}
	}
	if env["HF_REVISION"] != "week3" || env["HF_TASK_PATH"] != "task/week3" {
		t.Fatalf("with: not forwarded as env: HF_REVISION=%q HF_TASK_PATH=%q",
			env["HF_REVISION"], env["HF_TASK_PATH"])
	}
	if env["LOCAL_PATH"] != dir {
		t.Fatalf("path expansion broken: LOCAL_PATH=%q want %q", env["LOCAL_PATH"], dir)
	}
	if env["KEY"] != "tinglydev/cyber-xianjin" {
		t.Fatalf("dest not forwarded: KEY=%q", env["KEY"])
	}
}

func mustRead(t *testing.T, p string) []byte {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// TestPipelineWithIsBaseLayer: pipeline.with entries reach every step's
// command as env vars, and a step's own with: overrides the same key -- the
// campaign-wide constants (hf_revision, hf_task_path) are declared once at
// pipeline level, not repeated per step.
func TestPipelineWithIsBaseLayer(t *testing.T) {
	dir := t.TempDir()
	envfile := filepath.Join(dir, "env.txt")
	cmds := config.Commands{Cmds: map[string]config.Command{
		"capturing": {Run: []string{"sh", "-c", "/usr/bin/env > " + envfile}},
	}}
	base := map[string]any{"hf_revision": "week3", "hf_task_path": "task/week3"}
	t.Run("ship inherits base", func(t *testing.T) {
		os.Remove(envfile)
		c := &Ctx{Scope: PerTrial, RunDir: dir, BaseWith: base,
			Trial: &Trial{ID: "t-1"},
			Cmds: cmdrun.New(cmds, nil), Log: func(string, ...any) {}}
		a := config.Action{Uses: "ship", With: map[string]any{
			"using": "capturing", "dest": "tinglydev/cyber-xianjin",
		}}
		if err := (shipAction{}).Run(context.Background(), c, a); err != nil {
			t.Fatal(err)
		}
		env := envMap(t, envfile)
		if env["HF_REVISION"] != "week3" || env["HF_TASK_PATH"] != "task/week3" {
			t.Fatalf("base with: not inherited: %v %v", env["HF_REVISION"], env["HF_TASK_PATH"])
		}
	})
	t.Run("step overrides base", func(t *testing.T) {
		os.Remove(envfile)
		c := &Ctx{Scope: PerTrial, RunDir: dir, BaseWith: base,
			Trial: &Trial{ID: "t-1"},
			Cmds: cmdrun.New(cmds, nil), Log: func(string, ...any) {}}
		a := config.Action{Uses: "ship", With: map[string]any{
			"using": "capturing", "hf_revision": "week4",
		}}
		if err := (shipAction{}).Run(context.Background(), c, a); err != nil {
			t.Fatal(err)
		}
		env := envMap(t, envfile)
		if env["HF_REVISION"] != "week4" {
			t.Fatalf("step with: did not override base: %v", env["HF_REVISION"])
		}
		if env["HF_TASK_PATH"] != "task/week3" {
			t.Fatalf("unrelated base entry lost: %v", env["HF_TASK_PATH"])
		}
	})
	t.Run("command step inherits base", func(t *testing.T) {
		os.Remove(envfile)
		c := &Ctx{Scope: PerTrial, RunDir: dir, BaseWith: base, Trial: &Trial{ID: "t"},
			Cmds: cmdrun.New(cmds, nil), Log: func(string, ...any) {}}
		if err := (command{cmd: "capturing"}).Run(context.Background(),
			c, config.Action{Uses: "capturing"}); err != nil {
			t.Fatal(err)
		}
		env := envMap(t, envfile)
		if env["HF_REVISION"] != "week3" {
			t.Fatalf("command step did not inherit base: %v", env["HF_REVISION"])
		}
	})
}

func envMap(t *testing.T, p string) map[string]string {
	t.Helper()
	m := map[string]string{}
	for _, line := range strings.Split(string(mustRead(t, p)), "\n") {
		if k, v, ok := strings.Cut(line, "="); ok {
			m[k] = v
		}
	}
	return m
}
