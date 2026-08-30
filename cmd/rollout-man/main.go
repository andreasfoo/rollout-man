// Command rollout-man runs an evaluation experiment: orchestrate, execute,
// report, ship.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/andreasfoo/rollout-man/internal/actions"
	"github.com/andreasfoo/rollout-man/internal/casesrc"
	"github.com/andreasfoo/rollout-man/internal/cmdrun"
	"github.com/andreasfoo/rollout-man/internal/cmdwatch"
	"github.com/andreasfoo/rollout-man/internal/config"
	rexec "github.com/andreasfoo/rollout-man/internal/exec"
	"github.com/andreasfoo/rollout-man/internal/fail"
	"github.com/andreasfoo/rollout-man/internal/progress"
	"github.com/andreasfoo/rollout-man/internal/run"
)

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	switch os.Args[1] {
	case "run":
		os.Exit(cmdRun(os.Args[2:]))
	case "status":
		os.Exit(cmdStatus(os.Args[2:]))
	case "ship":
		os.Exit(cmdShip(os.Args[2:]))
	case "actions":
		for _, line := range actions.Describe() {
			fmt.Println(line)
		}
		os.Exit(0)
	case "cases":
		os.Exit(cmdCases(os.Args[2:]))
	case "watch":
		os.Exit(cmdWatch(os.Args[2:]))
	default:
		usage()
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `rollout-man

  run    <file.yaml> [--runs DIR] [--id NAME] [--executor harbor|local|auto]
         [--commands FILE] [--regate]
         orchestrate, execute, record. Re-running the same id resumes: trials
         already in results.jsonl are not repeated.

  status <run-dir> [--pass-at F] [--failures] [--case SUBSTR]
         what happened.

  ship   <run-dir> <file.yaml> [--commands FILE]
         re-run the per_experiment steps against an existing run directory.

  actions
         the built-in pipeline steps and the units each runs on.

  cases  <file.yaml> [--commands FILE]
         resolve every case and print its content hash.

  watch  <dir> <file.yaml> [--interval 15s] [--runs DIR] [--commands FILE]
         [--full]
         poll <dir> for case subdirectories (each with its own task.toml).
         A new one is gated through <file.yaml>'s pipeline.per_case (quality
         audit, admission, ...) automatically; per_experiment (ship) never
         runs. A case that already ran and then changes is only reported --
         re-gate it yourself with the same command once you're ready.
         With --full, a case that passes the gate is also run through the
         file's per_trial pipeline (in the background, bounded by its
         concurrency:) -- one trial per admitted case, recorded in the run
         directory's results.jsonl as if a run had produced it.
`)
	os.Exit(2)
}

func logf(format string, a ...any) {
	fmt.Printf("%s  %s\n", time.Now().Format("15:04:05"), fmt.Sprintf(format, a...))
}

func signalCtx(interruptedMsg string) (context.Context, func()) {
	ctx, cancel := context.WithCancel(context.Background())
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-ch
		fmt.Fprintln(os.Stderr, "\n"+interruptedMsg)
		cancel()
	}()
	return ctx, cancel
}

func runsRoot(v string) string {
	if v != "" {
		return v
	}
	if e := os.Getenv("ROLLOUT_MAN_RUNS"); e != "" {
		return e
	}
	return "runs"
}

