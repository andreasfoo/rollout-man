// Package exec runs the trial main chain: prepare -> agent -> verifier.
//
// Two executors share one interface. "docker" is the real one. "local" runs
// the same case scripts inside a private mount namespace with /app, /logs and
// /solution bind-mounted from a per-trial sandbox: no daemon required, and the
// absolute paths the case scripts hardcode still resolve.
package exec

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/andreasfoo/rollout-man/internal/casedef"
	"github.com/andreasfoo/rollout-man/internal/failure"
)

type AgentKind string

const (
	Oracle AgentKind = "oracle"
	Nop    AgentKind = "nop"
	LLM    AgentKind = "llm"
)

// LLMEnv is resolved on the runner. The key never leaves this struct.
type LLMEnv struct {
	BaseURL string
	Model   string
	APIKey  string
}

type AgentSpec struct {
	Kind AgentKind
	Name string
	LLM  *LLMEnv
	// Command runs a real agent; MVP ships oracle/nop only.
	Command []string
}

type CaseEnv struct {
	CaseDir string // unpacked case
	WorkDir string // per-trial working directory
	Cfg     *casedef.TaskConfig
	Image   string
}

func (e *CaseEnv) sandbox() string { return filepath.Join(e.WorkDir, "root") }
func (e *CaseEnv) OutDir() string  { return filepath.Join(e.WorkDir, "out") }

type Executor interface {
	Name() string
	Prepare(ctx context.Context, env *CaseEnv) error
	RunAgent(ctx context.Context, env *CaseEnv, a AgentSpec) error
	RunVerifier(ctx context.Context, env *CaseEnv) (float64, error)
	Collect(ctx context.Context, env *CaseEnv) error
	Cleanup(ctx context.Context, env *CaseEnv) error
}

func Pick(name string) (Executor, error) {
	switch name {
	case "", "auto":
		if dockerAvailable() {
			return &Docker{}, nil
		}
		return &Local{}, nil
	case "docker":
		return &Docker{}, nil
	case "local":
		return &Local{}, nil
	}
	return nil, fmt.Errorf("unknown executor %q", name)
}

func dockerAvailable() bool {
	c := exec.Command("docker", "info")
	c.Stdout, c.Stderr = nil, nil
	return c.Run() == nil
}

// ---------------------------------------------------------------- local ---

type Local struct{}

func (l *Local) Name() string { return "local" }

func (l *Local) Prepare(ctx context.Context, env *CaseEnv) error {
	sb := env.sandbox()
	for _, d := range []string{"app", "app/seed", "logs/verifier", "logs/agent", "solution", "tmp"} {
		if err := os.MkdirAll(filepath.Join(sb, d), 0o777); err != nil {
			return failure.Wrap(failure.HostError, "create sandbox", err)
		}
	}
	if err := os.MkdirAll(env.OutDir(), 0o755); err != nil {
		return failure.Wrap(failure.HostError, "create out dir", err)
	}
	if err := copyTree(filepath.Join(env.CaseDir, "solution"), filepath.Join(sb, "solution")); err != nil {
		return failure.Wrap(failure.ContainerStart, "stage solution", err)
	}
	// Seed material is the agent-visible clue set; the case Dockerfile copies it
	// into /app/seed, so mirror that.
	_ = copyTree(filepath.Join(env.CaseDir, "environment", "seed"), filepath.Join(sb, "app", "seed"))
	return nil
}

