package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/worker"

	"github.com/andreasfoo/rollout-man/internal/activities"
	"github.com/andreasfoo/rollout-man/internal/cas"
	"github.com/andreasfoo/rollout-man/internal/cmdrun"
	rexec "github.com/andreasfoo/rollout-man/internal/exec"
	"github.com/andreasfoo/rollout-man/internal/resolve"
	"github.com/andreasfoo/rollout-man/internal/spec"
	"github.com/andreasfoo/rollout-man/internal/store"
	"github.com/andreasfoo/rollout-man/internal/temporalx"
	"github.com/andreasfoo/rollout-man/internal/workflows"
)

const namespace = "rollout-man"

func temporalAddr(flagVal string) string {
	if flagVal != "" {
		return flagVal
	}
	if v := os.Getenv("ROLLOUT_MAN_TEMPORAL"); v != "" {
		return v
	}
	return "localhost:7233"
}

func dial(addr string) (client.Client, error) {
	return client.Dial(client.Options{HostPort: addr, Namespace: namespace})
}

func buildDeps(ctx context.Context, f *spec.File, work, dsn, executor, runnerID string) (*activities.Deps, error) {
	casStore, err := cas.New(filepath.Join(work, "objects"))
	if err != nil {
		return nil, err
	}
	runner := cmdrun.New(f.Commands)
	runner.Log = logf
	ex, err := rexec.Pick(executor)
	if err != nil {
		return nil, err
	}
	db, err := store.Open(ctx, dsn)
	if err != nil {
		return nil, fmt.Errorf("postgres: %w", err)
	}
	return &activities.Deps{
		Runner:   runner,
		CAS:      casStore,
		Resolver: &resolve.Resolver{Runner: runner, Store: casStore, WorkRoot: work, Log: logf},
		Exec:     ex,
		Store:    db,
		WorkRoot: work,
		RunnerID: runnerID,
	}, nil
}

