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
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/andreasfoo/rollout-man/internal/casedef"
	"github.com/andreasfoo/rollout-man/internal/fail"
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

	// Per-trial docker state. It lives here and not on Docker because one
	// Executor value serves every trial in the run, concurrently.
	container  string
	hasTimeout bool
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
			return fail.Wrap(fail.Host, "create sandbox", err)
		}
	}
	if err := os.MkdirAll(env.OutDir(), 0o755); err != nil {
		return fail.Wrap(fail.Host, "create out dir", err)
	}
	if err := copyTree(filepath.Join(env.CaseDir, "solution"), filepath.Join(sb, "solution")); err != nil {
		return fail.Wrap(fail.EnvFailed, "stage solution", err)
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
			return fail.Wrap(fail.EnvFailed, "case has no solution/solve.sh", err)
		}
		out, code, err := l.shell(ctx, env, "/solution/solve.sh", env.Cfg.AgentTimeout, nil)
		writeFile(filepath.Join(env.sandbox(), "logs", "agent", "stdout.log"), out)
		if err != nil {
			if errors.Is(err, context.DeadlineExceeded) {
				return fail.New(fail.AgentTimeout, "oracle exceeded agent timeout")
			}
			return fail.Wrap(fail.Host, "run oracle", err)
		}
		if code != 0 {
			return fail.New(fail.AgentFailed, fmt.Sprintf("oracle exited %d", code))
		}
		return nil
	case LLM:
		if len(a.Command) == 0 {
			return fail.New(fail.EnvFailed,
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
				return fail.New(fail.AgentTimeout, "agent exceeded timeout")
			}
			return fail.Wrap(fail.Host, "run agent", err)
		}
		if code != 0 {
			return fail.New(fail.AgentFailed, fmt.Sprintf("agent exited %d", code))
		}
		return nil
	}
	return fail.New(fail.Host, "unknown agent kind")
}

