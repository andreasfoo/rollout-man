package redact

import (
	"bytes"
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func scrub(t *testing.T, s *Redactor, in string, c Class) string {
	t.Helper()
	var out strings.Builder
	if _, err := s.Scrub(strings.NewReader(in), &out, c); err != nil {
		t.Fatal(err)
	}
	return out.String()
}

// Keys are scrubbed everywhere; IPs only in what leaves the team.
func TestTiering(t *testing.T) {
	key := "sk-live-abcdefghijklmnop"
	s := New([]string{key}, nil, map[Class]bool{Distributable: true, Debug: false})

	line := "used " + key + " against 10.1.2.3\n"

	dist := scrub(t, s, line, Distributable)
	if strings.Contains(dist, key) {
		t.Error("key survived in a distributable artifact")
	}
	if strings.Contains(dist, "10.1.2.3") {
		t.Error("IP survived in a distributable artifact")
	}

	dbg := scrub(t, s, line, Debug)
	if strings.Contains(dbg, key) {
		t.Error("key survived in a debug artifact -- key scrubbing has no opt-out")
	}
	if !strings.Contains(dbg, "10.1.2.3") {
		t.Error("IP was scrubbed from a debug artifact, destroying its value")
	}
}

func TestEncodedVariants(t *testing.T) {
	key := "sk-live-abcdefghijklmnop"
	s := New([]string{key}, nil, nil)
	// base64 of the same secret must be caught too
	in := "Y2s9c2stbGl2ZQ== " + b64(key) + "\n"
	out := scrub(t, s, in, Debug)
	if strings.Contains(out, b64(key)) {
		t.Error("base64-encoded secret survived")
	}
}

func TestLoopbackKept(t *testing.T) {
	s := New(nil, nil, map[Class]bool{Distributable: true})
	out := scrub(t, s, "bound 127.0.0.1:8080\n", Distributable)
	if !strings.Contains(out, "127.0.0.1") {
		t.Error("loopback should stay readable")
	}
}

// Version strings are not addresses; the stdlib parser rejects them, so they
// must survive IP scrubbing. These are the exact false positives that were
// published to HF: a libtool version in a changelog and an image tag in a
// Dockerfile.
func TestVersionStringsKept(t *testing.T) {
	s := New(nil, nil, map[Class]bool{Distributable: true, Task: true})
	in := "libtool 2.4.2.418\n" +
		"FROM cyborgzero/ch-base:26.3.12.3\n" +
		"version 1.26.3.12.3 released\n"
	out := scrub(t, s, in, Distributable)
	for _, v := range []string{"2.4.2.418", "26.3.12.3", "1.26.3.12.3"} {
		if !strings.Contains(out, v) {
			t.Errorf("version string %q was scrubbed as if it were an IP", v)
		}
	}
}

func TestRealIPsMasked(t *testing.T) {
	s := New(nil, nil, map[Class]bool{Distributable: true})
	in := "dial 10.0.0.9:8080 then https://8.8.8.8/dns then fe80::a00:1\n" +
		"routed to 192.168.1.20. done\n"
	out := scrub(t, s, in, Distributable)
	for _, ip := range []string{"10.0.0.9", "8.8.8.8", "fe80::a00:1", "192.168.1.20"} {
		if strings.Contains(out, ip) {
			t.Errorf("IP %q survived in a distributable artifact", ip)
		}
	}
	if !strings.Contains(out, ipMask+":8080") {
		t.Error("port suffix should stay readable next to the masked address")
	}
	if !strings.Contains(out, ". done") {
		t.Error("sentence-ending period after a masked IP should be kept")
	}
}

// Task content is case source material: only exact runner secrets may be
// rewritten there. Pattern shapes and IPs must pass through untouched.
func TestTaskClassExactOnly(t *testing.T) {
	key := "sk-live-abcdefghijklmnop"
	s := New([]string{key}, nil, map[Class]bool{Task: true})
	in := "planted DEDUP_TOKEN=abcdefghijklmnopqrstuvwxyz\n" +
		"proxy 10.0.0.9 and key " + key + "\n"
	out := scrub(t, s, in, Task)
	if strings.Contains(out, key) {
		t.Error("exact runner secret survived in task content")
	}
	if !strings.Contains(out, "DEDUP_TOKEN=abcdefghijklmnopqrstuvwxyz") {
		t.Error("pattern rule rewrote a planted marker in task content")
	}
	if !strings.Contains(out, "10.0.0.9") {
		t.Error("IP rule rewrote task content; only exact secrets are allowed there")
	}
}

// Binaries are never rewritten: a mask never has the length of the bytes it
// replaces, so any substitution corrupts offsets and checksums (an oracle
// ELF lost its planted DEDUP_TOKEN string; a TTF shrank by 24 bytes).
func TestBinarySkipped(t *testing.T) {
	key := "sk-live-abcdefghijklmnop"
	s := New([]string{key}, nil, map[Class]bool{Debug: true, Distributable: true, Task: true})

	bin := []byte("ELF\x00header\x00" + key + "\x00\x01\x02")
	path := filepath.Join(t.TempDir(), "target.vuln")
	if err := os.WriteFile(path, bin, 0o644); err != nil {
		t.Fatal(err)
	}
	h, err := s.ScrubFile(path, Distributable)
	if err != nil {
		t.Fatal(err)
	}
	if h != (Hits{}) {
		t.Errorf("binary file reported hits: %+v", h)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, bin) {
		t.Error("binary file was rewritten; even an exact secret must not be masked in-place")
	}
}

func b64(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }
