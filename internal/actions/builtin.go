package actions

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/BurntSushi/toml"

	"github.com/andreasfoo/rollout-man/internal/archive"
	"github.com/andreasfoo/rollout-man/internal/config"
	"github.com/andreasfoo/rollout-man/internal/fail"
	"github.com/andreasfoo/rollout-man/internal/redact"
)

func init() {
	register(admissionAction{})
	register(redactAction{})
	register(guardAction{})
	register(archiveAction{})
	register(datasetAction{})
	register(shipAction{})
	register(checkCaseAction{})
	register(reportAction{})
	register(shipGitHubAction{})
}

// ------------------------------------------------------------- admission ---

// admission asks whether a case can be measured at all, by running the two
// probes whose answers are known in advance. A broken environment, a rotted
// reference solution or a hole in the verifier all produce numbers that look
// exactly like a capability difference once they are in the table.
type admissionAction struct{}

func (admissionAction) Name() string    { return "admission" }
func (admissionAction) Scopes() []Scope { return []Scope{PerCase} }

func (admissionAction) Validate(a config.Action) error {
	if err := unknown(a, "oracle_min_reward", "nop_max_reward", "trials", "require"); err != nil {
		return err
	}
	if r := a.Str("require", "admitted"); r != "admitted" && r != "any" {
		return fmt.Errorf("require must be admitted or any, got %q", r)
	}
	return nil
}

func (admissionAction) Run(ctx context.Context, c *Ctx, a config.Action) error {
	if a.Str("require", "admitted") == "any" {
		c.Logf("case %s: admission skipped (require: any)", c.CaseLabel)
		return nil
	}
	n := int(a.Num("trials", 2))
	if n <= 0 {
		n = 2
	}
	probes := []struct {
		kind string
		want string
		ok   func(float64) bool
	}{
		{"oracle", fmt.Sprintf(">= %.2f", a.Num("oracle_min_reward", 1.0)),
			func(v float64) bool { return v >= a.Num("oracle_min_reward", 1.0)-1e-9 }},
		{"nop", fmt.Sprintf("<= %.2f", a.Num("nop_max_reward", 0.0)),
			func(v float64) bool { return v <= a.Num("nop_max_reward", 0.0)+1e-9 }},
	}
	for _, p := range probes {
		got, err := c.Probe(ctx, p.kind, n)
		if err != nil {
			// A probe that could not run is not a verdict.
			return fmt.Errorf("%s probe for %s could not run: %w", p.kind, c.CaseLabel, err)
		}
		pass := true
		for _, v := range got {
			if !p.ok(v) {
				pass = false
			}
		}
		verdict := "PASS"
		if !pass {
			verdict = "FAIL"
		}
		c.Logf("case %s: %s %s want %s -> %s", c.CaseLabel, p.kind, fmtNums(got), p.want, verdict)
		if !pass {
			return fmt.Errorf("CASE_NOT_ADMITTED: %s (%s scored %s, wanted %s)",
				c.CaseLabel, p.kind, fmtNums(got), p.want)
		}
	}
	return nil
}

func fmtNums(v []float64) string {
	parts := make([]string, len(v))
	for i, x := range v {
		parts[i] = fmt.Sprintf("%g", x)
	}
	return "[" + strings.Join(parts, " ") + "]"
}

// ---------------------------------------------------------------- redact ---

// redact is mandatory for keys and tiered for addresses. It has to run here,
// on the machine that produced the artifacts, because a key that reaches a
// published dataset cannot be taken back: git history, LFS, mirrors and the
// dataset viewer's cache all keep their copy.
type redactAction struct{}

func (redactAction) Name() string    { return "redact" }
func (redactAction) Scopes() []Scope { return []Scope{PerTrial} }

func (redactAction) Validate(a config.Action) error {
	if err := unknown(a, "keys", "ips", "extra_patterns"); err != nil {
		return err
	}
	if k := a.Str("keys", "required"); k != "required" {
		return fmt.Errorf(`keys must be "required" -- there is no switch for it, got %q`, k)
	}
	return nil
}

// distributable names the artifacts that leave the team. Addresses are scrubbed
// from those and kept in the rest: what ships and what you debug with want
// opposite things, and an IPv4 regex eats version numbers for breakfast.
var distributable = map[string]redact.Class{
	"traj.jsonl":  redact.Distributable,
	"result.json": redact.Distributable,
}

