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
		"routed to 192.168.1.20. done\n" +
		"via proxy 192.0.2.124:3128\n" +
		"client 203.0.113.7 connected\n"
	out := scrub(t, s, in, Distributable)
	for _, ip := range []string{"10.0.0.9", "8.8.8.8", "fe80::a00:1", "192.168.1.20", "192.0.2.124", "203.0.113.7"} {
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

// A dotted quad in version context is a release number, not an address --
// those survive. Everywhere else, even public addresses are masked: nothing
// published to HF/GitHub may carry an IP (2026-09-01 user policy).
func TestVersionQuadContextKept(t *testing.T) {
	s := New(nil, nil, map[Class]bool{Distributable: true})
	in := "Version 4.2.1.9. The tree is trimmed.\n" +
		"release 1.2.3.4 is out\n" +
		"dns via 8.8.8.8\n"
	out := scrub(t, s, in, Distributable)
	for _, v := range []string{"4.2.1.9", "1.2.3.4"} {
		if !strings.Contains(out, v) {
			t.Errorf("version string %q was scrubbed as if it were an IP", v)
		}
	}
	if strings.Contains(out, "8.8.8.8") {
		t.Error("public IP outside version context survived; published artifacts must not carry addresses")
	}
}

// C++ scope resolution is not IPv6: "::e" parses as an address because 'e'
// is a hex digit. A shipped clickhouse trajectory (2026-08-31) lost every
// "::execute"/"::create"/"st::bind_front" to that -- hundreds of masks in
// one job. Word-adjacent tokens are never addresses.
func TestScopeResolutionKept(t *testing.T) {
	s := New(nil, nil, map[Class]bool{Distributable: true})
	in := "year = static_cast<Int32>(ToYearImpl::execute(source, timezone))\n" +
		"instructions.emplace_back(st::bind_front(&Instruction<T>));\n" +
		"auto col_to = ColumnString::create();\n"
	out := scrub(t, s, in, Distributable)
	for _, v := range []string{"ToYearImpl::execute", "st::bind_front", "ColumnString::create"} {
		if !strings.Contains(out, v) {
			t.Errorf("C++ scope resolution %q was scrubbed as if it were an IPv6 address", v)
		}
	}
	if strings.Contains(out, ipMask) {
		t.Errorf("unexpected IP mask in %q", out)
	}
}

// An unquoted lowercase `token = f(...)` is ordinary code, not a credential
// (2026-09-01: a shipped nghttp2 trajectory lost the hd.c line
// `token = lookup_token(nv->name, nv->namelen);`). Quoted values and
// UPPERCASE env assignments are still masked -- with the key name kept.
func TestCredentialPatternScope(t *testing.T) {
	s := New(nil, nil, nil)
	in := "  token = lookup_token(nv->name, nv->namelen);\n" +
		`"api_key": "abc123def456ghi789"` + "\n" +
		"ANTHROPIC_API_KEY=abc123def456ghi789\n"
	out := scrub(t, s, in, Distributable)
	if !strings.Contains(out, "token = lookup_token(nv->name") {
		t.Error("ordinary code assignment was scrubbed as if it were a credential")
	}
	if strings.Contains(out, "abc123def456ghi789") {
		t.Error("quoted / env-assigned credential value survived")
	}
	if !strings.Contains(out, `"api_key": "`+keyMask) {
		t.Error("key name should survive the mask, only the value is secret")
	}
	if !strings.Contains(out, "ANTHROPIC_API_KEY="+keyMask) {
		t.Error("env key name should survive the mask, only the value is secret")
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

// Two-way redact invariant: pattern/IP rules must never mask a location that
// exact matching of the real secrets does not also mask. Every over-redaction
// incident that shipped -- C++ "::execute" read as IPv6, "Version 4.2.1.9",
// `token = lookup_token(...)`, "task-verifier" -- was a pattern-only mask
// location. Scrubbing twice (full vs exact-only) and demanding identical
// output makes the check mechanical: any divergence is a false positive by
// construction. (Policy exception, deliberately absent from this corpus: IP
// addresses are masked wherever they appear, exact-backed or not --
// publication must not carry any address, 2026-09-01 user policy.)
func TestTwoWayRedact(t *testing.T) {
	key := "ting-testkey-abcdef1234567890"
	proxy := "http://192.0.2.124:3128/tingly/anthropic"
	secrets := []string{key, proxy}

	full := New(secrets, nil, map[Class]bool{Distributable: true, Debug: true})
	exact := New(secrets, nil, nil) // exact secrets only, no IP tier

	// Real benign strings from shipped trajectories that earlier versions
	// of the pattern rules mangled. No parseable IP address may appear here
	// -- those are the policy exception above.
	benign := "year = ToYearImpl::execute(source, timezone)\n" +
		"instructions.emplace_back(st::bind_front(&Instruction<T>));\n" +
		"auto col_to = ColumnString::create();\n" +
		"DB::(anonymous namespace)::FunctionFormatDateTimeImpl<DB::X>\n" +
		"Version 4.2.1.9. The tree is trimmed.\n" +
		"if (CMAKE_CXX_COMPILER_VERSION VERSION_LESS 16)\n" +
		"  token = lookup_token(nv->name, nv->namelen);\n" +
		"wd=$(mktemp -d /tmp/task-verifier.XXXXXX)\n" +
		`key="sk-debuginfo"` + "\n" +
		"2026-08-25 02:25:10.346 [2286] main/104/x.lua box.cc:6163 I> ready\n" +
		"scheduled next checkpoint for Tue Aug 25 02:25:10 2026\n" +
		"libtool 2.4.2.418 and ch-base:26.3.12.3\n"
	f, e := scrub(t, full, benign, Distributable), scrub(t, exact, benign, Distributable)
	if f != e {
		t.Errorf("pattern rules masked locations exact matching leaves alone:\nfull:  %q\nexact: %q", f, e)
	}

	// Real leaks must be masked in BOTH passes (exact is the ground truth;
	// patterns are the backstop for encodings and contexts exact misses).
	leaks := `"ANTHROPIC_API_KEY": "` + key + `"` + "\n" +
		"base_url: " + proxy + "\n" +
		"Authorization: Bearer " + key + "\n"
	for _, out := range []string{
		scrub(t, full, leaks, Distributable),
		scrub(t, exact, leaks, Distributable),
	} {
		if strings.Contains(out, key) || strings.Contains(out, proxy) {
			t.Errorf("secret survived a pass: %q", out)
		}
	}
}

func b64(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }
