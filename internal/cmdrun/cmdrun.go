// Package cmdrun executes the configured external commands. A command is
// either an argv template ({{.Key}}) or an sh script reading upper-case
// environment variables. Exit code zero means success.
package cmdrun

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"text/template"
	"time"

	"github.com/andreasfoo/rollout-man/internal/config"
)

type Runner struct {
	Cmds        map[string]config.Command
	Timeout     time.Duration
	MaxAttempts int
	InheritEnv  bool
	Log         func(format string, args ...any)
	// LLMSpecs resolves a command's llm_spec: into LLM_BASE_URL / LLM_MODEL /
	// LLM_PROVIDER / LLM_API_KEY, exactly as a trial's agent llm_spec is
	// resolved (internal/run/run.go resolveLLM) -- an audit/fix subagent is
	// an LLM call like any other, so which endpoint it talks to belongs in
	// the submission, not hardcoded in the adapter script.
	LLMSpecs map[string]config.LLMSpec

	mu        sync.Mutex
	announced map[string]bool
}

func New(c config.Commands, llmSpecs map[string]config.LLMSpec) *Runner {
	r := &Runner{Cmds: c.Cmds, Timeout: c.Timeout.D(), MaxAttempts: c.MaxAttempts,
		InheritEnv: c.Inherits(), LLMSpecs: llmSpecs}
	if r.Timeout <= 0 {
		r.Timeout = 30 * time.Minute
	}
	if r.MaxAttempts <= 0 {
		r.MaxAttempts = 3
	}
	if r.Cmds == nil {
		r.Cmds = map[string]config.Command{}
	}
	return r
}

func (r *Runner) Has(name string) bool {
	c, ok := r.Cmds[name]
	return ok && !c.Empty()
}

type Result struct {
	Stdout string
	Stderr string
	// Outputs are key=value lines the command wrote to the file named by
	// $ROLLOUT_MAN_OUTPUT, mirroring GitHub Actions' $GITHUB_OUTPUT. A step
	// further down the same pipeline can read them as
	// ${{ steps.<label>.outputs.<key> }} instead of the two of them agreeing
	// on a stdout format to scrape.
	Outputs map[string]string
}

// Run executes the named command. vars keys are Go template field names
// ("Key", "LocalPath"); they are also exported as KEY / LOCAL_PATH.
func (r *Runner) Run(ctx context.Context, name string, vars map[string]string) (Result, error) {
	c, ok := r.Cmds[name]
	if !ok || c.Empty() {
		return Result{}, fmt.Errorf("command %q is not configured", name)
	}
	var last error
	for attempt := 1; attempt <= r.MaxAttempts; attempt++ {
		res, err := r.once(ctx, name, c, vars, r.Timeout)
		if err == nil {
			return res, nil
		}
		last = err
		if ctx.Err() != nil {
			break
		}
		if attempt < r.MaxAttempts {
			r.logf("command %s attempt %d failed: %v", name, attempt, err)
			time.Sleep(time.Duration(attempt) * time.Second)
		}
	}
	return Result{}, fmt.Errorf("command %s failed after %d attempts: %w", name, r.MaxAttempts, last)
}

// RunOnce executes the named command exactly once, with its own timeout. A
// trial goes through here rather than Run: retrying is the runner's decision,
// made from the failure code, and a silent retry inside the adapter would run
// the agent twice and report it once.
func (r *Runner) RunOnce(ctx context.Context, name string, vars map[string]string, timeout time.Duration) (Result, error) {
	c, ok := r.Cmds[name]
	if !ok || c.Empty() {
		return Result{}, fmt.Errorf("command %q is not configured", name)
	}
	if timeout <= 0 {
		timeout = r.Timeout
	}
	return r.once(ctx, name, c, vars, timeout)
}