func (redactAction) Run(ctx context.Context, c *Ctx, a config.Action) error {
	if _, err := os.Stat(c.Trial.OutDir); err != nil {
		return nil // the trial never got far enough to produce artifacts
	}
	ips := a.Flags("ips")
	s := redact.New(c.Secrets, a.Strs("extra_patterns"), map[redact.Class]bool{
		redact.Distributable: ips["traj"],
		redact.Debug:         ips["logs"],
	})
	var total redact.Hits
	// Walk, not glob: an adapter may leave a directory of trajectories, and the
	// one thing that must not happen is artifacts publishing unscrubbed because
	// they were one level deeper than we looked.
	err := filepath.WalkDir(c.Trial.OutDir, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		class := redact.Debug
		if cl, ok := distributable[d.Name()]; ok {
			class = cl
		}
		h, serr := s.ScrubFile(p, class)
		if serr != nil {
			rel, _ := filepath.Rel(c.Trial.OutDir, p)
			return fail.Wrap(fail.RedactFailed, "scrub "+rel, serr)
		}
		total.Exact += h.Exact
		total.Pattern += h.Pattern
		total.IP += h.IP
		return nil
	})
	if err != nil {
		return err
	}
	c.Note("redaction", total)
	return nil
}

// ----------------------------------------------------------------- guard ---

// guard decides whether a result is worth keeping. It is a different question
// from admission: admission asks whether a case can be measured at all, guard
// asks whether this particular measurement belongs in what you publish.
//
// on_violation: drop is the curation case -- "keep only the trials the agent
// found hard". The measurement is still recorded either way; what changes is
// whether its artifacts are published.
type guardAction struct{}

func (guardAction) Name() string { return "guard" }
func (guardAction) Scopes() []Scope {
	return []Scope{PerTrial, PerExperiment}
}

var trialMetrics = []string{"reward", "steps", "seconds"}
var batchMetrics = []string{"trials", "shipped", "mean_reward"}

func (guardAction) Validate(a config.Action) error {
	known := []string{"require_measured"}
	// *_exclusive is deliberately separate from max_/min_: the latter use
	// inclusive bounds, while curation often needs a strict cutoff (e.g.
	// reward < 0.6 means accepted; reward == 0.6 is rejected).
	for _, m := range append(append([]string{}, trialMetrics...), batchMetrics...) {
		known = append(known, "min_"+m, "max_"+m, "min_"+m+"_exclusive", "max_"+m+"_exclusive")
	}
	if err := unknown(a, known...); err != nil {
		return err
	}
	switch a.OnViolation {
	case "", "fail", "warn", "drop":
	default:
		return fmt.Errorf("on_violation must be fail, warn or drop, got %q", a.OnViolation)
	}
	return nil
}

func (g guardAction) Run(ctx context.Context, c *Ctx, a config.Action) error {
	metrics, err := g.metrics(c)
	if err != nil {
		return err
	}
	var violations []string
	for name, got := range metrics {
		if a.Has("min_" + name) {
			if want := a.Num("min_"+name, 0); got < want {
				violations = append(violations, fmt.Sprintf("%s = %g, wanted >= %g", name, got, want))
			}
		}
		if a.Has("max_" + name) {
			if want := a.Num("max_"+name, 0); got > want {
				violations = append(violations, fmt.Sprintf("%s = %g, wanted <= %g", name, got, want))
			}
		}
		if a.Has("min_" + name + "_exclusive") {
			if want := a.Num("min_"+name+"_exclusive", 0); got <= want {
				violations = append(violations, fmt.Sprintf("%s = %g, wanted > %g", name, got, want))
			}
		}
		if a.Has("max_" + name + "_exclusive") {
			if want := a.Num("max_"+name+"_exclusive", 0); got >= want {
				violations = append(violations, fmt.Sprintf("%s = %g, wanted < %g", name, got, want))
			}
		}
	}
	if c.Scope == PerTrial && a.Bool("require_measured", false) && c.Trial.Reward == nil {
		violations = append(violations, "no reward: "+c.Trial.Code)
	}
	if len(violations) == 0 {
		return nil
	}
	sort.Strings(violations)
	msg := strings.Join(violations, "; ")
	switch a.OnViolation {
	case "warn":
		c.Logf("%s: guard %s: %s", c.Where(), a.Label(), msg)
		return nil
	case "drop":
		c.Drop = true
		c.Note("dropped_by", a.Label())
		c.Logf("%s: dropped by %s: %s", c.Where(), a.Label(), msg)
		return nil
	}
	return fmt.Errorf("guard %s: %s", a.Label(), msg)
}

func (guardAction) metrics(c *Ctx) (map[string]float64, error) {
	if c.Scope == PerTrial {
		m := map[string]float64{
			"seconds": c.Trial.Seconds,
			"steps":   float64(countSteps(c.Trial.OutDir)),
		}
		if c.Trial.Reward != nil {
			m["reward"] = *c.Trial.Reward
		}
		return m, nil
	}
	all := c.Trials()
	var sum float64
	var scored, shipped int
	for _, t := range all {
		if t.Reward != nil {
			sum += *t.Reward
			scored++
		}
		if c.Dropped == nil || !c.Dropped(t.ID) {
			shipped++
		}
	}
	m := map[string]float64{"trials": float64(len(all)), "shipped": float64(shipped)}
	if scored > 0 {
		m["mean_reward"] = sum / float64(scored)
	}
	return m, nil
}

