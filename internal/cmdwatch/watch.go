// Package cmdwatch polls a directory of cases and gates each new one through
// an experiment's pipeline.per_case as soon as it appears. An existing case
// whose content changes is re-gated once the edit settles (same hash on two
// consecutive scans) -- a case left permanently un-gated because someone
// touched it is worse than re-judging settled bytes.
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
	"strconv"
	"strings"
	"sync"
	"syscall"
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

// gateBackoff paces re-gates of cases whose gate keeps failing hard (a
// command declaring EX_TEMPFAIL, a crashed step): without it, a persistent
// infrastructure failure would re-run an expensive gate (a 10-minute audit
// subagent) every poll interval forever. A case's backoff is cleared by any
// content change or clean verdict. In-memory only -- a watch restart resets
// it, which is the right posture for a failure that may have been the old
// process's own environment.
type gateBackoff struct {
	fails      map[string]int
	retryAfter map[string]time.Time
	failHash   map[string]string // content hash the last failure was recorded against
}

func newGateBackoff() *gateBackoff {
	return &gateBackoff{fails: map[string]int{}, retryAfter: map[string]time.Time{}, failHash: map[string]string{}}
}

func (g *gateBackoff) allow(name string, now time.Time) bool {
	t, ok := g.retryAfter[name]
	return !ok || !now.Before(t)
}

func (g *gateBackoff) record(name, hash string, now time.Time) time.Time {
	// Same bytes failing again: escalate. New bytes: the edit may have
	// fixed the failure, so the count starts over.
	if g.failHash[name] != hash {
		g.fails[name] = 0
		g.failHash[name] = hash
	}
	g.fails[name]++
	shift := g.fails[name] - 1
	if shift > 5 {
		shift = 5
	}
	d := (5 * time.Minute) << shift // 5m, 10m, 20m, ... capped at 160m
	g.retryAfter[name] = now.Add(d)
	return g.retryAfter[name]
}

// clearIfChanged drops the backoff when the case's content has moved on
// from the bytes that failed. A failed re-gate leaves the state hash at
// the pre-edit value, so "state hash != current hash" alone cannot say
// "edited since the failure" -- only the recorded failure hash can.
func (g *gateBackoff) clearIfChanged(name, hash string) {
	if h, ok := g.failHash[name]; ok && h != hash {
		g.clear(name)
	}
}

func (g *gateBackoff) clear(name string) {
	delete(g.fails, name)
	delete(g.retryAfter, name)
	delete(g.failHash, name)
}

// lockFile is a marker inside the watched directory itself: while it exists,
// exactly one watch process owns that directory. It holds the watcher's pid
// so a stale lock (process killed -9, host reboot) can be recognized and
// taken over instead of blocking forever.
func lockFile(absDir string) string {
	return filepath.Join(absDir, ".rollout-man-watch.lock")
}

// acquireLock claims the watched directory via O_EXCL creation of the lock
// file. A lock whose pid is still alive is refused; a lock whose pid is gone
// is swept and reclaimed -- a stale lock must never wedge the directory, and
// pid liveness is the only fact needed to tell the two apart. It returns a
// release func (nil on failure) that removes the file; the pid inside is
// rewritten first so a fast crash between sweep and claim cannot leave two
// processes both believing they hold the lock.
func acquireLock(absDir string) (func(), error) {
	path := lockFile(absDir)
	for {
		f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
		if err == nil {
			_, _ = f.WriteString(strconv.Itoa(os.Getpid()))
			_ = f.Close()
			return func() { _ = os.Remove(path) }, nil
		}
		if !os.IsExist(err) {
			return nil, err
		}
		b, rerr := os.ReadFile(path)
		if rerr != nil {
			return nil, fmt.Errorf("watch lock %s exists but is unreadable: %w", path, rerr)
		}
		pid, perr := strconv.Atoi(strings.TrimSpace(string(b)))
		if perr != nil {
			// Unreadable content: assume a torn write by a dead process.
			_ = os.Remove(path)
			continue
		}
		if pid == os.Getpid() {
			return nil, fmt.Errorf("watch lock %s already held by this pid", path)
		}
		if err := syscall.Kill(pid, 0); err == nil {
			return nil, fmt.Errorf("another watch (pid %d) is already watching %s; leaving it alone", pid, absDir)
		}
		// Holder is gone. Sweep and retry the claim.
		_ = os.Remove(path)
	}
}

