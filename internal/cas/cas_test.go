package cas

import (
	"os"
	"path/filepath"
	"testing"
)

// Packing must be deterministic: the content hash is the case version, so two
// packs of the same tree have to agree even with different mtimes.
func TestPackDirDeterministic(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "sub"), 0o755)
	os.WriteFile(filepath.Join(dir, "a.txt"), []byte("hello"), 0o644)
	os.WriteFile(filepath.Join(dir, "sub", "b.sh"), []byte("#!/bin/sh\n"), 0o755)

	tmp := t.TempDir()
	_, sha1, err := PackDir(dir, tmp)
	if err != nil {
		t.Fatal(err)
	}
	os.Chtimes(filepath.Join(dir, "a.txt"), zeroTime.AddDate(1, 0, 0), zeroTime.AddDate(1, 0, 0))
	_, sha2, err := PackDir(dir, tmp)
	if err != nil {
		t.Fatal(err)
	}
	if sha1 != sha2 {
		t.Fatalf("hash changed with mtime: %s != %s", sha1, sha2)
	}

	os.WriteFile(filepath.Join(dir, "a.txt"), []byte("hello!"), 0o644)
	_, sha3, _ := PackDir(dir, tmp)
	if sha3 == sha1 {
		t.Fatal("hash did not change when content changed")
	}
}

func TestRoundTrip(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "x"), []byte("payload"), 0o644)
	tar, sha, err := PackDir(dir, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	s, _ := New(t.TempDir())
	if err := s.Put(tar, sha); err != nil {
		t.Fatal(err)
	}
	if !s.Has(sha) {
		t.Fatal("object not in store")
	}
	out := t.TempDir()
	if err := Unpack(s.Path(sha), out); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(out, "x"))
	if err != nil || string(b) != "payload" {
		t.Fatalf("round trip lost content: %v %q", err, b)
	}
}