func (l *Local) RunAgent(ctx context.Context, env *CaseEnv, a AgentSpec) error {
	switch a.Kind {
	case Nop:
		return writeFile(filepath.Join(env.sandbox(), "logs", "agent", "stdout.log"), "nop: no action taken\n")
	case Oracle:
		script := filepath.Join(env.CaseDir, "solution", "solve.sh")
		if _, err := os.Stat(script); err != nil {
			return failure.Wrap(failure.ContainerStart, "case has no solution/solve.sh", err)
		}
		out, code, err := l.shell(ctx, env, "/solution/solve.sh", env.Cfg.AgentTimeout, nil)
		writeFile(filepath.Join(env.sandbox(), "logs", "agent", "stdout.log"), out)
		if err != nil {
			if errors.Is(err, context.DeadlineExceeded) {
				return failure.New(failure.AgentTimeout, "oracle exceeded agent timeout")
			}
			return failure.Wrap(failure.HostError, "run oracle", err)
		}
		if code != 0 {
			return failure.New(failure.AgentExitNonzero, fmt.Sprintf("oracle exited %d", code))
		}
		return nil
	case LLM:
		if len(a.Command) == 0 {
			return failure.New(failure.ContainerStart,
				"no agent command configured for "+a.Name+" (MVP ships oracle/nop only)")
		}
		envv := map[string]string{}
		if a.LLM != nil {
			envv["LLM_BASE_URL"] = a.LLM.BaseURL
			envv["LLM_MODEL"] = a.LLM.Model
			envv["LLM_API_KEY"] = a.LLM.APIKey
		}
		out, code, err := l.shell(ctx, env, quoteArgv(a.Command), env.Cfg.AgentTimeout, envv)
		writeFile(filepath.Join(env.sandbox(), "logs", "agent", "stdout.log"), out)
		if err != nil {
			if errors.Is(err, context.DeadlineExceeded) {
				return failure.New(failure.AgentTimeout, "agent exceeded timeout")
			}
			return failure.Wrap(failure.HostError, "run agent", err)
		}
		if code != 0 {
			return failure.New(failure.AgentExitNonzero, fmt.Sprintf("agent exited %d", code))
		}
		return nil
	}
	return failure.New(failure.InternalError, "unknown agent kind")
}

func (l *Local) RunVerifier(ctx context.Context, env *CaseEnv) (float64, error) {
	script := filepath.Join(env.CaseDir, "tests", "test.sh")
	if _, err := os.Stat(script); err != nil {
		return 0, failure.Wrap(failure.VerifierError, "case has no tests/test.sh", err)
	}
	out, code, err := l.shell(ctx, env, "/tests/test.sh", env.Cfg.VerifierTimeout, nil)
	writeFile(filepath.Join(env.sandbox(), "logs", "verifier", "verifier.log"), out)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return 0, failure.New(failure.VerifierTimeout, "verifier exceeded timeout")
		}
		return 0, failure.Wrap(failure.VerifierError, "run verifier", err)
	}
	reward, rerr := readReward(filepath.Join(env.sandbox(), "logs", "verifier", "reward.txt"))
	if rerr != nil {
		return 0, failure.Wrap(failure.VerifierError,
			fmt.Sprintf("verifier exited %d without a usable reward", code), rerr)
	}
	return reward, nil
}