func (r *Runner) once(ctx context.Context, name string, c config.Command, vars map[string]string, timeout time.Duration) (Result, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	var cmd *exec.Cmd
	switch {
	case c.Uses != "":
		sum, err := verify(c)
		if err != nil {
			return Result{}, err
		}
		if r.announce(name) {
			// Once per command, not once per invocation: at batch scale this
			// line would otherwise outnumber the results.
			r.logf("command %s -> %s (sha256 %s)", name, c.Uses, sum[:12])
		}
		// Run the file, not a shell reading the file. The contract is
		// "environment in, files out" -- nothing in it says shell -- so an
		// adapter is free to be Python, a compiled binary, or anything else
		// with a shebang. Interpreting it here would force every adapter to
		// be a shell script and silently ignore the one it declares.
		cmd = exec.CommandContext(ctx, c.Uses)
	case len(c.Run) > 0:
		argv := make([]string, 0, len(c.Run))
		for _, a := range c.Run {
			s, err := render(a, vars)
			if err != nil {
				return Result{}, err
			}
			argv = append(argv, s)
		}
		cmd = exec.CommandContext(ctx, argv[0], argv[1:]...)
	default:
		cmd = exec.CommandContext(ctx, shell(), "-c", c.Script)
	}
	cmd.Env = append(r.hostEnv(c), envPairs(vars)...)
	if c.LLMSpec != "" {
		llmEnv, err := r.resolveLLM(ctx, c.LLMSpec)
		if err != nil {
			return Result{}, err
		}
		cmd.Env = append(cmd.Env, llmEnv...)
	}

	outFile, err := os.CreateTemp("", "rollout-man-output-*")
	if err != nil {
		return Result{}, fmt.Errorf("create output file: %w", err)
	}
	outPath := outFile.Name()
	outFile.Close()
	defer os.Remove(outPath)
	cmd.Env = append(cmd.Env, "ROLLOUT_MAN_OUTPUT="+outPath)

	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		return Result{}, fmt.Errorf("%w: %s", err, strings.TrimSpace(tail(errb.String(), 2000)))
	}
	outputs, err := parseOutputFile(outPath)
	if err != nil {
		return Result{}, fmt.Errorf("%s wrote a malformed $ROLLOUT_MAN_OUTPUT: %w", name, err)
	}
	return Result{Stdout: out.String(), Stderr: errb.String(), Outputs: outputs}, nil
}

// parseOutputFile reads key=value lines written to $ROLLOUT_MAN_OUTPUT, the
// same convention as GitHub Actions' $GITHUB_OUTPUT. A missing file (most
// commands never write one) is not an error -- it just means no outputs.
func parseOutputFile(path string) (map[string]string, error) {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()
	out := map[string]string{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			return nil, fmt.Errorf("line %q is not key=value", line)
		}
		out[strings.TrimSpace(k)] = v
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return nil, nil
	}
	return out, nil
}

// hostEnv decides what of the machine the command gets to see. Inheriting
// everything is the default because the usual case is an operator running their
// own submission on their own machine, where the command needs DOCKER_HOST,
// HOME and its own credentials anyway. With inherit_env: false the command sees
// only what it declared -- so a ship command that talks to git does not also
// get handed the keys for the model provider.
func (r *Runner) hostEnv(c config.Command) []string {
	if r.InheritEnv {
		return os.Environ()
	}
	out := []string{
		"HOME=" + os.Getenv("HOME"),
		"PATH=" + os.Getenv("PATH"),
		"LANG=C.UTF-8",
		"TERM=dumb",
	}
	for _, k := range c.Env {
		if v, ok := os.LookupEnv(k); ok {
			out = append(out, k+"="+v)
		}
	}
	return out
}

// resolveLLM turns a command's llm_spec: into LLM_BASE_URL / LLM_MODEL /
// LLM_PROVIDER / LLM_API_KEY env pairs, the same names and the same
// api_key_env/api_key_cmd resolution internal/run/run.go's resolveLLM uses
// for a trial's agent -- one place decides how a key is obtained, whether the
// LLM call is a trial or an audit/fix subagent.
func (r *Runner) resolveLLM(ctx context.Context, name string) ([]string, error) {
	s, ok := r.LLMSpecs[name]
	if !ok {
		return nil, fmt.Errorf("llm_spec %q is not defined (add a kind: LLMSpec document)", name)
	}
	var key string
	switch {
	case s.APIKeyEnv != "":
		key = os.Getenv(s.APIKeyEnv)
		if key == "" {
			return nil, fmt.Errorf("llm_spec %s: %s is not set here", name, s.APIKeyEnv)
		}
	case len(s.APIKeyCmd) > 0:
		out, err := exec.CommandContext(ctx, s.APIKeyCmd[0], s.APIKeyCmd[1:]...).Output()
		if err != nil {
			return nil, fmt.Errorf("llm_spec %s: api_key_cmd: %w", name, err)
		}
		key = strings.TrimSpace(string(out))
		if key == "" {
			return nil, fmt.Errorf("llm_spec %s: api_key_cmd produced an empty key", name)
		}
	}
	return []string{
		"LLM_BASE_URL=" + s.BaseURL,
		"LLM_MODEL=" + s.Model,
		"LLM_PROVIDER=" + s.Provider,
		"LLM_API_KEY=" + key,
	}, nil
}