// countSteps reads the trajectory's length. One JSONL line per turn is the
// convention every agent runtime we talk to already follows, and it is the only
// step count available without parsing a format we do not own.
func countSteps(outDir string) int {
	b, err := os.ReadFile(filepath.Join(outDir, "traj.jsonl"))
	if err != nil {
		return 0
	}
	n := 0
	for _, line := range strings.Split(string(b), "\n") {
		if strings.TrimSpace(line) != "" {
			n++
		}
	}
	return n
}

// --------------------------------------------------------------- archive ---

// archive packs one trial's artifacts into one file, referenced by path from
// that trial's row.
//
// A note on formats, because it is easy to believe more than is true. `tar` is
// the format a dataset hub can read *as WebDataset*, but only when one tar
// holds many samples named <key>.<ext>; one tar per trial is an attachment, not
// a WebDataset shard. `zip` is strictly worse than tar here -- the datasets
// library has no builder for `.zip` at all, so zips are opaque blobs to
// load_dataset and to the viewer, readable only by addressing them explicitly
// (`zip://inner::outer.zip`). Either way it is the rows, not the archives, that
// make this a dataset.
//
// At batch sizes where the data is the problem, one archive per trial also
// means one LFS object per trial. Sharding many trials into a few tars is the
// answer there, and it is not what this action does.
type archiveAction struct{}

func (archiveAction) Name() string    { return "archive" }
func (archiveAction) Scopes() []Scope { return []Scope{PerTrial} }

func (archiveAction) Validate(a config.Action) error {
	if err := unknown(a, "format", "include", "name", "keep_files"); err != nil {
		return err
	}
	switch a.Str("format", "tar") {
	case "tar", "tar.gz", "zip":
	default:
		return fmt.Errorf("format must be tar, tar.gz or zip, got %q", a.Str("format", ""))
	}
	return nil
}

func (archiveAction) Run(ctx context.Context, c *Ctx, a config.Action) error {
	if c.Drop {
		return nil // a guard already decided this one is not being published
	}
	if _, err := os.Stat(c.Trial.OutDir); err != nil {
		return nil
	}
	format := a.Str("format", "tar")
	name := a.Str("name", c.Trial.ID) + "." + format
	dst := filepath.Join(c.RunDir, "archives", name)
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	n, err := archive.Pack(dst, c.Trial.OutDir, format, a.Strs("include"))
	if err != nil {
		return err
	}
	c.Note("archive", map[string]any{"path": filepath.Join("archives", name), "files": n})
	if !a.Bool("keep_files", true) {
		return os.RemoveAll(c.Trial.OutDir)
	}
	return nil
}

// --------------------------------------------------------------- dataset ---

// dataset turns the run into the shape a dataset hub understands: one row per
// trial plus a card. Publishing a directory tree gives you a folder on a
// server; publishing this gives you something load_dataset can open and a
// viewer can render.
//
// The card carries the provenance the numbers are worthless without: which
// bytes each case was, whether it was admitted, which model, which adapter.
type datasetAction struct{}

func (datasetAction) Name() string    { return "dataset" }
func (datasetAction) Scopes() []Scope { return []Scope{PerExperiment} }

func (datasetAction) Validate(a config.Action) error {
	return unknown(a, "dir", "include_dropped", "include_infra_failures", "title", "license")
}

type row struct {
	TrialID string   `json:"trial_id"`
	Case    string   `json:"case"`
	CaseSHA string   `json:"case_sha256"`
	Agent   string   `json:"agent"`
	LLMSpec string   `json:"llm_spec,omitempty"`
	Index   int      `json:"index"`
	Reward  *float64 `json:"reward"`
	Code    string   `json:"failure_code,omitempty"`
	Steps   int      `json:"steps"`
	Seconds float64  `json:"seconds"`
	Archive string   `json:"archive,omitempty"`
}

