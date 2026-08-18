// Command rollout-man is the MVP CLI.
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

	"github.com/andreasfoo/rollout-man/internal/cas"
	"github.com/andreasfoo/rollout-man/internal/cmdrun"
	"github.com/andreasfoo/rollout-man/internal/resolve"
	"github.com/andreasfoo/rollout-man/internal/spec"
	"github.com/andreasfoo/rollout-man/internal/store"
)

func main() {
	if len(os.Args) < 3 {
		usage()
	}
	group, verb := os.Args[1], os.Args[2]
	args := os.Args[3:]

	switch group + " " + verb {
	case "experiment create", "experiment submit":
		os.Exit(cmdSubmit(args))
	case "experiment results":
		os.Exit(cmdResults(args))
	case "case resolve":
		os.Exit(cmdCaseResolve(args))
	case "worker start":
		os.Exit(cmdWorker(args))
	default:
		usage()
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `rollout-man (MVP)

  rollout-man worker start   [--config FILE] [--work DIR] [--dsn DSN] [--executor docker|local|auto]
  rollout-man experiment create <file.yaml> [--temporal ADDR] [--dsn DSN] [--runner ID] [--dry-run]
  rollout-man experiment results <experiment-id> [--dsn DSN] [--pass-at F]
  rollout-man case resolve <file.yaml> [--work DIR]

The worker must be running: orchestration is Temporal workflows, not an
in-process loop.
`)
	os.Exit(2)
}

func defaultDSN() string {
	if v := os.Getenv("ROLLOUT_MAN_DSN"); v != "" {
		return v
	}
	return "postgres:///rollout_man?host=/tmp&port=5433&user=rollout&sslmode=disable"
}

func defaultWork() string {
	if v := os.Getenv("ROLLOUT_MAN_WORK"); v != "" {
		return v
	}
	return filepath.Join(os.TempDir(), "rollout-man")
}

func logf(format string, a ...any) {
	fmt.Printf("%s  %s\n", time.Now().Format("15:04:05"), fmt.Sprintf(format, a...))
}

func signalCtx() (context.Context, func()) {
	ctx, cancel := context.WithCancel(context.Background())
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, os.Interrupt, syscall.SIGTERM)
	go func() { <-ch; cancel() }()
	return ctx, cancel
}

func cmdResults(args []string) int {
	fs := flag.NewFlagSet("results", flag.ExitOnError)
	dsn := fs.String("dsn", defaultDSN(), "PostgreSQL DSN")
	passAt := fs.Float64("pass-at", -1, "also show the pass rate at this reward threshold")
	if len(args) == 0 {
		usage()
	}
	id := args[0]
	fs.Parse(args[1:])

	ctx := context.Background()
	db, err := store.Open(ctx, *dsn)
	if err != nil {
		fmt.Fprintln(os.Stderr, "postgres:", err)
		return 1
	}
	defer db.Close()
	return printResults(ctx, db, id, *passAt)
}

func printResults(ctx context.Context, db *store.DB, id string, passAt float64) int {
	rows, err := db.Results(ctx, id)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	if len(rows) == 0 {
		fmt.Println("no trials recorded for", id)
		return 0
	}
	fmt.Printf("Experiment %s\n\n", id)
	head := fmt.Sprintf("%-14s %-12s %5s  %7s %7s %7s %7s   %s",
		"Agent", "LLM Spec", "done", "mean", "median", "p25", "p75", "failures")
	if passAt >= 0 {
		head += fmt.Sprintf("   pass@%.2f", passAt)
	}
	fmt.Println(head)
	for _, r := range rows {
		mean, med, p25, p75 := quart(r.Rewards)
		line := fmt.Sprintf("%-14s %-12s %5d  %7.3f %7.3f %7.3f %7.3f   %s",
			r.Agent, dash(r.LLMSpec), r.Completed, mean, med, p25, p75, fails(r.FailCounts))
		if passAt >= 0 {
			pass := 0
			for _, v := range r.Rewards {
				if v >= passAt {
					pass++
				}
			}
			denom := r.Completed + r.FailCounts["AGENT_TIMEOUT"] + r.FailCounts["AGENT_EXIT_NONZERO"] + r.FailCounts["AGENT_CRASH"]
			if denom == 0 {
				denom = 1
			}
			line += fmt.Sprintf("   %d/%d = %.0f%%", pass, denom, 100*float64(pass)/float64(denom))
		}
		fmt.Println(line)
	}
	return 0
}

func cmdCaseResolve(args []string) int {
	fs := flag.NewFlagSet("resolve", flag.ExitOnError)
	work := fs.String("work", defaultWork(), "working directory")
	if len(args) == 0 {
		usage()
	}
	file := args[0]
	fs.Parse(args[1:])

	f, err := spec.Load(file)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	st, err := cas.New(filepath.Join(*work, "objects"))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	runner := cmdrun.New(f.Commands)
	res := &resolve.Resolver{Runner: runner, Store: st, WorkRoot: *work, Log: logf}
	rc := 0
	for _, c := range f.Experiment.Cases {
		ref := c.Merge(f.Experiment.CaseDefaults)
		cv, err := res.Resolve(context.Background(), ref)
		if err != nil {
			fmt.Printf("%-44s INVALID  %v\n", ref.Label(), err)
			rc = 1
			continue
		}
		fmt.Printf("%-44s %s  %s\n", cv.Label, cv.SHA256[:16], cv.State)
	}
	return rc
}

func agentNames(a []spec.AgentRef) string {
	var n []string
	for _, x := range a {
		n = append(n, x.Name)
	}
	return strings.Join(n, ", ")
}

func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func fails(m map[string]int) string {
	if len(m) == 0 {
		return "-"
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var parts []string
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s=%d", k, m[k]))
	}
	return strings.Join(parts, " ")
}

func quart(v []float64) (mean, med, p25, p75 float64) {
	if len(v) == 0 {
		return
	}
	s := append([]float64(nil), v...)
	sort.Float64s(s)
	for _, x := range s {
		mean += x
	}
	mean /= float64(len(s))
	q := func(f float64) float64 {
		i := int(f * float64(len(s)-1))
		return s[i]
	}
	return mean, q(0.5), q(0.25), q(0.75)
}
