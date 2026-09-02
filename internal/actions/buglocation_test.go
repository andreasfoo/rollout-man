package actions

import (
	"strings"
	"testing"

	"github.com/andreasfoo/rollout-man/internal/config"
)

func TestBuglocationRequiresUsing(t *testing.T) {
	a := buglocationAction{}
	if err := a.Validate(config.Action{Uses: "buglocation"}); err == nil ||
		!strings.Contains(err.Error(), "using: is required") {
		t.Fatalf("missing using: want error, got %v", err)
	}
	if err := a.Validate(config.Action{
		Uses: "buglocation",
		With: map[string]any{"using": "buglocate_kimi"},
	}); err != nil {
		t.Fatalf("using set: want nil, got %v", err)
	}
	if err := a.Validate(config.Action{
		Uses: "buglocation",
		With: map[string]any{"using": "x", "bogus": 1},
	}); err == nil {
		t.Fatal("unknown input: want error")
	}
	if got := a.Name(); got != "buglocation" {
		t.Fatalf("Name() = %q", got)
	}
	if got := a.Scopes(); len(got) != 1 || got[0] != PerCase {
		t.Fatalf("Scopes() = %v", got)
	}
}
