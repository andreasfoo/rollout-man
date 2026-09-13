package cmdwatch

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// TestHFTrajCountsStamps guards the counting shape: a case with two job
// stamps under trajectory/<case>/jobs/ counts as 2, not as the number of
// files inside each stamp (the bug ls-tree without -r, or line counting,
// would produce -- one subtree entry, or every file).
func TestHFTrajCountsStamps(t *testing.T) {
	mirror := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command(args[0], args[1:]...)
		cmd.Dir = mirror
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%v: %s", args, out)
		}
	}
	run("git", "init", "-q", "-b", "week4", ".")
	// alpha: two stamps of two files each; beta: one stamp; gamma: no
	// trajectory dir at all (a task shipped before any trajectory).
	for _, p := range []string{
		"trajectory/alpha/jobs/20260901T000000Z-abc/payload.json",
		"trajectory/alpha/jobs/20260901T000000Z-abc/session.jsonl",
		"trajectory/alpha/jobs/20260902T000000Z-def/payload.json",
		"trajectory/alpha/jobs/20260902T000000Z-def/session.jsonl",
		"trajectory/beta/jobs/20260901T000000Z-ghi/payload.json",
	} {
		full := filepath.Join(mirror, p)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte("{}"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	run("git", "add", ".")
	run("git", "commit", "-qm", "trajs")

	// hfTrajCounts fetches from remote "origin": point that at the repo
	// itself so the fetch path runs without a second clone.
	run("git", "remote", "add", "origin", mirror)
	run("git", "fetch", "-q", "origin", "week4")

	counts, ok := hfTrajCountsIn(mirror, "week4", []string{"alpha", "beta", "gamma"})
	if !ok {
		t.Fatal("hfTrajCounts reported failure")
	}
	if counts["alpha"] != 2 {
		t.Errorf("alpha: want 2 stamps, got %d", counts["alpha"])
	}
	if counts["beta"] != 1 {
		t.Errorf("beta: want 1 stamp, got %d", counts["beta"])
	}
	if counts["gamma"] != 0 {
		t.Errorf("gamma: want 0 for a case with no trajectory dir, got %d", counts["gamma"])
	}
}
