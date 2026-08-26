// Package cmdwatch polls a directory of cases and gates each new one through
// an experiment's pipeline.per_case as soon as it appears. An existing case
// that changes is only reported, never re-gated on its own -- editing a case
// is a decision a person makes, not something watch should act on.
package cmdwatch

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"time"

	"github.com/andreasfoo/rollout-man/internal/casesrc"
	"github.com/andreasfoo/rollout-man/internal/cmdrun"
	"github.com/andreasfoo/rollout-man/internal/config"
	rexec "github.com/andreasfoo/rollout-man/internal/exec"
	"github.com/andreasfoo/rollout-man/internal/run"
)

// Deps is what main.go already knows how to build for `run`; watch reuses it
// rather than re-deriving its own copy of --commands/--executor resolution.
type Deps struct {
	RunsRoot  func(string) string
	Build     func(f *config.File, executor, commandsFile string) (*cmdrun.Runner, rexec.Executor, error)
	Logf      func(format string, a ...any)
	SignalCtx func() (context.Context, func())
}

// state is the on-disk record of every case name watch has ever admitted a
// verdict for, and the content hash it saw last. It is deliberately not the
// same thing as the run's gate cache: the gate cache is keyed by content
// hash alone, so it cannot tell "a name never seen before" from "a name
// whose content changed to something never gated before" -- only this name
// keyed map can.
type state struct {
	Cases map[string]string `json:"cases"` // dir name -> last-seen content hash
}

func loadState(path string) *state {
	s := &state{Cases: map[string]string{}}
	b, err := os.ReadFile(path)
	if err != nil {
		return s
	}
	_ = json.Unmarshal(b, s)
	if s.Cases == nil {
		s.Cases = map[string]string{}
	}
	return s
}

func (s *state) save(path string) error {
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o644)
}

var sanitizeRe = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

func sanitize(s string) string {
	s = sanitizeRe.ReplaceAllString(s, "-")
	if s == "" {
		return "dir"
	}
	return s
}

// Cmd is the `rollout-man watch <dir> <file.yaml>` entry point.
func Cmd(args []string, d Deps) int {
	fs := flag.NewFlagSet("watch", flag.ExitOnError)
	runs := fs.String("runs", "", "directory holding runs (default ./runs)")
	commandsFile := fs.String("commands", "", "take commands from this file instead of the submission")
	interval := fs.Duration("interval", 15*time.Second, "how often to re-scan the watched directory")
	if len(args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: rollout-man watch <dir> <file.yaml> [--interval 15s] [--runs DIR] [--commands FILE]")
		return 2
	}
	dir, file := args[0], args[1]
	fs.Parse(args[2:])

	absDir, err := filepath.Abs(dir)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}

	f, err := config.Load(file)
	if err != nil {
		fmt.Fprintln(os.Stderr, "load:", err)
		return 1
	}
	cmds, ex, err := d.Build(f, "auto", *commandsFile)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}

	runDir := filepath.Join(d.RunsRoot(*runs), "watch-"+sanitize(filepath.Base(absDir)))
	if err := os.MkdirAll(runDir, 0o755); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	tmp, err := os.MkdirTemp("", "rollout-man-watch-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer os.RemoveAll(tmp)

	r := &run.Runner{
		File: f, Cmds: cmds, Exec: ex, Dir: runDir, Log: d.Logf,
		Res: &casesrc.Resolver{Cmds: cmds, TempDir: tmp},
	}
	if err := r.Validate(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}

	statePath := filepath.Join(runDir, "watch-state.json")
	st := loadState(statePath)

	ctx, cancel := d.SignalCtx()
	defer cancel()

	d.Logf("watching %s (poll every %s, gate: %s)", absDir, *interval, file)
	poll(ctx, r, absDir, st, statePath, d.Logf)
	t := time.NewTicker(*interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return 0
		case <-t.C:
			poll(ctx, r, absDir, st, statePath, d.Logf)
		}
	}
}

// poll does one scan of dir: gate every not-yet-seen case, and notify (but do
// not act) on every already-seen case whose content changed.
func poll(ctx context.Context, r *run.Runner, dir string, st *state, statePath string, logf func(string, ...any)) {
	defaults := r.File.Experiment.CaseDefaults
	entries, err := os.ReadDir(dir)
	if err != nil {
		logf("watch: read %s: %v", dir, err)
		return
	}

	seen := map[string]bool{}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if _, err := os.Stat(filepath.Join(dir, e.Name(), "task.toml")); err != nil {
			continue // not a case directory
		}
		names = append(names, e.Name())
	}
	sort.Strings(names)

	dirty := false
	for _, name := range names {
		if ctx.Err() != nil {
			return
		}
		seen[name] = true
		caseDir := filepath.Join(dir, name)
		hash, err := casesrc.HashDir(caseDir)
		if err != nil {
			logf("watch: hash %s: %v", name, err)
			continue
		}

		prev, known := st.Cases[name]
		switch {
		case !known:
			logf("watch: new case %s -> gating", name)
			ref := config.CaseRef{Path: caseDir}.Merge(defaults)
			_, admitted, err := r.GateOne(ctx, ref)
			if err != nil {
				logf("watch: case %s: gate failed, will retry next poll: %v", name, err)
				continue // do not record state -- retry next tick
			}
			logf("watch: case %s: %s", name, map[bool]string{true: "admitted", false: "rejected"}[admitted])
			st.Cases[name] = hash
			dirty = true

		case prev != hash:
			logf("watch: case %s changed since last seen; re-run manually to re-gate it", name)
			st.Cases[name] = hash
			dirty = true
		}
	}

	for name := range st.Cases {
		if !seen[name] {
			logf("watch: case %s removed", name)
			delete(st.Cases, name)
			dirty = true
		}
	}

	if dirty {
		if err := st.save(statePath); err != nil {
			logf("watch: save state: %v", err)
		}
	}
}