func (l *Local) RunVerifier(ctx context.Context, env *CaseEnv) (float64, error) {
	script := filepath.Join(env.CaseDir, "tests", "test.sh")
	if _, err := os.Stat(script); err != nil {
		return 0, fail.Wrap(fail.VerifierBad, "case has no tests/test.sh", err)
	}
	out, code, err := l.shell(ctx, env, "/tests/test.sh", env.Cfg.VerifierTimeout, nil)
	writeFile(filepath.Join(env.sandbox(), "logs", "verifier", "verifier.log"), out)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return 0, fail.New(fail.VerifierBad, "verifier exceeded timeout")
		}
		return 0, fail.Wrap(fail.VerifierBad, "run verifier", err)
	}
	reward, rerr := readReward(filepath.Join(env.sandbox(), "logs", "verifier", "reward.txt"))
	if rerr != nil {
		return 0, fail.Wrap(fail.VerifierBad,
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
	// The case script runs children (a sleep, a build) that outlive a kill of
	// unshare itself, and CombinedOutput waits on the pipe until every writer
	// is gone -- so without a process-group kill an "agent timeout" would only
	// be noticed once the agent finished anyway. Setpgid + kill(-pgid) ends the
	// whole tree; WaitDelay is the backstop for anything that ignores it.
	c.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	c.Cancel = func() error { return syscall.Kill(-c.Process.Pid, syscall.SIGKILL) }
	c.WaitDelay = 10 * time.Second
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

// Docker keeps ONE container alive for the whole trial and runs each step with
// docker exec. That is not an optimisation: the agent's deliverable lives at
// /app/crash.bin and the verifier's score lands in /logs/verifier/reward.txt,
// so agent and verifier have to see the same filesystem. Two `docker run --rm`
// invocations would throw the deliverable away between them.
type Docker struct{}

func (d *Docker) Name() string { return "docker" }

func (d *Docker) Prepare(ctx context.Context, env *CaseEnv) error {
	if err := os.MkdirAll(env.OutDir(), 0o755); err != nil {
		return fail.Wrap(fail.Host, "create out dir", err)
	}
	env.Image = "rollout-man/case:" + strings.ToLower(filepath.Base(env.WorkDir))

	bctx, cancel := context.WithTimeout(ctx, env.Cfg.BuildTimeout)
	defer cancel()
	build := exec.CommandContext(bctx, "docker", "build", "-t", env.Image,
		"-f", filepath.Join(env.CaseDir, "environment", "Dockerfile"),
		filepath.Join(env.CaseDir, "environment"))
	if out, err := build.CombinedOutput(); err != nil {
		writeFile(filepath.Join(env.OutDir(), "build.log"), string(out))
		if bctx.Err() != nil {
			return fail.New(fail.EnvFailed, "image build exceeded build_timeout_sec")
		}
		return fail.Wrap(fail.EnvFailed, tail(string(out), 4000), err)
	}

	env.container = "rm-" + strings.ToLower(filepath.Base(env.WorkDir))
	args := []string{"run", "-d", "--name", env.container,
		"--entrypoint", "/bin/sh",
		"-v", filepath.Join(env.CaseDir, "solution") + ":/solution:ro",
		"-v", filepath.Join(env.CaseDir, "tests") + ":/tests:ro",
		"--cpus", strconv.Itoa(max(1, env.Cfg.Resources.CPUs)),
		"--memory", strconv.Itoa(max(512, env.Cfg.Resources.MemoryMB)) + "m",
		"-w", "/app",
	}
	if !env.Cfg.AllowInternet {
		args = append(args, "--network", "none")
	}
	// `sleep infinity` is GNU-only; the loop works on busybox images too.
	args = append(args, env.Image, "-c", "while :; do sleep 3600; done")
	if out, err := exec.CommandContext(ctx, "docker", args...).CombinedOutput(); err != nil {
		return fail.Wrap(fail.EnvFailed, "start case container: "+tail(string(out), 2000), err)
	}

	// The case Dockerfile is supposed to create these, but a case that forgot
	// would otherwise fail as VerifierBad ("no reward") instead of EnvFailed.
	if out, err := d.exec(ctx, env, "root", "mkdir -p /app /logs/agent /logs/verifier"); err != nil {
		return fail.Wrap(fail.EnvFailed, "prepare log dirs: "+tail(out, 1000), err)
	}
	_, terr := d.exec(ctx, env, "root", "command -v timeout >/dev/null")
	env.hasTimeout = terr == nil
	return nil
}

func (d *Docker) RunAgent(ctx context.Context, env *CaseEnv, a AgentSpec) error {
	var cmd string
	var envv map[string]string
	switch a.Kind {
	case Nop:
		return writeFile(filepath.Join(env.OutDir(), "stdout.log"), "nop: no action taken\n")
	case Oracle:
		cmd = "/solution/solve.sh"
	case LLM:
		if len(a.Command) == 0 {
			return fail.New(fail.EnvFailed,
				"no agent command configured for "+a.Name+" (MVP ships oracle/nop only)")
		}
		cmd = quoteArgv(a.Command)
		if a.LLM != nil {
			envv = map[string]string{
				"LLM_BASE_URL": a.LLM.BaseURL,
				"LLM_MODEL":    a.LLM.Model,
				"LLM_API_KEY":  a.LLM.APIKey,
			}
		}
	default:
		return fail.New(fail.Host, "unknown agent kind")
	}
	out, err := d.step(ctx, env, env.Cfg.AgentUser, cmd, env.Cfg.AgentTimeout, envv,
		fail.AgentTimeout, fail.AgentFailed)
	writeFile(filepath.Join(env.OutDir(), "stdout.log"), out)
	return err
}

func (d *Docker) RunVerifier(ctx context.Context, env *CaseEnv) (float64, error) {
	out, err := d.step(ctx, env, env.Cfg.VerifierUser, "/tests/test.sh",
		env.Cfg.VerifierTimeout, nil, fail.VerifierBad, fail.VerifierBad)
	writeFile(filepath.Join(env.OutDir(), "verifier.stdout.log"), out)
	if err != nil {
		return 0, err
	}
	// The score is whatever the case wrote inside the container, at the path
	// the Harbor contract fixes -- never a path we invented on the host.
	raw, cerr := d.exec(ctx, env, "root", "cat /logs/verifier/reward.txt")
	if cerr != nil {
		return 0, fail.New(fail.VerifierBad, "verifier wrote no /logs/verifier/reward.txt")
	}
	return parseReward(raw)
}

// step runs one chain step inside the live container. The timeout is enforced
// *inside* the container when coreutils timeout(1) is there, because killing
// the `docker exec` client on the host leaves the process running in the
// container -- and that process would keep writing into the next step.
func (d *Docker) step(ctx context.Context, env *CaseEnv, user, cmd string, timeout time.Duration,
	extraEnv map[string]string, tCode, xCode fail.Code) (string, error) {
	if timeout <= 0 {
		timeout = 30 * time.Minute
	}
	inner := "exec /bin/bash -lc " + shq(cmd)
	if env.hasTimeout {
		inner = "exec timeout -k 10 " + strconv.Itoa(int(timeout.Seconds())) +
			" /bin/bash -lc " + shq(cmd)
	}
	// Host-side backstop: a container whose timeout(1) is missing, or whose
	// process ignores SIGKILL delivery, still has to end this step.
	hctx, cancel := context.WithTimeout(ctx, timeout+30*time.Second)
	defer cancel()

	args := []string{"exec", "-u", user, "-w", "/app"}
	for _, k := range sortedKeys(extraEnv) {
		args = append(args, "-e", k+"="+extraEnv[k])
	}
	args = append(args, env.container, "/bin/sh", "-c", inner)

	raw, err := exec.CommandContext(hctx, "docker", args...).CombinedOutput()
	out := string(raw)
	if hctx.Err() != nil {
		return out, fail.New(tCode, "exceeded timeout")
	}
	if err == nil {
		return out, nil
	}
	var ee *exec.ExitError
	if !errors.As(err, &ee) {
		return out, fail.Wrap(fail.Host, "docker exec", err)
	}
	if env.hasTimeout && ee.ExitCode() == 124 {
		return out, fail.New(tCode, "exceeded timeout")
	}
	return out, fail.New(xCode, fmt.Sprintf("exited %d", ee.ExitCode()))
}

// exec is for the runner's own housekeeping, not for case code: no timeout
// wrapper, no per-case user, output returned raw.
func (d *Docker) exec(ctx context.Context, env *CaseEnv, user, cmd string) (string, error) {
	c := exec.CommandContext(ctx, "docker", "exec", "-u", user, env.container, "/bin/sh", "-c", cmd)
	out, err := c.CombinedOutput()
	return string(out), err
}

func (d *Docker) Collect(ctx context.Context, env *CaseEnv) error {
	if env.container == "" {
		return nil
	}
	for _, f := range []struct{ in, out string }{
		{"/logs/agent/agent.log", "agent.log"},
		{"/logs/agent/traj.jsonl", "traj.jsonl"},
		{"/logs/verifier/verifier.log", "verifier.log"},
		{"/logs/verifier/reward.txt", "reward.txt"},
	} {
		cp := exec.CommandContext(ctx, "docker", "cp",
			env.container+":"+f.in, filepath.Join(env.OutDir(), f.out))
		cp.Stdout, cp.Stderr = nil, nil
		_ = cp.Run()
	}
	if _, err := os.Stat(filepath.Join(env.OutDir(), "traj.jsonl")); err != nil {
		writeFile(filepath.Join(env.OutDir(), "traj.jsonl"), "")
	}
	return nil
}

func (d *Docker) Cleanup(ctx context.Context, env *CaseEnv) error {
	if env.container != "" {
		_ = exec.Command("docker", "rm", "-f", env.container).Run()
		env.container = ""
	}
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
	return parseReward(string(b))
}

func parseReward(raw string) (float64, error) {
	v, err := strconv.ParseFloat(strings.TrimSpace(raw), 64)
	if err != nil {
		return 0, fail.Wrap(fail.VerifierBad, "reward.txt is not a number", err)
	}
	if v < 0 || v > 1 {
		return 0, fail.New(fail.VerifierBad, "reward out of range: "+strconv.FormatFloat(v, 'f', -1, 64))
	}
	return v, nil
}

func sortedKeys(m map[string]string) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
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
