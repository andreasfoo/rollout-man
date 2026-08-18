// Package cmdrun executes the configured external commands. A command is
// either an argv template ({{.Key}}) or an sh script reading upper-case
// environment variables. Exit code zero means success.
package cmdrun

import (
	"bytes"
	"context"
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
	Log         func(format string, args ...any)
}

func New(c config.Commands) *Runner {
	r := &Runner{Cmds: c.Cmds, Timeout: c.Timeout.D(), MaxAttempts: c.MaxAttempts}
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
		res, err := r.once(ctx, name, c, vars)
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

func (r *Runner) once(ctx context.Context, name string, c config.Command, vars map[string]string) (Result, error) {
	ctx, cancel := context.WithTimeout(ctx, r.Timeout)
	defer cancel()

	var cmd *exec.Cmd
	switch {
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
	cmd.Env = append(os.Environ(), envPairs(vars)...)

	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		return Result{}, fmt.Errorf("%w: %s", err, strings.TrimSpace(tail(errb.String(), 2000)))
	}
	return Result{Stdout: out.String(), Stderr: errb.String()}, nil
}

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
