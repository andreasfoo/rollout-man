package actions

import (
	"context"
	"testing"

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