// shell runs cmd inside a private mount namespace where the sandbox dirs are
// bound onto the absolute paths the case scripts expect.
func (l *Local) shell(ctx context.Context, env *CaseEnv, cmd string, timeout time.Duration, extraEnv map[string]string) (string, int, error) {
	if timeout <= 0 {
		timeout = 30 * time.Minute
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	sb := env.sandbox()
	script := strings.Join([]string{
		"set -e",
		"mkdir -p /app /logs /solution /tests",
		"mount --bind " + shq(filepath.Join(sb, "app")) + " /app",
		"mount --bind " + shq(filepath.Join(sb, "logs")) + " /logs",
		"mount --bind " + shq(filepath.Join(sb, "solution")) + " /solution",
		"mount --bind " + shq(filepath.Join(env.CaseDir, "tests")) + " /tests",
		"cd /app",
		"set +e",
		cmd,
		"echo \"__RM_EXIT__:$?\"",
	}, "\n")

	c := exec.CommandContext(ctx, "unshare", "--mount", "--propagation", "private", shell(), "-c", script)
	// Deliberately NOT os.Environ(): the case scripts are untrusted and their
	// output is an artifact, so the host environment must not reach them.
	c.Env = []string{
		"HOME=/root",
		"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
		"TERM=dumb",
		"LANG=C.UTF-8",
	}
	for k, v := range extraEnv {
		c.Env = append(c.Env, k+"="+v)
	}
	raw, err := c.CombinedOutput()
	out := string(raw)
	if ctx.Err() != nil {
		return out, -1, ctx.Err()
	}
	code := 0
	if i := strings.LastIndex(out, "__RM_EXIT__:"); i >= 0 {
		rest := out[i+len("__RM_EXIT__:"):]
		if j := strings.IndexAny(rest, "\r\n"); j >= 0 {
			rest = rest[:j]
		}
		code, _ = strconv.Atoi(strings.TrimSpace(rest))
		out = out[:i]
	} else if err != nil {
		return out, -1, err
	}
	return out, code, nil
}

func (l *Local) Collect(ctx context.Context, env *CaseEnv) error {
	sb := env.sandbox()
	out := env.OutDir()
	copyIfExists(filepath.Join(sb, "logs", "agent", "agent.log"), filepath.Join(out, "agent.log"))
	copyIfExists(filepath.Join(sb, "logs", "agent", "stdout.log"), filepath.Join(out, "stdout.log"))
	copyIfExists(filepath.Join(sb, "logs", "verifier", "verifier.log"), filepath.Join(out, "verifier.log"))
	copyIfExists(filepath.Join(sb, "logs", "agent", "traj.jsonl"), filepath.Join(out, "traj.jsonl"))
	if _, err := os.Stat(filepath.Join(out, "traj.jsonl")); err != nil {
		writeFile(filepath.Join(out, "traj.jsonl"), "")
	}
	return nil
}

func (l *Local) Cleanup(ctx context.Context, env *CaseEnv) error {
	return os.RemoveAll(env.sandbox())
}

// --------------------------------------------------------------- docker ---

type Docker struct{}

func (d *Docker) Name() string { return "docker" }

func (d *Docker) Prepare(ctx context.Context, env *CaseEnv) error {
	ctx, cancel := context.WithTimeout(ctx, env.Cfg.BuildTimeout)
	defer cancel()
	env.Image = "rollout-man/case:" + filepath.Base(env.WorkDir)
	c := exec.CommandContext(ctx, "docker", "build", "-t", env.Image,
		"-f", filepath.Join(env.CaseDir, "environment", "Dockerfile"),
		filepath.Join(env.CaseDir, "environment"))
	if out, err := c.CombinedOutput(); err != nil {
		if ctx.Err() != nil {
			return failure.New(failure.EnvironmentTimeout, "image build exceeded build_timeout_sec")
		}
		return failure.Wrap(failure.ImageBuildFailed, tail(string(out), 4000), err)
	}
	if err := os.MkdirAll(env.OutDir(), 0o755); err != nil {
		return failure.Wrap(failure.HostError, "create out dir", err)
	}
	return nil
}

func (d *Docker) container(env *CaseEnv) string { return "rm-" + filepath.Base(env.WorkDir) }

func (d *Docker) RunAgent(ctx context.Context, env *CaseEnv, a AgentSpec) error {
	if a.Kind == Nop {
		return nil
	}
	args := []string{"run", "--rm", "--name", d.container(env) + "-agent",
		"-v", filepath.Join(env.CaseDir, "solution") + ":/solution:ro",
		"-v", env.WorkDir + "/state:/state",
		"--cpus", strconv.Itoa(max(1, env.Cfg.Resources.CPUs)),
		"--memory", strconv.Itoa(max(512, env.Cfg.Resources.MemoryMB)) + "m",
		"-u", env.Cfg.AgentUser,
	}
	if a.LLM != nil {
		args = append(args, "-e", "LLM_BASE_URL="+a.LLM.BaseURL, "-e", "LLM_MODEL="+a.LLM.Model,
			"-e", "LLM_API_KEY="+a.LLM.APIKey)
	}
	cmd := []string{"bash", "-lc", "/solution/solve.sh"}
	if a.Kind == LLM {
		if len(a.Command) == 0 {
			return failure.New(failure.ContainerStart, "no agent command configured for "+a.Name)
		}
		cmd = a.Command
	}
	args = append(args, env.Image)
	args = append(args, cmd...)
	return d.run(ctx, env, args, env.Cfg.AgentTimeout, failure.AgentTimeout, failure.AgentExitNonzero)
}

func (d *Docker) RunVerifier(ctx context.Context, env *CaseEnv) (float64, error) {
	args := []string{"run", "--rm", "--name", d.container(env) + "-verify",
		"-v", filepath.Join(env.CaseDir, "tests") + ":/tests:ro",
		"-v", env.WorkDir + "/state:/state",
		"-u", env.Cfg.VerifierUser, env.Image, "bash", "-lc", "/tests/test.sh"}
	err := d.run(ctx, env, args, env.Cfg.VerifierTimeout, failure.VerifierTimeout, failure.VerifierError)
	if err != nil {
		return 0, err
	}
	return readReward(filepath.Join(env.WorkDir, "state", "reward.txt"))
}

func (d *Docker) run(ctx context.Context, env *CaseEnv, args []string, timeout time.Duration, tCode, xCode failure.Code) error {
	if timeout <= 0 {
		timeout = 30 * time.Minute
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	c := exec.CommandContext(ctx, "docker", args...)
	out, err := c.CombinedOutput()
	writeFile(filepath.Join(env.OutDir(), "stdout.log"), string(out))
	if ctx.Err() != nil {
		return failure.New(tCode, "exceeded timeout")
	}
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return failure.New(xCode, fmt.Sprintf("exited %d", ee.ExitCode()))
		}
		return failure.Wrap(failure.DockerError, "docker run", err)
	}
	return nil
}

