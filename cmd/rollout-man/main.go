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

	"github.com/andreasfoo/rollout-man/internal/casesrc"
	"github.com/andreasfoo/rollout-man/internal/cmdrun"
	"github.com/andreasfoo/rollout-man/internal/config"
	rexec "github.com/andreasfoo/rollout-man/internal/exec"
	"github.com/andreasfoo/rollout-man/internal/fail"
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
	case "cases":
		os.Exit(cmdCases(os.Args[2:]))
	default:
		usage()
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `rollout-man

  run    <file.yaml> [--runs DIR] [--id NAME] [--executor harbor|local|auto]
         orchestrate, execute, record. Re-running the same id resumes: trials
         already in results.jsonl are not repeated.

  status <run-dir> [--pass-at F] [--failures]
         what happened.

  ship   <run-dir> <file.yaml>
         hand the run directory to the configured ship command.

  cases  <file.yaml>
         resolve every case and print its content hash.
`)
	os.Exit(2)
}

func logf(format string, a ...any) {
	fmt.Printf("%s  %s\n", time.Now().Format("15:04:05"), fmt.Sprintf(format, a...))
}

func signalCtx() (context.Context, func()) {
	ctx, cancel := context.WithCancel(context.Background())
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-ch
		fmt.Fprintln(os.Stderr, "\ninterrupted; re-run with the same --id to pick up where this stopped")
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

func build(f *config.File, executor string) (*cmdrun.Runner, rexec.Executor, error) {
	cmds := cmdrun.New(f.Commands)
	cmds.Log = logf
	ex, err := pick(executor, cmds)
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
	cmds, ex, err := build(f, *executor)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	runID := *id
	if runID == "" {
		runID = fmt.Sprintf("%s-%s", f.Experiment.Name, time.Now().UTC().Format("20060102-150405"))
	}
	dir := filepath.Join(runsRoot(*runs), runID)

	ctx, cancel := signalCtx()
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
		File: f, Cmds: cmds, Exec: ex, Dir: dir, Log: logf,
		Res: &casesrc.Resolver{Cmds: cmds, TempDir: tmp},
	}
	fmt.Printf("\n%s  (%s executor)\n  %s\n\n", f.Experiment.Name, ex.Name(), dir)
	if err := r.Run(ctx); err != nil {
		fmt.Fprintln(os.Stderr, "\n"+err.Error())
		return 1
	}
	fmt.Println()
	report(dir, -1, false)
	return 0
}

func cmdStatus(args []string) int {
	fs := flag.NewFlagSet("status", flag.ExitOnError)
	passAt := fs.Float64("pass-at", -1, "also show the pass rate at this reward")
	failures := fs.Bool("failures", false, "list the failed trials")
	if len(args) == 0 {
		usage()
	}
	dir := args[0]
	fs.Parse(args[1:])
	return report(dir, *passAt, *failures)
}

func cmdShip(args []string) int {
	if len(args) < 2 {
		usage()
	}
	dir, file := args[0], args[1]
	f, err := config.Load(file)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	cmds, ex, err := build(f, "local")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	r := &run.Runner{File: f, Cmds: cmds, Exec: ex, Dir: dir, Log: logf}
	if err := r.Ship(context.Background()); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return 0
}

func cmdCases(args []string) int {
	if len(args) == 0 {
		usage()
	}
	f, err := config.Load(args[0])
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	cmds, _, err := build(f, "local")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	res := &casesrc.Resolver{Cmds: cmds, TempDir: os.TempDir()}
	rc := 0
	for _, raw := range f.Experiment.Cases {
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

// --------------------------------------------------------------- report ---

type row struct {
	agent, spec string
	rewards     []float64
	fails       map[fail.Code]int
}

func report(dir string, passAt float64, listFailures bool) int {
	results, err := run.Load(dir)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	if len(results) == 0 {
		fmt.Println("no trials recorded in", dir)
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
