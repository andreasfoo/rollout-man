// Package progress answers the three questions a long batch keeps raising:
// how much is there, how far along is it, and what is happening right now.
//
// A run's log only ever spoke when a trial finished. With a pipeline of five
// steps and trials that take an hour, that is a very long silence followed by
// one line -- and no denominator anywhere, so "is this halfway or nearly done"
// had no answer at all.
//
// The state lives here rather than in the log because a log is a bad place to
// look something up: it is written once, read by scrolling, and says nothing
// about the present. This is written to progress.json as it changes, so a
// second terminal can read the current state without parsing anything.
package progress

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Entry is one trial that is currently somewhere in the pipeline.
type Entry struct {
	TrialID string    `json:"trial_id"`
	Case    string    `json:"case"`
	Step    string    `json:"step"`
	Since   time.Time `json:"since"`
}

type CaseCount struct {
	Total   int `json:"total"`
	Done    int `json:"done"`
	Dropped int `json:"dropped"`
	Failed  int `json:"failed"`
}

// Snapshot is the whole state at one instant, and the shape of progress.json.
type Snapshot struct {
	Experiment string               `json:"experiment"`
	RunID      string               `json:"run_id"`
	StartedAt  time.Time            `json:"started_at"`
	UpdatedAt  time.Time            `json:"updated_at"`
	Total      int                  `json:"total"`
	Done       int                  `json:"done"`
	Dropped    int                  `json:"dropped"`
	Failed     int                  `json:"failed"`
	Cases      map[string]CaseCount `json:"cases"`
	Running    []Entry              `json:"running"`
	Finished   bool                 `json:"finished"`
}

type Tracker struct {
	mu       sync.Mutex
	path     string
	snap     Snapshot
	running  map[string]*Entry
	now      func() time.Time
	disabled bool
}

// New seeds the tracker with the trial list, so the denominator is known
// before the first trial starts rather than discovered at the end.
func New(dir, experiment, runID string, trialCases []string) *Tracker {
	t := &Tracker{
		path:    filepath.Join(dir, "progress.json"),
		running: map[string]*Entry{},
		now:     time.Now,
		snap: Snapshot{
			Experiment: experiment, RunID: runID, StartedAt: time.Now(),
			Total: len(trialCases), Cases: map[string]CaseCount{},
		},
	}
	for _, c := range trialCases {
		e := t.snap.Cases[c]
		e.Total++
		t.snap.Cases[c] = e
	}
	t.flush()
	return t
}

// Resumed accounts for trials that a previous run already recorded, so the
// counter reads against the whole batch rather than only the missing part.
func (t *Tracker) Resumed(caseLabels []string, dropped map[string]bool) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, c := range caseLabels {
		e := t.snap.Cases[c]
		e.Done++
		t.snap.Cases[c] = e
		t.snap.Done++
	}
	t.flushLocked()
}

// Step records that a trial has entered a named step. This is the only place
// the middle of a pipeline becomes visible.
func (t *Tracker) Step(trialID, caseLabel, step string) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.running[trialID] = &Entry{TrialID: trialID, Case: caseLabel, Step: step, Since: t.now()}
	t.flushLocked()
}

// Finish removes a trial from the in-flight set and counts it.
func (t *Tracker) Finish(trialID, caseLabel string, measured, dropped bool) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.running, trialID)
	e := t.snap.Cases[caseLabel]
	e.Done++
	switch {
	case dropped:
		e.Dropped++
		t.snap.Dropped++
	case !measured:
		e.Failed++
		t.snap.Failed++
	}
	t.snap.Cases[caseLabel] = e
	t.snap.Done++
	t.flushLocked()
}

func (t *Tracker) Close() {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.snap.Finished = true
	t.flushLocked()
}

func (t *Tracker) Snapshot() Snapshot {
	if t == nil {
		return Snapshot{}
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.snapshotLocked()
}

func (t *Tracker) snapshotLocked() Snapshot {
	s := t.snap
	s.UpdatedAt = t.now()
	s.Cases = map[string]CaseCount{}
	for k, v := range t.snap.Cases {
		s.Cases[k] = v
	}
	s.Running = make([]Entry, 0, len(t.running))
	for _, e := range t.running {
		s.Running = append(s.Running, *e)
	}
	sort.Slice(s.Running, func(i, j int) bool { return s.Running[i].Since.Before(s.Running[j].Since) })
	return s
}

// flushLocked writes progress.json by rename, so a reader never sees a half
// written file -- something that matters precisely because the expected use is
// another process reading it while this one writes.
func (t *Tracker) flushLocked() {
	if t.disabled || t.path == "" {
		return
	}
	b, err := json.MarshalIndent(t.snapshotLocked(), "", "  ")
	if err != nil {
		return
	}
	tmp := t.path + ".tmp"
	if os.WriteFile(tmp, append(b, '\n'), 0o644) != nil {
		return
	}
	_ = os.Rename(tmp, t.path)
}

func (t *Tracker) flush() { t.mu.Lock(); defer t.mu.Unlock(); t.flushLocked() }

// Load reads a run's live state, for anything that is not the process writing it.
func Load(dir string) (Snapshot, error) {
	var s Snapshot
	b, err := os.ReadFile(filepath.Join(dir, "progress.json"))
	if err != nil {
		return s, err
	}
	return s, json.Unmarshal(b, &s)
}

// Lines renders the heartbeat: one line of counts and what is running, and a
// second of per-case progress when there is more than one case. Empty when
// there is nothing in flight and nothing to say.
func (s Snapshot) Lines(slowAfter time.Duration, now time.Time) []string {
	if len(s.Running) == 0 && s.Done == 0 {
		return nil
	}
	var out []string

	head := fmt.Sprintf("%d/%d done", s.Done, s.Total)
	if len(s.Running) > 0 {
		head += fmt.Sprintf(" · %d running (%s)", len(s.Running), stepCounts(s.Running))
	}
	if s.Dropped > 0 {
		head += fmt.Sprintf(" · %d dropped", s.Dropped)
	}
	if s.Failed > 0 {
		head += fmt.Sprintf(" · %d not measured", s.Failed)
	}
	out = append(out, head)

	if len(s.Cases) > 1 {
		out = append(out, "   "+caseLine(s.Cases))
	}
	// The oldest thing in flight is the only one worth naming: it is the
	// answer to "is something stuck", which is why anyone reads this at all.
	if len(s.Running) > 0 {
		if e := s.Running[0]; now.Sub(e.Since) >= slowAfter {
			out = append(out, fmt.Sprintf("   slowest: %s in %s for %s",
				e.TrialID, e.Step, round(now.Sub(e.Since))))
		}
	}
	return out
}

func stepCounts(running []Entry) string {
	n := map[string]int{}
	for _, e := range running {
		n[e.Step]++
	}
	keys := make([]string, 0, len(n))
	for k := range n {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s×%d", k, n[k]))
	}
	return strings.Join(parts, ", ")
}

func caseLine(cases map[string]CaseCount) string {
	keys := make([]string, 0, len(cases))
	for k := range cases {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		c := cases[k]
		parts = append(parts, fmt.Sprintf("%s %d/%d", Short(k), c.Done, c.Total))
	}
	return strings.Join(parts, " · ")
}

// Short trims a case path to its last segment: the run's own log already said
// where it came from, and the full path crowds out everything else.
func Short(label string) string {
	if i := strings.LastIndexByte(label, '/'); i >= 0 && i+1 < len(label) {
		return label[i+1:]
	}
	return label
}

func round(d time.Duration) time.Duration {
	if d < time.Minute {
		return d.Round(time.Second)
	}
	return d.Round(time.Second)
}