func (d *Docker) Collect(ctx context.Context, env *CaseEnv) error {
	src := filepath.Join(env.WorkDir, "state")
	for _, n := range []string{"agent.log", "verifier.log", "traj.jsonl"} {
		copyIfExists(filepath.Join(src, n), filepath.Join(env.OutDir(), n))
	}
	return nil
}

func (d *Docker) Cleanup(ctx context.Context, env *CaseEnv) error {
	if env.Image != "" {
		_ = exec.Command("docker", "rmi", "-f", env.Image).Run()
	}
	return nil
}

// ---------------------------------------------------------------- utils ---

func readReward(path string) (float64, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	v, err := strconv.ParseFloat(strings.TrimSpace(string(b)), 64)
	if err != nil {
		return 0, failure.Wrap(failure.InvalidReward, "reward.txt is not a number", err)
	}
	if v < 0 || v > 1 {
		return 0, failure.New(failure.InvalidReward, "reward out of range: "+strconv.FormatFloat(v, 'f', -1, 64))
	}
	return v, nil
}

func writeFile(path, content string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(content), 0o644)
}

func copyIfExists(src, dst string) {
	b, err := os.ReadFile(src)
	if err != nil {
		return
	}
	writeFile(dst, string(b))
}

func copyTree(src, dst string) error {
	fi, err := os.Stat(src)
	if err != nil {
		return err
	}
	if !fi.IsDir() {
		b, err := os.ReadFile(src)
		if err != nil {
			return err
		}
		return writeFile(dst, string(b))
	}
	return filepath.Walk(src, func(p string, fi os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		target := filepath.Join(dst, rel)
		if fi.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		return os.WriteFile(target, b, fi.Mode().Perm())
	})
}

func shell() string {
	if p, err := exec.LookPath("bash"); err == nil {
		return p
	}
	return "sh"
}

// Command builds an exec.Cmd from argv.
func Command(ctx context.Context, argv []string) *exec.Cmd {
	return exec.CommandContext(ctx, argv[0], argv[1:]...)
}

// ScriptCommand wraps a shell script as argv using the same shell the
// executor itself uses.
func ScriptCommand(script string) []string { return []string{shell(), "-c", script} }

// quoteArgv renders argv as a shell command without losing word boundaries.
func quoteArgv(argv []string) string {
	parts := make([]string, len(argv))
	for i, a := range argv {
		parts[i] = shq(a)
	}
	return strings.Join(parts, " ")
}

func shq(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return "..." + s[len(s)-n:]
}
