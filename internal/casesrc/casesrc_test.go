package casesrc

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// The hash is the version, so it must depend on content and nothing else.
func TestHashDirIsDeterministic(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "sub"), 0o755)
	os.WriteFile(filepath.Join(dir, "a.txt"), []byte("hello"), 0o644)
	os.WriteFile(filepath.Join(dir, "sub", "b.sh"), []byte("#!/bin/sh\n"), 0o755)

	first, err := HashDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	later := time.Now().Add(time.Hour)
	os.Chtimes(filepath.Join(dir, "a.txt"), later, later)
	second, err := HashDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Fatalf("mtime changed the hash: %s != %s", first, second)
	}

	os.WriteFile(filepath.Join(dir, "a.txt"), []byte("hello!"), 0o644)
	third, _ := HashDir(dir)
	if third == first {
		t.Fatal("content changed but the hash did not")
	}
}

func TestHashIgnoresGitDir(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "a.txt"), []byte("x"), 0o644)
	before, _ := HashDir(dir)
	os.MkdirAll(filepath.Join(dir, ".git", "objects"), 0o755)
	os.WriteFile(filepath.Join(dir, ".git", "HEAD"), []byte("ref: refs/heads/main\n"), 0o644)
	after, _ := HashDir(dir)
	if before != after {
		t.Fatal("a .git directory changed the case hash")
	}
}

func TestHashIgnoresCaseForgeRuntimeDirs(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "task.toml"), []byte("[task]\nid = \"x\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	before, err := HashDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, rel := range []string{".factory/state.json", "trials/job/agent/sessions/private.json"} {
		p := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("runtime"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	after, err := HashDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if before != after {
		t.Fatalf("runtime artifacts changed hash: %s != %s", before, after)
	}
}