func (datasetAction) Run(ctx context.Context, c *Ctx, a config.Action) error {
	dir := filepath.Join(c.RunDir, a.Str("dir", "dataset"))
	if err := os.MkdirAll(filepath.Join(dir, "data"), 0o755); err != nil {
		return err
	}
	includeDropped := a.Bool("include_dropped", false)
	// An agent that failed on its own merits is data; a case that would not
	// build on our machine is not. Same rule as the denominator: our bad day
	// does not belong in a published dataset any more than it belongs in an
	// agent's score.
	includeInfra := a.Bool("include_infra_failures", false)

	var rows []row
	for _, t := range c.Trials() {
		if !includeDropped && c.Dropped != nil && c.Dropped(t.ID) {
			continue
		}
		if !includeInfra && t.Reward == nil && !fail.Code(t.Code).CountsAgainstAgent() {
			continue
		}
		r := row{
			TrialID: t.ID, Case: t.Case, CaseSHA: t.CaseSHA, Agent: t.Agent,
			LLMSpec: t.LLMSpec, Index: t.Index, Reward: t.Reward, Code: t.Code,
			Seconds: t.Seconds, Steps: countSteps(t.OutDir),
		}
		for _, ext := range []string{"tar", "tar.gz", "zip"} {
			p := filepath.Join(c.RunDir, "archives", t.ID+"."+ext)
			if _, err := os.Stat(p); err == nil {
				r.Archive = "archives/" + t.ID + "." + ext
				break
			}
		}
		rows = append(rows, r)
	}

	var b strings.Builder
	for _, r := range rows {
		line, err := json.Marshal(r)
		if err != nil {
			return err
		}
		b.Write(line)
		b.WriteByte('\n')
	}
	if err := os.WriteFile(filepath.Join(dir, "data", "trials.jsonl"), []byte(b.String()), 0o644); err != nil {
		return err
	}
	card := datasetCard(c, a, rows)
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte(card), 0o644); err != nil {
		return err
	}
	c.Logf("dataset: %d rows -> %s", len(rows), dir)
	return nil
}

func datasetCard(c *Ctx, a config.Action, rows []row) string {
	title := a.Str("title", c.Experiment)
	cases := map[string]string{}
	for _, r := range rows {
		cases[r.Case] = r.CaseSHA
	}
	names := make([]string, 0, len(cases))
	for k := range cases {
		names = append(names, k)
	}
	sort.Strings(names)

	var b strings.Builder
	b.WriteString("---\n")
	if l := a.Str("license", ""); l != "" {
		b.WriteString("license: " + l + "\n")
	}
	b.WriteString("configs:\n")
	b.WriteString("  - config_name: default\n    data_files: data/trials.jsonl\n")
	b.WriteString("---\n\n")
	b.WriteString("# " + title + "\n\n")
	b.WriteString(fmt.Sprintf("One row per trial (`data/trials.jsonl`), %d rows, run `%s`.\n\n", len(rows), c.RunID))
	b.WriteString("Each row carries the score, the failure code when there is no score, the\n")
	b.WriteString("step count, and the path of that trial's artifact archive.\n\n")
	b.WriteString("## Provenance\n\n")
	b.WriteString("A score means nothing without the bytes it was measured on, so the exact\n")
	b.WriteString("content hash of every case is recorded here. Cases were admitted only if\n")
	b.WriteString("their reference solution scored full marks and doing nothing scored zero.\n\n")
	b.WriteString("| case | sha256 |\n|---|---|\n")
	for _, n := range names {
		b.WriteString(fmt.Sprintf("| `%s` | `%s` |\n", n, cases[n]))
	}
	b.WriteString("\nThe run's `manifest.json` records which adapter produced these trials and\n")
	b.WriteString("its hash.\n\n## Redaction\n\nKeys are removed from every artifact; addresses are removed from what is\n")
	b.WriteString("published and kept in the debug logs.\n")
	return b.String()
}

// ------------------------------------------------------------------ ship ---

// ship hands a path to a configured command. What shipping means -- a copy, a
// git push, a dataset upload, an rclone remote -- is the command's business,
// which is why there is exactly one ship action and not four.
type shipAction struct{}

func (shipAction) Name() string    { return "ship" }
func (shipAction) Scopes() []Scope { return []Scope{PerTrial, PerExperiment} }

func (shipAction) Validate(a config.Action) error {
	if err := unknown(a, "using", "dest", "path"); err != nil {
		return err
	}
	if a.Str("using", "") == "" {
		return fmt.Errorf("using: is required -- it names the command that does the shipping")
	}
	return nil
}

func (shipAction) Run(ctx context.Context, c *Ctx, a config.Action) error {
	if c.Scope == PerTrial && c.Drop {
		return nil
	}
	using := a.Str("using", "")
	if !c.Cmds.Has(using) {
		return fmt.Errorf("no command named %q is configured", using)
	}
	local := c.RunDir
	if p := a.Str("path", ""); p != "" {
		local = filepath.Join(c.RunDir, p)
	} else if c.Scope == PerTrial {
		local = c.Trial.OutDir
	}
	dest := expand(a.Str("dest", ""), c)
	if _, err := runStep(ctx, c, a, using, map[string]string{
		"LocalPath": local, "Key": dest,
		"Experiment": c.Experiment, "RunId": c.RunID, "RunDir": c.RunDir,
	}); err != nil {
		return err
	}
	c.Logf("shipped %s -> %s", local, dest)
	return nil
}

