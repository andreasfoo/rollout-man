package sanitize

import (
	"encoding/base64"
	"strings"
	"testing"
)

func scrub(t *testing.T, s *Sanitizer, in string, c Class) string {
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

func b64(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }
