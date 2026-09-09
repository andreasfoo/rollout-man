package cmdrun

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/andreasfoo/rollout-man/internal/config"
)

// TestWithVarsDoNotClobberPathOrHome pins the 2026-09-03 fix: a command
// invoked with a vars map carrying "path" (ship's with: {path: <tree>}) must
// not end up with PATH=<that tree>. envPairs upperSnakes "path" to PATH and
// once() appended it after the host PATH, so the adapter's
// `#!/usr/bin/env bash` failed with exit 127 ("bash not found") on five
// batch3 ships.
func TestWithVarsDoNotClobberPathOrHome(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "adapter.sh")
	if err := os.WriteFile(script, []byte("#!/usr/bin/env bash\nenv | grep -E '^(PATH|HOME)=' | sort\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	// A PATH/HOME distinct from the test process's own, so the assertions
	// cannot pass by inheritance.
	hostPath := "/usr/bin:/bin"
	hostHome := "/root"
	t.Setenv("PATH", hostPath)
	t.Setenv("HOME", hostHome)

	r := &Runner{
		Cmds: map[string]config.Command{
			"probe": {Uses: script},
		},
		Timeout: 30 * time.Second,
	}
	// The vars a ship step forwards: with.path must NOT become PATH.
	vars := map[string]string{
		"path":    "/srv/materialized/trajectory/case",
		"dest":    "tinglydev/cyber-xianjin",
		"home":    "/srv/definitely-not-home",
		"hf_rev":  "week3",
	}
	res, err := r.RunOnce(t.Context(), "probe", vars, 0)
	if err != nil {
		t.Fatalf("probe failed: %v", err)
	}
	stdout := res.Stdout
	if !containsLine(stdout, "PATH="+hostPath) {
		t.Errorf("PATH clobbered by with.path; stdout:\n%s", stdout)
	}
	if !containsLine(stdout, "HOME="+hostHome) {
		t.Errorf("HOME clobbered by with.home; stdout:\n%s", stdout)
	}
}

func containsLine(out, want string) bool {
	for _, line := range splitLines(out) {
		if line == want {
			return true
		}
	}
	return false
}

func splitLines(s string) []string {
	var lines []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			lines = append(lines, s[start:i])
			start = i + 1
		}
	}
	if start < len(s) {
		lines = append(lines, s[start:])
	}
	return lines
}