// camel turns with-key names into the CamelCase the command runner expects
// ({{.MyKey}} on the template side, MY_KEY in the environment).
func camel(s string) string {
	out := []rune{}
	up := true
	for _, r := range s {
		if r == '_' || r == '-' {
			up = true
			continue
		}
		if up && r >= 'a' && r <= 'z' {
			r = r - 'a' + 'A'
		}
		up = false
		out = append(out, r)
	}
	return string(out)
}

func expand(s string, c *Ctx) string {
	rep := strings.NewReplacer(
		"{{.Experiment}}", c.Experiment,
		"{{.RunID}}", c.RunID,
	)
	if c.Trial != nil {
		s = strings.NewReplacer("{{.TrialID}}", c.Trial.ID, "{{.Case}}", c.Trial.Case).Replace(s)
	}
	s = expandStepOutputs(s, c)
	return rep.Replace(s)
}

var stepOutputRe = regexp.MustCompile(`\{\{\s*steps\.([^.\s]+)\.outputs\.([^\s}]+)\s*\}\}`)

// expandStepOutputs resolves {{steps.<label>.outputs.<key>}} against what an
// earlier step in the same pipeline list wrote to $ROLLOUT_MAN_OUTPUT. An
// unresolved reference (unknown step or key) is left as an error rather than
// silently becoming an empty string, since a typo'd label would otherwise
// pass a blank value straight into a shell adapter.
func expandStepOutputs(s string, c *Ctx) string {
	if !strings.Contains(s, "{{steps.") && !strings.Contains(s, "{{ steps.") {
		return s
	}
	return stepOutputRe.ReplaceAllStringFunc(s, func(m string) string {
		g := stepOutputRe.FindStringSubmatch(m)
		label, key := g[1], g[2]
		outs, ok := c.StepOutputs[label]
		if !ok {
			return m // leave unresolved; caller sees the literal placeholder
		}
		return outs[key]
	})
}

// --------------------------------------------------------------- command ---

// command is the fallback: `uses:` that names a configured command rather than
// a built-in. This is how a custom step is written, and it is deliberately the
// same machinery -- pin, env allowlist, timeout -- as every other command.
type command struct{ cmd string }

func (c command) Name() string { return c.cmd }
func (command) Scopes() []Scope {
	return []Scope{PerCase, PerTrial, PerExperiment}
}
func (command) Validate(config.Action) error { return nil }

func (cm command) Run(ctx context.Context, c *Ctx, a config.Action) error {
	vars := map[string]string{
		"Experiment": c.Experiment, "RunId": c.RunID, "RunDir": c.RunDir,
		"LocalPath": c.RunDir, "Key": expand(a.Str("dest", ""), c),
	}
	if c.CaseDir != "" {
		vars["CaseDir"], vars["CaseLabel"], vars["CaseSha"] = c.CaseDir, c.CaseLabel, c.CaseSHA
	}
	if c.Trial != nil {
		vars["TrialId"], vars["OutDir"] = c.Trial.ID, c.Trial.OutDir
		vars["LocalPath"] = c.Trial.OutDir
	}
	// with: entries reach the command as template names and env vars, so a
	// custom step is configured the same way every other command is.
	// String values may reference {{steps.<label>.outputs.<key>}} from an
	// earlier step in the same pipeline list.
	for k, v := range a.With {
		if s, ok := v.(string); ok {
			vars[camel(k)] = expandStepOutputs(s, c)
		}
	}
	res, err := runStep(ctx, c, a, cm.cmd, vars)
	if len(res.Outputs) > 0 {
		if c.StepOutputs == nil {
			c.StepOutputs = map[string]map[string]string{}
		}
		c.StepOutputs[a.Label()] = res.Outputs
	}
	return err
}

// ------------------------------------------------------------ check_case ---

// checkCase validates the case package against the contract Harbor expects,
// before anything expensive runs against it.
//
// It is built in because it is the one check that is about the case *format*
// rather than about a project's own conventions -- and because the shell
// version of it parses TOML with grep, which finds a commented-out key, misses
// one under an unexpected table, and cannot tell an empty value from a missing
// one. rollout-man already reads task.toml; asking it the question directly is
// both shorter and right.
type checkCaseAction struct{}

func (checkCaseAction) Name() string    { return "check_case" }
func (checkCaseAction) Scopes() []Scope { return []Scope{PerCase} }

func (checkCaseAction) Validate(a config.Action) error {
	return unknown(a, "require", "require_fields")
}

