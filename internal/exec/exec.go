// Package exec runs one trial and returns its reward.
//
// It does NOT run containers. Building the case image, starting it, running the
// agent inside it and running the verifier are Harbor's job -- this package's
// job is to ask Harbor for a trial and read the answer back. That split is the
// whole reason the two systems exist separately (design §1.2/§1.3), and the
// adapter is a command the operator configures, exactly like storage is.
//
// "local" is the exception, and it is a test fixture, not a runtime: it runs
// the same case scripts in a private mount namespace so the orchestration can
// be exercised on a machine with no Harbor and no daemon.
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
	Provider string // names the env vars the agent expects (anthropic, openai, ...)
	BaseURL  string
	Model    string
	APIKey   string
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
}

func (e *CaseEnv) TrialID() string { return filepath.Base(e.WorkDir) }

func (e *CaseEnv) sandbox() string { return filepath.Join(e.WorkDir, "root") }
func (e *CaseEnv) OutDir() string  { return filepath.Join(e.WorkDir, "out") }

// Executor runs one trial to a number. Everything between -- build, agent,
// verifier, artifacts -- belongs to the implementation, because for the real
// one it belongs to Harbor.
type Executor interface {
	Name() string
	Trial(ctx context.Context, env *CaseEnv, a AgentSpec) (float64, error)
}

// ---------------------------------------------------------------- local ---

type Local struct{}

func (l *Local) Name() string { return "local" }

// Trial is the whole chain. Artifacts are collected on every exit: a trial you
// cannot read is a trial you cannot fix, and the failed ones are the ones you
// need to read.
func (l *Local) Trial(ctx context.Context, env *CaseEnv, a AgentSpec) (float64, error) {
	defer l.cleanup(env)
	if err := l.prepare(env); err != nil {
		return 0, err
	}
	defer l.collect(env)
	if err := l.runAgent(ctx, env, a); err != nil {
		return 0, err
	}
	return l.runVerifier(ctx, env)
}

func (l *Local) prepare(env *CaseEnv) error {
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

func (l *Local) runAgent(ctx context.Context, env *CaseEnv, a AgentSpec) error {
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

func (l *Local) runVerifier(ctx context.Context, env *CaseEnv) (float64, error) {
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

func (l *Local) collect(env *CaseEnv) {
	sb := env.sandbox()
	out := env.OutDir()
	copyIfExists(filepath.Join(sb, "logs", "agent", "agent.log"), filepath.Join(out, "agent.log"))
	copyIfExists(filepath.Join(sb, "logs", "agent", "stdout.log"), filepath.Join(out, "stdout.log"))
	copyIfExists(filepath.Join(sb, "logs", "verifier", "verifier.log"), filepath.Join(out, "verifier.log"))
	copyIfExists(filepath.Join(sb, "logs", "agent", "traj.jsonl"), filepath.Join(out, "traj.jsonl"))
	if _, err := os.Stat(filepath.Join(out, "traj.jsonl")); err != nil {
		writeFile(filepath.Join(out, "traj.jsonl"), "")
	}
}

func (l *Local) cleanup(env *CaseEnv) { _ = os.RemoveAll(env.sandbox()) }

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
