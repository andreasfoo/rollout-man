package exec

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/andreasfoo/rollout-man/internal/fail"
)

// CommandFunc runs one configured command to completion. It is supplied by the
// caller so this package never learns how commands are configured.
type CommandFunc func(ctx context.Context, name string, vars map[string]string, timeout time.Duration) error

// Harbor hands the whole trial to one command and reads the answer back off
// disk. Everything the command needs arrives as environment variables; nothing
// about how it runs the case is our business.
//
// The contract, in both directions:
//
//	in   CASE_DIR, OUT_DIR, AGENT_KIND/AGENT_NAME/AGENT_COMMAND, the LLM
//	     endpoint, and every limit task.toml declares
//	out  $OUT_DIR/reward.txt   the score, and the only thing that means "measured"
//	     $OUT_DIR/failure.txt  optional: a failure code, when the adapter knows why
//	     $OUT_DIR/*            whatever artifacts should ship (traj.jsonl, logs)
type Harbor struct {
	Cmd string      // the configured command's name
	Run CommandFunc // how to run it
}

func (h *Harbor) Name() string { return h.Cmd }

func (h *Harbor) Trial(ctx context.Context, env *CaseEnv, a AgentSpec) (float64, error) {
	out := env.OutDir()
	if err := os.MkdirAll(out, 0o755); err != nil {
		return 0, fail.Wrap(fail.Host, "create out dir", err)
	}
	c := env.Cfg
	vars := map[string]string{
		"TrialId":  env.TrialID(),
		"CaseDir":  env.CaseDir,
		"CaseName": c.Name,
		"WorkDir":  env.WorkDir,
		"OutDir":   out,

		"AgentKind":    string(a.Kind),
		"AgentName":    a.Name,
		"AgentCommand": quoteArgv(a.Command),

		// Straight out of task.toml: the adapter enforces them, because the
		// thing that can actually stop a running agent is the thing that
		// started it.
		"AgentUser":          c.AgentUser,
		"VerifierUser":       c.VerifierUser,
		"AgentTimeoutSec":    secs(c.AgentTimeout),
		"VerifierTimeoutSec": secs(c.VerifierTimeout),
		"BuildTimeoutSec":    secs(c.BuildTimeout),
		"Cpus":               strconv.Itoa(c.Resources.CPUs),
		"MemoryMb":           strconv.Itoa(c.Resources.MemoryMB),
		"StorageMb":          strconv.Itoa(c.Resources.StorageMB),
		"Gpus":               strconv.Itoa(c.Resources.GPUs),
		"AllowInternet":      boolStr(c.AllowInternet),

		"LlmProvider": "",
		"LlmBaseUrl":  "",
		"LlmModel":    "",
		"LlmApiKey":   "",
	}
	if a.LLM != nil {
		vars["LlmProvider"] = a.LLM.Provider
		vars["LlmBaseUrl"], vars["LlmModel"], vars["LlmApiKey"] = a.LLM.BaseURL, a.LLM.Model, a.LLM.APIKey
	}

	// Our own clock is only a backstop: the per-step limits above are the real
	// ones and the adapter owns them. Cutting the command off early would turn
	// a measured trial into an unmeasured one.
	budget := c.BuildTimeout + c.AgentTimeout + c.VerifierTimeout + 5*time.Minute
	err := h.Run(ctx, h.Cmd, vars, budget)
	return h.score(out, err)
}

// score decides what happened, preferring the most specific evidence:
// a declared failure code, then a number, then the exit status.
func (h *Harbor) score(out string, runErr error) (float64, error) {
	if code, msg, ok := readFailure(out); ok {
		if !code.Known() {
			// Our configuration is wrong, not the agent and not the case.
			return 0, fail.New(fail.Host, "adapter reported unknown failure code "+string(code))
		}
		return 0, fail.New(code, msg)
	}
	reward, rerr := readReward(filepath.Join(out, "reward.txt"))
	if rerr == nil {
		return reward, nil
	}
	if runErr != nil {
		// It broke and did not say why. "Could not measure" is not the agent's
		// failure, so this must never land in the agent's denominator.
		return 0, fail.Wrap(fail.EnvFailed, "trial command failed and wrote no failure.txt", runErr)
	}
	return 0, fail.Wrap(fail.VerifierBad, "trial command exited clean but wrote no reward.txt", rerr)
}

// readFailure reads $OUT_DIR/failure.txt: the code on the first line, an
// optional human explanation on the rest.
func readFailure(out string) (fail.Code, string, bool) {
	b, err := os.ReadFile(filepath.Join(out, "failure.txt"))
	if err != nil {
		return "", "", false
	}
	text := strings.TrimSpace(string(b))
	if text == "" {
		return "", "", false
	}
	code, msg, _ := strings.Cut(text, "\n")
	code = strings.TrimSpace(code)
	msg = strings.TrimSpace(msg)
	if msg == "" {
		msg = "reported by " + filepath.Base(out) + " adapter"
	}
	return fail.Code(code), msg, true
}

func secs(d time.Duration) string {
	return strconv.Itoa(int(d.Seconds()))
}

func boolStr(b bool) string {
	if b {
		return "1"
	}
	return "0"
}
