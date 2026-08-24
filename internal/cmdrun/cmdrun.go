// Package cmdrun executes the configured external commands. A command is
// either an argv template ({{.Key}}) or an sh script reading upper-case
// environment variables. Exit code zero means success.
package cmdrun

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"os/exec"
	"strings"
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
}

func New(c config.Commands) *Runner {
	r := &Runner{Cmds: c.Cmds, Timeout: c.Timeout.D(), MaxAttempts: c.MaxAttempts,
		InheritEnv: c.Inherits()}
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
		r.logf("command %s -> %s (sha256 %s)", name, c.Uses, sum[:12])
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

	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		return Result{}, fmt.Errorf("%w: %s", err, strings.TrimSpace(tail(errb.String(), 2000)))
	}
	return Result{Stdout: out.String(), Stderr: errb.String()}, nil
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

func upperSnake(s string) string {
	var b strings.Builder
	for i, r := range s {
		if i > 0 && r >= 'A' && r <= 'Z' {
			b.WriteByte('_')
		}
		if r >= 'a' && r <= 'z' {
			r = r - 'a' + 'A'
		}
		b.WriteRune(r)
	}
	return b.String()
}

func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return "..." + s[len(s)-n:]
}

func (r *Runner) logf(f string, a ...any) {
	if r.Log != nil {
		r.Log(f, a...)
	}
}