// verify reads the file named by uses: and checks the pin. A pin that does not
// match is a refusal, not a warning: the whole value of writing the hash down
// is that nobody has to notice a warning for it to work.
func verify(c config.Command) (string, error) {
	b, err := os.ReadFile(c.Uses)
	if err != nil {
		return "", fmt.Errorf("uses %s: %w", c.Uses, err)
	}
	// The file is executed directly, so it has to be executable and say what
	// runs it. Catching that here turns an opaque "exec format error" into the
	// one-line fix it actually is.
	if fi, err := os.Stat(c.Uses); err == nil && fi.Mode()&0o111 == 0 {
		return "", fmt.Errorf("uses %s: not executable (chmod +x %s)", c.Uses, c.Uses)
	}
	if len(b) > 2 && b[0] == '#' && b[1] != '!' {
		return "", fmt.Errorf("uses %s: no #! line; an adapter is run directly, "+
			"so it must name its own interpreter", c.Uses)
	}
	sum := fmt.Sprintf("%x", sha256.Sum256(b))
	if c.SHA256 != "" && !strings.EqualFold(c.SHA256, sum) {
		return "", fmt.Errorf("uses %s: sha256 is %s, pinned to %s -- refusing to run",
			c.Uses, sum[:12], strings.ToLower(c.SHA256)[:min(12, len(c.SHA256))])
	}
	return sum, nil
}

// Pin returns the sha256 of a uses: script, for the run manifest.
func Pin(c config.Command) (string, error) { return verify(c) }

// shell prefers bash: scripts in the wild are written with bashisms
// (set -o pipefail, [[ ]]), and dash fails them in ways that read as a
// storage-backend problem rather than a shell problem.
func shell() string {
	if p, err := exec.LookPath("bash"); err == nil {
		return p
	}
	return "sh"
}

func render(tpl string, vars map[string]string) (string, error) {
	if !strings.Contains(tpl, "{{") {
		return tpl, nil
	}
	t, err := template.New("c").Option("missingkey=error").Parse(tpl)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	if err := t.Execute(&b, vars); err != nil {
		return "", err
	}
	return b.String(), nil
}

// envPairs turns CamelCase template names into UPPER_SNAKE env vars.
func envPairs(vars map[string]string) []string {
	out := make([]string, 0, len(vars))
	for k, v := range vars {
		out = append(out, upperSnake(k)+"="+v)
	}
	return out
}

// upperSnake turns a CamelCase template name into the environment variable
// spelling. Acronyms stay whole: CaseSHA is CASE_SHA, not CASE_S_H_A, because
// the variable an adapter reads should be the one a person would guess.
//
// A separator goes before an upper-case letter only where a word actually
// begins: after a lower-case letter or digit, or at the end of a run of
// capitals that is followed by a lower-case letter (HTTPServer -> HTTP_SERVER).
func upperSnake(s string) string {
	r := []rune(s)
	var b strings.Builder
	for i, c := range r {
		if i > 0 && isUpper(c) {
			prev := r[i-1]
			startsWord := isLower(prev) || isDigit(prev)
			endsAcronym := isUpper(prev) && i+1 < len(r) && isLower(r[i+1])
			if startsWord || endsAcronym {
				b.WriteByte('_')
			}
		}
		if isLower(c) {
			c = c - 'a' + 'A'
		}
		b.WriteRune(c)
	}
	return b.String()
}

func isUpper(r rune) bool { return r >= 'A' && r <= 'Z' }
func isLower(r rune) bool { return r >= 'a' && r <= 'z' }
func isDigit(r rune) bool { return r >= '0' && r <= '9' }

func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return "..." + s[len(s)-n:]
}

// announce reports whether this command has not been named in the log yet.
func (r *Runner) announce(name string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.announced == nil {
		r.announced = map[string]bool{}
	}
	if r.announced[name] {
		return false
	}
	r.announced[name] = true
	return true
}

func (r *Runner) logf(f string, a ...any) {
	if r.Log != nil {
		r.Log(f, a...)
	}
}