// build resolves the commands and the executor. When --commands names a file,
// that file is the only place commands may come from: a submission that also
// declares its own is refused rather than merged or ignored. Merging would let
// a submitter redefine `ship`; ignoring would let them think theirs ran.
func build(f *config.File, executor, commandsFile string) (*cmdrun.Runner, rexec.Executor, error) {
	if commandsFile != "" {
		trusted, err := config.LoadCommands(commandsFile)
		if err != nil {
			return nil, nil, err
		}
		if f.DeclaresCommands {
			return nil, nil, fmt.Errorf(
				"the submission declares its own kind: Commands, but commands are taken from %s -- "+
					"remove the Commands document from the submission", commandsFile)
		}
		f.Commands = trusted
	}
	cmds := cmdrun.New(f.Commands, f.LLMSpecs)
	cmds.Log = logf
	// The first per_trial action names the executor. This lets an experiment
	// select a workflow adapter (such as forge-flow-fuzz) while its admission
	// probes still use the same adapter's oracle/nop delegation. An explicit
	// --executor remains an operator override.
	selected := executor
	if selected == "" || selected == "auto" {
		a, err := f.Experiment.Pipeline.Executor()
		if err != nil {
			return nil, nil, err
		}
		selected = a.Uses
	}
	ex, err := pick(selected, cmds)
	return cmds, ex, err
}

// pick resolves --executor. Anything other than "local" names a configured
// command, because running a case is Harbor's job and reaching it is a command
// like any other external system. "auto" prefers the command named "harbor"
// when the submission declares one.
func pick(name string, cmds *cmdrun.Runner) (rexec.Executor, error) {
	switch name {
	case "local":
		return &rexec.Local{}, nil
	case "", "auto":
		if cmds.Has("harbor") {
			return harbor("harbor", cmds), nil
		}
		return &rexec.Local{}, nil
	}
	if !cmds.Has(name) {
		return nil, fmt.Errorf("--executor %s: no command named %q is configured "+
			"(declare it under `kind: Commands`, or use --executor local)", name, name)
	}
	return harbor(name, cmds), nil
}

func harbor(name string, cmds *cmdrun.Runner) rexec.Executor {
	return &rexec.Harbor{Cmd: name, Run: func(ctx context.Context, n string,
		vars map[string]string, timeout time.Duration) error {
		_, err := cmds.RunOnce(ctx, n, vars, timeout)
		return err
	}}
}