func firstLineOf(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

// Cmd is the `rollout-man watch <dir> <file.yaml>` entry point.
func Cmd(args []string, d Deps) int {
	fs := flag.NewFlagSet("watch", flag.ExitOnError)
	runs := fs.String("runs", "", "directory holding runs (default ./runs)")
	commandsFile := fs.String("commands", "", "take commands from this file instead of the submission")
	interval := fs.Duration("interval", 15*time.Second, "how often to re-scan the watched directory")
	full := fs.Bool("full", false, "run the trial (per_trial) for every newly admitted case, not just the gate")
	if len(args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: rollout-man watch <dir> <file.yaml> [--interval 15s] [--runs DIR] [--commands FILE] [--full]")
		return 2
	}
	dir, file := args[0], args[1]
	fs.Parse(args[2:])

	absDir, err := filepath.Abs(dir)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}

	// Claim the watched directory before doing anything else. Two watchers
	// on one directory double-gate every case (duplicate oracle/nop trials,
	// duplicate ships) while sharing one run dir -- the lock makes the
	// second process refuse loudly instead.
	release, err := acquireLock(absDir)
	if err != nil {
		fmt.Fprintln(os.Stderr, "watch:", err)
		return 1
	}
	defer release()

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

	// Under --full a trial can run for hours, so it cannot run inside poll():
	// nothing else would be gated until it finished. Each admitted case's
	// trial runs in its own goroutine, capped by the concurrency the yaml
	// already declares for a full run -- watch honors the same budget.
	var wg sync.WaitGroup
	var sem chan struct{}
	if *full {
		sem = make(chan struct{}, r.File.Experiment.Concurrency)
	}

	pending := map[string]string{}
	gb := newGateBackoff()
	poll(ctx, r, absDir, st, statePath, d.Logf, *full, sem, &wg, pending, gb)
	t := time.NewTicker(*interval)
	defer t.Stop()
	finished := false
	for {
		select {
		case <-ctx.Done():
			// The same posture as a Ctrl-C'd run: an interrupted trial is
			// simply not recorded. Waiting for hours-long containers to exit
			// would turn "stop the watch" into "stop the machine's work".
			if n := inflightCount(); n > 0 {
				d.Logf("watch: stopping with %d trial(s) still in flight; their results are not recorded", n)
			}
			return 0
		case <-t.C:
			idle := poll(ctx, r, absDir, st, statePath, d.Logf, *full, sem, &wg, pending, gb)
			// A quiet poll with nothing in flight is the batch-shaped moment
			// a `run` would reach at its end: run the per_experiment steps
			// (record_progress and friends) exactly once. Watch has no
			// natural "end", so this is the closest equivalent -- and the
			// steps themselves stay idempotent for a resumed watch.
			if idle && inflightCount() == 0 && !finished {
				if err := r.Finish(ctx); err != nil {
					d.Logf("watch: per_experiment steps failed: %v", err)
				} else {
					finished = true
				}
			}
		}
	}
}

// inflight counts --full trials currently running, so shutdown can say how
// many it is abandoning rather than only hanging or going quiet.
var inflight struct {
	mu sync.Mutex
	n  int
}

func inflightCount() int {
	inflight.mu.Lock()
	defer inflight.mu.Unlock()
	return inflight.n
}

