package actions

import (
	"testing"

	"github.com/andreasfoo/rollout-man/internal/config"
)

func TestRolloutRequiresUsing(t *testing.T) {
	r := rolloutAction{}
	a := config.Action{Uses: "rollout"}
	if err := r.Validate(a); err == nil {
		t.Fatal("expected error for missing using:")
	}
	a.With = map[string]any{"using": "kimi3_rollout"}
	if err := r.Validate(a); err != nil {
		t.Fatalf("with using: set: %v", err)
	}
	a.With["bogus"] = 1
	if err := r.Validate(a); err == nil {
		t.Fatal("expected error for unknown input")
	}
}