// cmdWorker runs both workers in one process. Splitting them across machines is
// a deployment choice, not a code change: the queues already separate them.
func cmdWorker(args []string) int {
	fs := flag.NewFlagSet("worker", flag.ExitOnError)
	cfg := fs.String("config", "", "file holding kind: Commands (usually deploy config)")
	work := fs.String("work", defaultWork(), "working directory")
	dsn := fs.String("dsn", defaultDSN(), "PostgreSQL DSN")
	executor := fs.String("executor", "auto", "docker | local | auto")
	runnerID := fs.String("runner", "runner-01", "runner id")
	addr := fs.String("temporal", "", "Temporal frontend address")
	fs.Parse(args)

	f := &spec.File{LLMSpecs: map[string]spec.LLMSpec{}}
	if *cfg != "" {
		loaded, err := spec.LoadCommandsOnly(*cfg)
		if err != nil {
			fmt.Fprintln(os.Stderr, "config:", err)
			return 1
		}
		f.Commands = loaded
	}

	ctx := context.Background()
	deps, err := buildDeps(ctx, f, *work, *dsn, *executor, *runnerID)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	c, err := dial(temporalAddr(*addr))
	if err != nil {
		fmt.Fprintln(os.Stderr, "temporal:", err)
		return 1
	}
	defer c.Close()

	orch := worker.New(c, temporalx.QueueOrchestrator, worker.Options{})
	orch.RegisterWorkflow(workflows.ExperimentWorkflow)
	orch.RegisterWorkflow(workflows.TrialWorkflow)
	orch.RegisterActivity(deps)

	rq := temporalx.RunnerQueue(*runnerID)
	run := worker.New(c, rq, worker.Options{
		DisableWorkflowWorker:              true,
		MaxConcurrentActivityExecutionSize: 12,
	})
	run.RegisterActivity(deps)

	logf("worker up: queues=%s,%s executor=%s temporal=%s",
		temporalx.QueueOrchestrator, rq, deps.Exec.Name(), temporalAddr(*addr))

	if err := orch.Start(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer orch.Stop()
	if err := run.Run(worker.InterruptCh()); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return 0
}

// preview resolves the cases and prints what would run, without touching
// Temporal or the read model.
func preview(ctx context.Context, f *spec.File, work string) int {
	casStore, err := cas.New(filepath.Join(work, "objects"))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	runner := cmdrun.New(f.Commands)
	res := &resolve.Resolver{Runner: runner, Store: casStore, WorkRoot: work, Log: logf}
	fmt.Printf("\nExperiment %s\n", f.Experiment.Name)
	rc := 0
	for _, c := range f.Experiment.Cases {
		ref := c.Merge(f.Experiment.CaseDefaults)
		cv, err := res.Resolve(ctx, ref)
		if err != nil {
			fmt.Printf("  %-46s INVALID: %v\n", ref.Label(), err)
			rc = 1
			continue
		}
		fmt.Printf("  %-46s %s  cpus=%d mem=%dMB agent_timeout=%s\n",
			cv.Label, cv.SHA256[:12], cv.Cfg.Resources.CPUs, cv.Cfg.Resources.MemoryMB, cv.Cfg.AgentTimeout)
	}
	return rc
}

// cmdSubmit starts an ExperimentWorkflow and waits for it.
func cmdSubmit(args []string) int {
	fs := flag.NewFlagSet("submit", flag.ExitOnError)
	dsn := fs.String("dsn", defaultDSN(), "PostgreSQL DSN")
	addr := fs.String("temporal", "", "Temporal frontend address")
	runnerID := fs.String("runner", "runner-01", "runner id to place work on")
	wait := fs.Bool("wait", true, "block until the experiment finishes")
	dry := fs.Bool("dry-run", false, "resolve the cases and print a preview, start nothing")
	work := fs.String("work", defaultWork(), "working directory (dry-run only)")
	if len(args) == 0 {
		usage()
	}
	file := args[0]
	fs.Parse(args[1:])

	f, err := spec.Load(file)
	if err != nil {
		fmt.Fprintln(os.Stderr, "load:", err)
		return 1
	}
	ctx, cancel := signalCtx()
	defer cancel()

	if *dry {
		return preview(ctx, f, *work)
	}

	db, err := store.Open(ctx, *dsn)
	if err != nil {
		fmt.Fprintln(os.Stderr, "postgres:", err)
		return 1
	}
	defer db.Close()
	for _, s := range f.LLMSpecs {
		if err := db.UpsertLLMSpec(ctx, s.Name, s.Provider, s.BaseURL, s.Model,
			s.APIKeyEnv, s.APIKeyCmd, s.MaxConcurrent, s.Parameters); err != nil {
			fmt.Fprintln(os.Stderr, "llm spec:", err)
			return 1
		}
	}

	c, err := dial(temporalAddr(*addr))
	if err != nil {
		fmt.Fprintln(os.Stderr, "temporal:", err)
		return 1
	}
	defer c.Close()

	expID := "exp-" + strconv.FormatInt(time.Now().Unix(), 36)
	in := workflows.ExperimentInput{
		ExpID: expID, Experiment: f.Experiment, LLMSpecs: f.LLMSpecs,
		RunnerQueue: temporalx.RunnerQueue(*runnerID), RunnerID: *runnerID,
	}
	we, err := c.ExecuteWorkflow(ctx, client.StartWorkflowOptions{
		ID:        expID,
		TaskQueue: temporalx.QueueOrchestrator,
		// a repeated confirm of the same experiment must not fan out twice
		WorkflowIDReusePolicy: 3, // REJECT_DUPLICATE
	}, workflows.ExperimentWorkflow, in)
	if err != nil {
		fmt.Fprintln(os.Stderr, "start:", err)
		return 1
	}
	fmt.Printf("started %s (run %s)\n", we.GetID(), we.GetRunID())
	if !*wait {
		return 0
	}
	if err := we.Get(ctx, nil); err != nil {
		fmt.Fprintln(os.Stderr, "\nexperiment failed:", err)
		printResults(ctx, db, expID, -1)
		return 1
	}
	fmt.Println()
	printResults(ctx, db, expID, -1)
	fmt.Printf("\nexperiment id: %s\n", expID)
	return 0
}