// poll does one scan of dir: gate every not-yet-seen case, and re-gate every
// changed case whose content has settled (identical hash on two consecutive
// scans -- a writer mid-edit must not be gated on torn bytes). Gating runs
// concurrently, bounded by pipeline.per_case_concurrency like a batch run's
// per_case stage; state is applied serially as verdicts arrive. Under full, a
// case that passes the gate also has its trial run -- in a capped background
// goroutine, so one hours-long trial never blocks gating the next arrival.
// It reports whether the scan was quiet: every case dir seen before, no new
// one gated, none settled into a re-gate -- the caller's cue that nothing is
// in progress scan-wise. pending tracks changed-not-yet-settled cases across
// polls (name -> hash seen last poll) and is owned by the caller's goroutine.
func poll(ctx context.Context, r *run.Runner, dir string, st *state, statePath string,
	logf func(string, ...any), full bool, sem chan struct{}, wg *sync.WaitGroup,
	pending map[string]string, gb *gateBackoff) bool {
	defaults := r.File.Experiment.CaseDefaults
	entries, err := os.ReadDir(dir)
	if err != nil {
		logf("watch: read %s: %v", dir, err)
		return false
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

	// Scan phase (serial, cheap): hash every case and decide what this pass
	// gates. State is not touched by the gate goroutines below -- verdicts
	// come back on a channel and are applied here, one at a time.
	type gateJob struct {
		name, dir, hash string
		regate          bool
	}
	var jobs []gateJob
	dirty := false
	for _, name := range names {
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
			// A case whose last gate failed hard backs off instead of
			// re-running an expensive gate every poll -- unless its bytes
			// have changed since that failure, which may be the fix.
			gb.clearIfChanged(name, hash)
			if !gb.allow(name, time.Now()) {
				continue
			}
			logf("watch: new case %s -> gating", name)
			jobs = append(jobs, gateJob{name, caseDir, hash, false})
			delete(pending, name)
		case prev != hash:
			// New bytes since the last recorded failure: the edit may
			// have fixed it, so the backoff starts over.
			gb.clearIfChanged(name, hash)
			if pending[name] == hash {
				// Same hash as last poll: the edit has settled. A case
				// whose content changed was edited deliberately -- gate
				// the new bytes rather than leaving the case in limbo.
				// A backed-off case stays pending so it re-offers itself
				// once the backoff expires.
				if !gb.allow(name, time.Now()) {
					continue
				}
				logf("watch: case %s changed and has settled -> re-gating", name)
				jobs = append(jobs, gateJob{name, caseDir, hash, true})
				delete(pending, name)
			} else {
				logf("watch: case %s changed since last seen; waiting for it to settle before re-gating", name)
				pending[name] = hash
			}
		default:
			delete(pending, name)
		}
	}

	for name := range st.Cases {
		if !seen[name] {
			logf("watch: case %s removed", name)
			delete(st.Cases, name)
			delete(pending, name)
			dirty = true
		}
	}

	if len(jobs) == 0 {
		if dirty {
			if err := st.save(statePath); err != nil {
				logf("watch: save state: %v", err)
			}
		}
		// A case waiting to settle is work about to happen, not quiet.
		return len(pending) == 0
	}

	// Gate phase: the same bound a batch run's per_case stage uses. A full
	// pass of fresh cases gates conc-at-a-time instead of strictly one --
	// the audit subagent is the slowest step in the pipeline and it
	// parallelizes cleanly across cases.
	conc := r.File.Experiment.Pipeline.PerCaseConcurrency
	if conc < 1 {
		conc = 1
	}
	type gateResult struct {
		job      gateJob
		c        *casesrc.Case
		admitted bool
		err      error
	}
	results := make(chan gateResult, len(jobs))
	gsem := make(chan struct{}, conc)
	var gwg sync.WaitGroup
	for _, j := range jobs {
		gwg.Add(1)
		go func(j gateJob) {
			defer gwg.Done()
			gsem <- struct{}{}
			defer func() { <-gsem }()
			if ctx.Err() != nil {
				results <- gateResult{job: j, err: ctx.Err()}
				return
			}
			ref := config.CaseRef{Path: j.dir}.Merge(defaults)
			c, admitted, err := r.GateOne(ctx, ref)
			results <- gateResult{job: j, c: c, admitted: admitted, err: err}
		}(j)
	}
	go func() { gwg.Wait(); close(results) }()

	for res := range results {
		if res.err != nil {
			// An interrupted gate (Ctrl-C) and a hard failure share the
			// retry posture: record nothing, judge the case fresh next
			// poll. GateOne has likewise left no cache entry behind.
			// A hard failure additionally backs off, so a persistent
			// tempfail does not re-burn an expensive gate every poll.
			if ctx.Err() != nil {
				logf("watch: case %s: gate interrupted, will retry next poll", res.job.name)
			} else {
				next := gb.record(res.job.name, res.job.hash, time.Now())
				logf("watch: case %s: gate failed, retrying at %s: %v",
					res.job.name, next.Format("15:04:05"), res.err)
			}
			continue
		}
		gb.clear(res.job.name)
		logf("watch: case %s: %s", res.job.name, map[bool]string{true: "admitted", false: "rejected"}[res.admitted])
		// Record the post-gate hash, not the scan-time one: a repair step
		// edits the case in place, and recording the pre-repair bytes
		// would make the very next poll see a phantom "changed" case and
		// re-gate a verdict it just reached.
		st.Cases[res.job.name] = res.c.SHA256
		dirty = true
		// Save immediately, not at pass end: one gate can take minutes,
		// and a crash mid-pass would otherwise forget every verdict from
		// this pass and re-run them all.
		if err := st.save(statePath); err != nil {
			logf("watch: save state: %v", err)
		}

		// The state says "gated", not "tried" -- recording it here, right
		// after the gate, is today's semantics and stays. A trial killed
		// mid-flight is lost, like a Ctrl-C'd run; that limitation is
		// documented, not silently retried.
		if full && res.admitted {
			logf("watch: case %s: admitted -> running full pipeline (this may take a while)", res.job.name)
			wg.Add(1)
			inflight.mu.Lock()
			inflight.n++
			inflight.mu.Unlock()
			go func(c *casesrc.Case, name string) {
				// The slot is taken here, not in poll: a full set of
				// in-flight trials must queue the next one, not freeze
				// the scan that gates newly arrived cases.
				sem <- struct{}{}
				defer func() {
					<-sem
					inflight.mu.Lock()
					inflight.n--
					inflight.mu.Unlock()
					wg.Done()
				}()
				res, err := r.RunOneTrial(ctx, c)
				switch {
				case err != nil && strings.Contains(err.Error(), "already recorded"):
					// A re-gated case whose trial already shipped: the
					// new bytes were judged, but republishing a case
					// that is already on HF is a manual decision, not
					// watch's to make.
					logf("watch: case %s: re-gated admitted; trial already recorded, changed content not re-shipped automatically", name)
				case err != nil:
					logf("watch: case %s: trial failed: %v", name, err)
				case res.OK() && res.Dropped:
					logf("watch: case %s: trial %s done, reward=%.3f (dropped)", name, res.TrialID, *res.Reward)
				case res.OK():
					logf("watch: case %s: trial %s done, reward=%.3f", name, res.TrialID, *res.Reward)
				default:
					logf("watch: case %s: trial %s done, %s %s", name, res.TrialID, res.Code, firstLineOf(res.Message))
				}
			}(res.c, res.job.name)
		}
	}
	return false
}