// harborShape is what `harbor run --path <dir>` needs to see a single task at
// all. Without environment/ it silently falls through to dataset mode, scans
// the directory, finds no tasks, and the failure surfaces much later as
// "Either datasets or tasks must be provided" -- a message about nothing that
// is wrong with the case.
var harborShape = []string{"task.toml", "environment"}

func (checkCaseAction) Run(ctx context.Context, c *Ctx, a config.Action) error {
	required := a.Strs("require")
	if len(required) == 0 {
		required = harborShape
	}
	var missing []string
	for _, rel := range required {
		if _, err := os.Stat(filepath.Join(c.CaseDir, rel)); err != nil {
			missing = append(missing, rel)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("%s is missing %s", c.CaseLabel, strings.Join(missing, ", "))
	}

	fields := a.Strs("require_fields")
	if len(fields) == 0 {
		return nil
	}
	var doc map[string]any
	b, err := os.ReadFile(filepath.Join(c.CaseDir, "task.toml"))
	if err != nil {
		return err
	}
	if err := toml.Unmarshal(b, &doc); err != nil {
		return fmt.Errorf("%s: task.toml does not parse: %w", c.CaseLabel, err)
	}
	var empty []string
	for _, f := range fields {
		if v, ok := lookupField(doc, f); !ok || isBlank(v) {
			empty = append(empty, f)
		}
	}
	if len(empty) > 0 {
		return fmt.Errorf("%s: task.toml has no value for %s", c.CaseLabel, strings.Join(empty, ", "))
	}
	return nil
}

// lookupField finds a dotted path (environment.docker_image), or a bare name
// at any depth. The bare form exists because a submission should not have to
// know which table a case's author put the key in.
func lookupField(doc map[string]any, name string) (any, bool) {
	if strings.Contains(name, ".") {
		cur := any(doc)
		for _, part := range strings.Split(name, ".") {
			m, ok := cur.(map[string]any)
			if !ok {
				return nil, false
			}
			if cur, ok = m[part]; !ok {
				return nil, false
			}
		}
		return cur, true
	}
	if v, ok := doc[name]; ok {
		return v, true
	}
	for _, v := range doc {
		if m, ok := v.(map[string]any); ok {
			if got, ok := lookupField(m, name); ok {
				return got, true
			}
		}
	}
	return nil, false
}

func isBlank(v any) bool {
	s, ok := v.(string)
	return ok && strings.TrimSpace(s) == ""
}

// ---------------------------------------------------------------- report ---

// report writes what happened to a file, so a batch leaves something readable
// behind without anyone re-running `status` and pasting the output.
//
// It is the same aggregation `status` prints. Which number counts as a pass is
// a question, not a setting -- pass_at is the threshold this particular report
// was asked about, and it is recorded in the file so the answer cannot be read
// without the question.
type reportAction struct{}

func (reportAction) Name() string    { return "report" }
func (reportAction) Scopes() []Scope { return []Scope{PerExperiment} }

func (reportAction) Validate(a config.Action) error {
	if err := unknown(a, "dest", "format", "pass_at", "append"); err != nil {
		return err
	}
	switch a.Str("format", "") {
	case "", "md", "json", "csv":
	default:
		return fmt.Errorf("format must be md, json or csv, got %q", a.Str("format", ""))
	}
	return nil
}

func (reportAction) Run(ctx context.Context, c *Ctx, a config.Action) error {
	dest := a.Str("dest", "report.md")
	format := a.Str("format", "")
	if format == "" {
		format = strings.TrimPrefix(filepath.Ext(dest), ".")
		if format != "json" && format != "csv" {
			format = "md"
		}
	}
	path := dest
	if !filepath.IsAbs(path) {
		path = filepath.Join(c.RunDir, dest)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}

	rows := aggregate(c, a.Num("pass_at", -1))
	var body string
	switch format {
	case "json":
		b, err := json.MarshalIndent(map[string]any{
			"experiment": c.Experiment, "run_id": c.RunID, "agents": rows,
		}, "", "  ")
		if err != nil {
			return err
		}
		body = string(b) + "\n"
	case "csv":
		body = "agent,llm_spec,measured,mean,not_measured,passed\n"
		for _, r := range rows {
			body += fmt.Sprintf("%s,%s,%d,%.4f,%d,%d\n",
				r.Agent, r.LLMSpec, r.Measured, r.Mean, r.NotMeasured, r.Passed)
		}
	default:
		body = markdownReport(c, rows, a.Num("pass_at", -1))
	}

	if a.Bool("append", false) {
		f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		if err != nil {
			return err
		}
		defer f.Close()
		_, err = f.WriteString(body)
		return err
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		return err
	}
	c.Logf("report: %s", path)
	return nil
}

type reportRow struct {
	Agent       string  `json:"agent"`
	LLMSpec     string  `json:"llm_spec,omitempty"`
	Measured    int     `json:"measured"`
	Mean        float64 `json:"mean"`
	NotMeasured int     `json:"not_measured"`
	Passed      int     `json:"passed,omitempty"`
	Denominator int     `json:"denominator,omitempty"`
}

// aggregate counts the same way status does, denominator included: only the
// agent's own failures belong in it, because counting our infrastructure
// trouble marks every agent down for our bad day.
func aggregate(c *Ctx, passAt float64) []reportRow {
	idx := map[string]*reportRow{}
	var order []*reportRow
	for _, t := range c.Trials() {
		if strings.HasPrefix(t.ID, "admit-") {
			continue
		}
		k := t.Agent + "|" + t.LLMSpec
		r, ok := idx[k]
		if !ok {
			r = &reportRow{Agent: t.Agent, LLMSpec: t.LLMSpec}
			idx[k], order = r, append(order, r)
		}
		switch {
		case t.Reward != nil:
			r.Measured++
			r.Mean += *t.Reward
			if passAt >= 0 && *t.Reward >= passAt {
				r.Passed++
			}
		default:
			r.NotMeasured++
			if fail.Code(t.Code).CountsAgainstAgent() {
				r.Denominator++
			}
		}
	}
	out := make([]reportRow, 0, len(order))
	for _, r := range order {
		if r.Measured > 0 {
			r.Mean /= float64(r.Measured)
		}
		r.Denominator += r.Measured
		out = append(out, *r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Agent < out[j].Agent })
	return out
}

func markdownReport(c *Ctx, rows []reportRow, passAt float64) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# %s — %s\n\n", c.Experiment, c.RunID)
	b.WriteString("| agent | llm spec | measured | mean | not measured |")
	if passAt >= 0 {
		fmt.Fprintf(&b, " pass@%.2f |", passAt)
	}
	b.WriteString("\n|---|---|---:|---:|---:|")
	if passAt >= 0 {
		b.WriteString("---:|")
	}
	b.WriteString("\n")
	for _, r := range rows {
		fmt.Fprintf(&b, "| %s | %s | %d | %.3f | %d |",
			r.Agent, dashIf(r.LLMSpec), r.Measured, r.Mean, r.NotMeasured)
		if passAt >= 0 {
			fmt.Fprintf(&b, " %d/%d |", r.Passed, r.Denominator)
		}
		b.WriteString("\n")
	}
	b.WriteString("\nOnly an agent's own failures are in the denominator: " +
		"infrastructure trouble is ours, and counting it would mark every agent " +
		"down for our bad day.\n")
	return b.String()
}

