package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const emptyCasesExperiment = `
---
kind: Experiment
name: watch-empty-cases
matrix:
  trials: 1
pipeline:
  per_trial:
    - uses: local
`

func writeExperiment(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "experiment.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadForWatchAllowsEmptyCases(t *testing.T) {
	path := writeExperiment(t, emptyCasesExperiment)

	if _, err := Load(path); err == nil || !strings.Contains(err.Error(), "experiment has no cases") {
		t.Fatalf("Load() error = %v, want empty-cases validation error", err)
	}
	if _, err := LoadForWatch(path); err != nil {
		t.Fatalf("LoadForWatch() error = %v, want nil", err)
	}
}

func TestLoadForWatchKeepsOtherValidation(t *testing.T) {
	path := writeExperiment(t, strings.Replace(emptyCasesExperiment, "name: watch-empty-cases\n", "", 1))

	if _, err := LoadForWatch(path); err == nil || !strings.Contains(err.Error(), "experiment has no name") {
		t.Fatalf("LoadForWatch() error = %v, want name validation error", err)
	}
}

func TestLoadForWatchLoadsBatch4Config(t *testing.T) {
	path := filepath.Join("..", "..", "experiments", "tc-batch4-watch.yaml")
	f, err := LoadForWatch(path)
	if err != nil {
		t.Fatalf("LoadForWatch(%q) error = %v", path, err)
	}
	if len(f.Experiment.Cases) != 0 {
		t.Fatalf("Batch 4 static cases = %d, want 0", len(f.Experiment.Cases))
	}
	if got := f.Experiment.Pipeline.With["hf_revision"]; got != "week4" {
		t.Fatalf("hf_revision = %q, want week4", got)
	}
	if got := f.Experiment.Pipeline.With["hf_task_path"]; got != "task/week4" {
		t.Fatalf("hf_task_path = %q, want task/week4", got)
	}
}
