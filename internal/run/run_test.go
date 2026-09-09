package run

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/andreasfoo/rollout-man/internal/casesrc"
	"github.com/andreasfoo/rollout-man/internal/cmdrun"
	"github.com/andreasfoo/rollout-man/internal/config"
	"github.com/andreasfoo/rollout-man/internal/fail"
)

// newRunner builds the smallest Runner that can run execTrial: a configured
// command (the real gates are commands) and a scratch run dir. No Exec, no
// attempts -- execTrial is called directly with a hand-built result.
func newRunner(t *testing.T) (*Runner, string) {
	t.Helper()
	dir := t.TempDir()
	cmds := config.Commands{Cmds: map[string]config.Command{
		"always_fail": {Run: []string{"false"}},
		// Proves later steps do not run: it leaves a file behind if it does.
		"after": {Run: []string{"touch", filepath.Join(dir, "after-skip-ran")}},
	}}
	return &Runner{
		File: &config.File{Commands: cmds, Experiment: config.Experiment{Name: "test"}},
		Cmds: cmdrun.New(cmds, nil),
		Dir:  dir,
		Log:  func(string, ...any) {},
	}, dir
}

func measuredTrial(dir string) (*trial, *Result) {
	rw := 1.0
	tr := &trial{ID: "t-1", Case: &casesrc.Case{Label: "c1", Dir: dir, SHA256: "x"}}
	return tr, &Result{TrialID: "t-1", Reward: &rw}
}

// TestExecTrialSkipPreservesReward: a measured trial whose per_trial gate has
// on_failure: skip keeps its reward, is marked dropped, records which gate
// skipped, and runs no later step.
func TestExecTrialSkipPreservesReward(t *testing.T) {
	r, dir := newRunner(t)
	r.File.Experiment.Pipeline.PerTrial = []config.Action{
		{Uses: "rollout"}, // executor slot; PostTrial() drops it
		{Uses: "always_fail", OnFailure: "skip"},
		{Uses: "after"},
	}
	tr, res := measuredTrial(dir)
	r.execTrial(context.Background(), tr, res)
	if res.Reward == nil || *res.Reward != 1.0 {
		t.Fatalf("skip lost the measured reward: %+v", res)
	}
	if res.Code != "" || res.Category != "" || res.Message != "" {
		t.Fatalf("skip became a failure row: %+v", res)
	}
	if !res.Dropped || !r.isDropped("t-1") {
		t.Fatalf("skip not marked dropped: %+v", res)
	}
	if by, _ := res.Notes["skipped_by"].(string); by != "always_fail" {
		t.Fatalf("skipped_by=%q, want always_fail", by)
	}
	if _, err := os.Stat(filepath.Join(dir, "after-skip-ran")); err == nil {
		t.Fatal("a step after the skipping gate ran")
	}
}

// TestExecTrialHardFailureStillFails: a post step without on_failure on a
// measured trial still converts the row to a failure -- the pre-existing
// behavior a skip must not paper over.
func TestExecTrialHardFailureStillFails(t *testing.T) {
	r, dir := newRunner(t)
	r.File.Experiment.Pipeline.PerTrial = []config.Action{
		{Uses: "rollout"},
		{Uses: "always_fail"},
	}
	tr, res := measuredTrial(dir)
	r.execTrial(context.Background(), tr, res)
	if res.Reward != nil {
		t.Fatalf("hard failure kept the reward: %+v", res)
	}
	if res.Code != fail.Host {
		t.Fatalf("code=%v, want %v", res.Code, fail.Host)
	}
}

// TestSkipRowSurvivesResume: a skipped row round-trips through results.jsonl
// as done-and-dropped, so a resumed run neither re-rolls the trial nor
// publishes it -- the state a ship-only resume (task #77) builds on.
func TestSkipRowSurvivesResume(t *testing.T) {
	r, dir := newRunner(t)
	r.File.Experiment.Pipeline.PerTrial = []config.Action{
		{Uses: "rollout"},
		{Uses: "always_fail", OnFailure: "skip"},
	}
	tr, res := measuredTrial(dir)
	r.execTrial(context.Background(), tr, res)
	r.append(*res)

	r2 := &Runner{File: r.File, Dir: dir, Log: func(string, ...any) {}}
	if err := r2.loadDone(); err != nil {
		t.Fatal(err)
	}
	if !r2.done["t-1"] {
		t.Fatal("skipped trial not marked done on resume")
	}
	if !r2.isDropped("t-1") {
		t.Fatal("skipped trial not restored as dropped on resume")
	}
}