func cmdRun(args []string) int {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	runs := fs.String("runs", "", "directory holding runs (default ./runs)")
	id := fs.String("id", "", "run id (default: experiment name + timestamp)")
	executor := fs.String("executor", "auto", "name of the configured trial command (e.g. harbor) | local | auto")
	commandsFile := fs.String("commands", "", "take commands from this file instead of the submission")
	regate := fs.Bool("regate", false, "re-run per_case even for cases with a cached verdict")
	if len(args) == 0 {
		usage()
	}
	file := args[0]
	fs.Parse(args[1:])

	f, err := config.Load(file)
	if err != nil {
		fmt.Fprintln(os.Stderr, "load:", err)
		return 1
	}
	cmds, ex, err := build(f, *executor, *commandsFile)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	runID := *id
	if runID == "" {
		runID = fmt.Sprintf("%s-%s", f.Experiment.Name, time.Now().UTC().Format("20060102-150405"))
	}
	dir := filepath.Join(runsRoot(*runs), runID)

	ctx, cancel := signalCtx("interrupted; re-run with the same --id to pick up where this stopped")
	defer cancel()

	// scratch lives outside the run directory: the run directory is a
	// deliverable and shipping it should not sweep up temporary clones
	tmp, err := os.MkdirTemp("", "rollout-man-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer os.RemoveAll(tmp)
	r := &run.Runner{
		File: f, Cmds: cmds, Exec: ex, Dir: dir, Log: logf, Regate: *regate,
		Res: &casesrc.Resolver{Cmds: cmds, TempDir: tmp},
	}
	fmt.Printf("\n%s  (%s executor)\n  %s\n\n", f.Experiment.Name, ex.Name(), dir)
	if err := r.Run(ctx); err != nil {
		fmt.Fprintln(os.Stderr, "\n"+err.Error())
		return 1
	}
	fmt.Println()
	report(dir, -1, false, "", false)
	return 0
}

func cmdStatus(args []string) int {
	fs := flag.NewFlagSet("status", flag.ExitOnError)
	passAt := fs.Float64("pass-at", -1, "also show the pass rate at this reward")
	failures := fs.Bool("failures", false, "list the failed trials")
	only := fs.String("case", "", "only this case (substring match)")
	if len(args) == 0 {
		usage()
	}
	dir := args[0]
	fs.Parse(args[1:])
	inFlight := live(dir, *only)
	return report(dir, *passAt, *failures, *only, inFlight)
}

// live prints the run's current state, if it has one. A run that is still
// going has nothing to say in results.jsonl about the trials still in flight,
// which is exactly the half you want while waiting.
// live prints the run's current state and reports whether it is still going.
func live(dir, only string) bool {
	snap, err := progress.Load(dir)
	if err != nil {
		return false
	}
	fmt.Printf("\n%s  %s\n", snap.Experiment, state(snap))
	fmt.Printf("  %d/%d trials done", snap.Done, snap.Total)
	if snap.Dropped > 0 {
		fmt.Printf(" · %d dropped", snap.Dropped)
	}
	if snap.Failed > 0 {
		fmt.Printf(" · %d not measured", snap.Failed)
	}
	fmt.Println()

	names := make([]string, 0, len(snap.Cases))
	for k := range snap.Cases {
		if only == "" || strings.Contains(k, only) {
			names = append(names, k)
		}
	}
	sort.Strings(names)
	if len(names) > 1 || only != "" {
		fmt.Println()
		fmt.Printf("  %-44s %5s %8s %8s %8s\n", "Case", "done", "of", "dropped", "failed")
		for _, n := range names {
			c := snap.Cases[n]
			fmt.Printf("  %-44s %5d %8d %8d %8d\n", trim(n, 44), c.Done, c.Total, c.Dropped, c.Failed)
		}
	}

	var running []progress.Entry
	for _, e := range snap.Running {
		if only == "" || strings.Contains(e.Case, only) {
			running = append(running, e)
		}
	}
	if len(running) > 0 {
		fmt.Printf("\n  in flight\n")
		now := time.Now()
		for _, e := range running {
			fmt.Printf("    %-52s %-12s %s\n", trim(e.TrialID, 52), e.Step,
				now.Sub(e.Since).Round(time.Second))
		}
	}
	fmt.Println()
	return !snap.Finished
}

func state(s progress.Snapshot) string {
	if s.Finished {
		return "(finished)"
	}
	return "(running)"
}

func trim(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return "…" + s[len(s)-n+1:]
}

func cmdShip(args []string) int {
	if len(args) < 2 {
		usage()
	}
	fs := flag.NewFlagSet("ship", flag.ExitOnError)
	commandsFile := fs.String("commands", "", "take commands from this file instead of the submission")
	dir, file := args[0], args[1]
	fs.Parse(args[2:])
	f, err := config.Load(file)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	cmds, ex, err := build(f, "local", *commandsFile)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	r := &run.Runner{File: f, Cmds: cmds, Exec: ex, Dir: dir, Log: logf}
	if err := r.Restore(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	if err := r.Finish(context.Background()); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return 0
}

func cmdCases(args []string) int {
	if len(args) == 0 {
		usage()
	}
	fs := flag.NewFlagSet("cases", flag.ExitOnError)
	commandsFile := fs.String("commands", "", "take commands from this file instead of the submission")
	fs.Parse(args[1:])
	f, err := config.Load(args[0])
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	cmds, _, err := build(f, "local", *commandsFile)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	res := &casesrc.Resolver{Cmds: cmds, TempDir: os.TempDir()}
	rc := 0
	expanded, err := casesrc.Expand(f.Experiment.Cases, f.Experiment.CaseDefaults, f.Near)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	for _, raw := range expanded {
		ref := raw.Merge(f.Experiment.CaseDefaults)
		c, err := res.Resolve(context.Background(), ref)
		if err != nil {
			fmt.Printf("%-46s INVALID  %v\n", ref.Label(), err)
			rc = 1
			continue
		}
		fmt.Printf("%-46s %s  cpus=%d mem=%dMB agent_timeout=%s\n",
			c.Label, c.SHA256[:16], c.Config.Resources.CPUs, c.Config.Resources.MemoryMB,
			c.Config.AgentTimeout)
	}
	return rc
}

func cmdWatch(args []string) int {
	return cmdwatch.Cmd(args, cmdwatch.Deps{
		RunsRoot: runsRoot,
		Build:    build,
		Logf:     logf,
		SignalCtx: func() (context.Context, func()) {
			return signalCtx("interrupted; stopping the watch loop")
		},
	})
}

// --------------------------------------------------------------- report ---

type row struct {
	agent, spec string
	rewards     []float64
	fails       map[fail.Code]int
}

func report(dir string, passAt float64, listFailures bool, only string, inFlight bool) int {
	results, err := run.Load(dir)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	if only != "" {
		kept := results[:0]
		for _, r := range results {
			if strings.Contains(r.Case, only) {
				kept = append(kept, r)
			}
		}
		results = kept
	}
	if len(results) == 0 {
		if inFlight {
			// The live view above already said what is happening; saying
			// "nothing recorded" after it reads as a contradiction.
			fmt.Println("  no trial has finished yet")
		} else {
			fmt.Println("no trials recorded in", dir)
		}
		return 0
	}
	idx := map[string]*row{}
	var order []*row
	for _, r := range run.Sorted(results) {
		k := r.Agent + "|" + r.LLMSpec
		x, ok := idx[k]
		if !ok {
			x = &row{agent: r.Agent, spec: r.LLMSpec, fails: map[fail.Code]int{}}
			idx[k] = x
			order = append(order, x)
		}
		if r.OK() {
			x.rewards = append(x.rewards, *r.Reward)
		} else {
			x.fails[r.Code]++
		}
	}

	fmt.Printf("%s\n\n", dir)
	head := fmt.Sprintf("%-14s %-12s %5s  %7s %7s %7s %7s   %s",
		"Agent", "LLM Spec", "done", "mean", "median", "p25", "p75", "not measured")
	if passAt >= 0 {
		head += fmt.Sprintf("   pass@%.2f", passAt)
	}
	fmt.Println(head)
	for _, x := range order {
		mean, med, p25, p75 := quartiles(x.rewards)
		line := fmt.Sprintf("%-14s %-12s %5d  %7.3f %7.3f %7.3f %7.3f   %s",
			x.agent, dash(x.spec), len(x.rewards), mean, med, p25, p75, failSummary(x.fails))
		if passAt >= 0 {
			n := 0
			for _, v := range x.rewards {
				if v >= passAt {
					n++
				}
			}
			// Only the agent's own failures belong in the denominator.
			// Infrastructure trouble is ours, and counting it would quietly
			// mark every agent down for our bad day.
			denom := len(x.rewards)
			for c, k := range x.fails {
				if c.CountsAgainstAgent() {
					denom += k
				}
			}
			if denom == 0 {
				denom = 1
			}
			line += fmt.Sprintf("   %d/%d = %.0f%%", n, denom, 100*float64(n)/float64(denom))
		}
		fmt.Println(line)
	}

	if listFailures {
		fmt.Println()
		for _, r := range run.Sorted(results) {
			if !r.OK() {
				fmt.Printf("  %-44s %-14s %s\n", r.TrialID, r.Code, firstLine(r.Message))
			}
		}
	}
	return 0
}

func failSummary(m map[fail.Code]int) string {
	if len(m) == 0 {
		return "-"
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, string(k))
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s=%d", k, m[fail.Code(k)]))
	}
	return strings.Join(parts, " ")
}

func quartiles(v []float64) (mean, med, p25, p75 float64) {
	if len(v) == 0 {
		return
	}
	s := append([]float64(nil), v...)
	sort.Float64s(s)
	for _, x := range s {
		mean += x
	}
	mean /= float64(len(s))
	q := func(f float64) float64 { return s[int(f*float64(len(s)-1))] }
	return mean, q(0.5), q(0.25), q(0.75)
}

func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