func dashIf(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// ------------------------------------------------------------ transports ---

// The three destinations everyone needs, built in.
//
// They drive the same CLIs an adapter would (`hf`, `rclone`, `git`), so no
// storage SDK enters this repository and no credential is read, stored or
// forwarded by it -- each tool finds its own, exactly as before. What changes
// is only who carries the glue: the tool, rather than every submission
// copying a script.
//
// They are transport and nothing else. *What* to send is already decided by
// the step before -- `dataset`, `archive`, or an explicit path: -- and keeping
// that separate is what stops one project's idea of "the right files" from
// becoming everyone's. Anything these three cannot reach is still `ship` with
// `using:` naming a command.
type transport struct {
	name string
	// env names the host variables this transport's CLI needs to find its own
	// credentials. The tool knows them; the operator should not have to.
	env  []string
	args func(c *Ctx, a config.Action, local string) ([]string, string, error)
}

func (t transport) Name() string    { return t.name }
func (t transport) Scopes() []Scope { return []Scope{PerTrial, PerExperiment} }

func (t transport) Validate(a config.Action) error {
	if err := unknown(a, "repo", "remote", "path", "dest", "private", "revision",
		"message", "branch", "env"); err != nil {
		return err
	}
	switch t.name {
	case "ship_hf":
		if a.Str("repo", "") == "" {
			return fmt.Errorf("repo: is required -- the dataset repo id, e.g. my-org/rollouts")
		}
	case "ship_rclone":
		if a.Str("remote", "") == "" {
			return fmt.Errorf("remote: is required -- an rclone remote, e.g. onedrive:rollout-man")
		}
	}
	return nil
}

func (t transport) Run(ctx context.Context, c *Ctx, a config.Action) error {
	if c.Scope == PerTrial && c.Drop {
		return nil
	}
	local := shipSource(c, a)
	if _, err := os.Stat(local); err != nil {
		return fmt.Errorf("nothing to ship at %s", local)
	}
	argv, where, err := t.args(c, a, local)
	if err != nil {
		return err
	}
	if _, err := c.Cmds.RunArgv(ctx, t.name, argv, append(t.env, a.Strs("env")...), 0); err != nil {
		return err
	}
	c.Logf("shipped %s -> %s", local, where)
	return nil
}

// shipSource is what to send: an explicit path, else the trial's artifacts,
// else the whole run directory.
func shipSource(c *Ctx, a config.Action) string {
	if p := a.Str("path", ""); p != "" {
		if filepath.IsAbs(p) {
			return p
		}
		return filepath.Join(c.RunDir, p)
	}
	if c.Scope == PerTrial && c.Trial != nil {
		return c.Trial.OutDir
	}
	return c.RunDir
}

func init() {
	register(transport{
		name: "ship_hf",
		env:  []string{"HF_TOKEN", "HF_HOME", "HF_ENDPOINT"},
		args: func(c *Ctx, a config.Action, local string) ([]string, string, error) {
			repo := a.Str("repo", "")
			argv := []string{"hf", "upload", repo, local, a.Str("dest", "."),
				"--repo-type", "dataset",
				"--commit-message", a.Str("message", "rollout-man: "+c.Experiment+" "+c.RunID)}
			if rev := a.Str("revision", ""); rev != "" {
				argv = append(argv, "--revision", rev)
			}
			if a.Bool("private", true) {
				argv = append(argv, "--private")
			}
			return argv, repo, nil
		},
	})
	register(transport{
		name: "ship_rclone",
		env:  []string{"RCLONE_CONFIG", "RCLONE_CONFIG_PASS"},
		args: func(c *Ctx, a config.Action, local string) ([]string, string, error) {
			dest := a.Str("remote", "") + "/" + strings.TrimPrefix(expand(a.Str("dest", ""), c), "/")
			return []string{"rclone", "copyto", "--create-empty-src-dirs", "--", local, dest}, dest, nil
		},
	})
}

// shipGitHub commits into a git checkout and pushes. Unlike the other two it
// is not one CLI call, so it is written out rather than squeezed into an argv
// builder -- but it is still transport only: what to commit was decided before.
type shipGitHubAction struct{}

func (shipGitHubAction) Name() string    { return "ship_github" }
func (shipGitHubAction) Scopes() []Scope { return []Scope{PerExperiment} }

func (shipGitHubAction) Validate(a config.Action) error {
	if err := unknown(a, "repo", "branch", "path", "dest", "message", "env"); err != nil {
		return err
	}
	if a.Str("dest", "") == "" {
		return fmt.Errorf("dest: is required -- the path inside the repository to write to")
	}
	return nil
}

func (s shipGitHubAction) Run(ctx context.Context, c *Ctx, a config.Action) error {
	repo := a.Str("repo", "")
	if repo == "" {
		return fmt.Errorf("repo: is required -- a checkout to commit into")
	}
	local := shipSource(c, a)
	dest := expand(a.Str("dest", ""), c)
	full := filepath.Join(repo, dest)
	if err := os.MkdirAll(full, 0o755); err != nil {
		return err
	}
	if err := copyTree(local, full); err != nil {
		return err
	}

	env := append([]string{"SSH_AUTH_SOCK", "GIT_AUTHOR_NAME", "GIT_AUTHOR_EMAIL",
		"GIT_COMMITTER_NAME", "GIT_COMMITTER_EMAIL", "GIT_SSH_COMMAND"}, a.Strs("env")...)
	git := func(args ...string) error {
		_, err := c.Cmds.RunArgv(ctx, "ship_github", append([]string{"git", "-C", repo}, args...), env, 0)
		return err
	}
	if err := git("add", "--", dest); err != nil {
		return err
	}
	// Nothing staged is success, not failure: a batch that produced no new
	// files should not fail the run for having nothing to say.
	if err := git("diff", "--cached", "--quiet", "--", dest); err == nil {
		c.Logf("ship_github: nothing new under %s", dest)
		return nil
	}
	msg := a.Str("message", fmt.Sprintf("%s: %s", c.Experiment, c.RunID))
	if err := git("commit", "-q", "-m", msg); err != nil {
		return err
	}
	branch := a.Str("branch", "HEAD")
	if err := git("push", "origin", branch); err != nil {
		return err
	}
	c.Logf("shipped %s -> %s (%s)", local, dest, branch)
	return nil
}

func copyTree(src, dst string) error {
	return filepath.WalkDir(src, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, rerr := filepath.Rel(src, p)
		if rerr != nil {
			return rerr
		}
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		b, rerr := os.ReadFile(p)
		if rerr != nil {
			return rerr
		}
		return os.WriteFile(target, b, 0o644)
	})
}
