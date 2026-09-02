// Package redact scrubs artifacts before they leave the runner.
//
// Key scrubbing is mandatory and applies to every class. IP scrubbing is
// tiered: on for artifacts that get distributed, off for the logs people
// debug with.
package redact

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"io"
	"net/netip"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

type Class string

const (
	// Distributable leaves the team: trajectory, result.
	Distributable Class = "distributable"
	// Debug stays for troubleshooting: the various logs.
	Debug Class = "debug"
	// Task is case content copied into the trial dir for shipping (OUT_DIR/case):
	// the author's Dockerfiles, changelogs, oracle binaries. Only exact runner
	// secrets are scrubbed there -- pattern/IP rules exist for runtime-produced
	// artifacts (trajectories, logs), and running them over source material
	// rewrites version numbers (ch-base:26.3.12.3 became a broken FROM line on
	// HF) and corrupts binaries (an 88MB oracle ELF lost its planted
	// DEDUP_TOKEN string to the "token[:=]" pattern).
	Task Class = "task"
)

const (
	keyMask = "***REDACTED_KEY***"
	ipMask  = "***REDACTED_IP***"
)

var patterns = []*regexp.Regexp{
	regexp.MustCompile(`sk-[A-Za-z0-9_\-]{16,}`),
	regexp.MustCompile(`AKIA[0-9A-Z]{16}`),
	regexp.MustCompile(`gh[pousr]_[A-Za-z0-9]{20,}`),
	// Quoted value after a credential-ish key (JSON/YAML/config dumps):
	//   "ANTHROPIC_API_KEY": "sk-..."   api_key = 'abc123...'
	// The opening quote is required: an unquoted lowercase assignment is
	// ordinary code, not a credential (2026-09-01: a shipped nghttp2
	// trajectory lost the C line `token = lookup_token(nv->name, ...)` to
	// this pattern). No \b on the key: "ANTHROPIC_API_KEY" must match via
	// its "_API_KEY" tail, and the quote requirement is the real guard.
	// Groups 1+2 are kept by the replacement so the key name survives and
	// only the value is masked.
	regexp.MustCompile(`(?i)(authorization|x-api-key|api[_-]?key|token)(["']?\s*[:=]\s*["'])[A-Za-z0-9._\-]{12,}`),
	// Shell-style env assignment needs no quote: API_KEY=abc... Kept
	// case-sensitive and space-free around '=' so C code like
	// `token = lookup_token(...)` can never match.
	regexp.MustCompile(`(AUTHORIZATION|X-API-KEY|API[_-]?KEY|TOKEN)(=)[A-Za-z0-9._\-]{16,}`),
	regexp.MustCompile(`(?i)bearer\s+[A-Za-z0-9._\-]{12,}`),
	// credential parameters carried inside pre-authorised URLs
	regexp.MustCompile(`(?i)(tempauth|authkey|sig|signature|x-amz-signature|x-amz-credential|access_token)(=)[^&\s"']+`),
}

// IP detection delegates "is this an address" to the stdlib: candidate
// tokens are maximal runs of address characters, then netip.ParseAddr /
// ParseAddrPort decide. No hand-rolled octet regex -- the old one ate
// version numbers for breakfast (libtool 2.4.2.418 in a changelog, a
// ch-base:26.3.12.3 image tag in a Dockerfile). Version strings now survive
// for free: they are not parseable addresses.

// isIPTokChar reports whether c can appear inside an IP-looking token.
// Hex letters are included for IPv6; '%' for zones; ':' for v6 and ports.
func isIPTokChar(c byte) bool {
	return c == '.' || c == ':' || c == '%' ||
		(c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')
}

type Hits struct {
	Exact   int `json:"exact"`
	Pattern int `json:"pattern"`
	IP      int `json:"ip"`
}

func (h *Hits) add(o Hits) { h.Exact += o.Exact; h.Pattern += o.Pattern; h.IP += o.IP }

type Redactor struct {
	exact     []string
	ipAllow   map[string]bool
	extra     []*regexp.Regexp
	RedactIPs map[Class]bool
}

// New builds a sanitizer. secrets are the plaintext values this runner handed
// to the trial; every encoding variant of each is scrubbed.
func New(secrets []string, extra []string, redactIPs map[Class]bool) *Redactor {
	s := &Redactor{
		ipAllow:   map[string]bool{"127.0.0.1": true, "::1": true, "0.0.0.0": true},
		RedactIPs: redactIPs,
	}
	seen := map[string]bool{}
	for _, sec := range secrets {
		for _, v := range variants(sec) {
			if len(v) >= 8 && !seen[v] {
				seen[v] = true
				s.exact = append(s.exact, v)
			}
		}
	}
	// longest first so a prefix never masks its own superstring
	sort.Slice(s.exact, func(i, j int) bool { return len(s.exact[i]) > len(s.exact[j]) })
	for _, p := range extra {
		if re, err := regexp.Compile(p); err == nil {
			s.extra = append(s.extra, re)
		}
	}
	return s
}

func variants(s string) []string {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	return []string{
		s,
		base64.StdEncoding.EncodeToString([]byte(s)),
		base64.RawStdEncoding.EncodeToString([]byte(s)),
		base64.URLEncoding.EncodeToString([]byte(s)),
		url.QueryEscape(s),
	}
}

// isBinary sniffs the first 8KB for a NUL byte, like grep -I. Binary files
// are never rewritten: a mask is a different length than the bytes it
// replaces, so any hit would corrupt offsets and checksums -- a Roboto TTF
// lost 24 bytes to "IPs" that were font tables, and an oracle ELF shrank by
// the length of its planted DEDUP_TOKEN string. A real runner secret inside
// a binary is accepted as a leak risk: case binaries are oracle artifacts,
// and silently corrupting them is the worse failure.
func isBinary(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	var buf [8192]byte
	n, _ := f.Read(buf[:])
	return bytes.IndexByte(buf[:n], 0) >= 0
}

// ScrubFile rewrites path in place and reports what was hit.
func (s *Redactor) ScrubFile(path string, class Class) (Hits, error) {
	if isBinary(path) {
		return Hits{}, nil
	}
	in, err := os.Open(path)
	if err != nil {
		return Hits{}, err
	}
	tmp := path + ".scrub"
	out, err := os.Create(tmp)
	if err != nil {
		in.Close()
		return Hits{}, err
	}
	h, err := s.Scrub(in, out, class)
	in.Close()
	out.Close()
	if err != nil {
		os.Remove(tmp)
		return h, err
	}
	return h, os.Rename(tmp, path)
}

// Scrub streams line by line, keeping one line of overlap so a secret split
// across a newline is still caught.
func (s *Redactor) Scrub(r io.Reader, w io.Writer, class Class) (Hits, error) {
	var total Hits
	br := bufio.NewReaderSize(r, 1<<20)
	bw := bufio.NewWriter(w)
	defer bw.Flush()

	prev := ""
	for {
		line, err := br.ReadString('\n')
		if line != "" || err == nil {
			// join with the previous line to catch newline-split secrets
			joined, jh := s.scrubExact(prev + line)
			total.add(jh)
			if prev != "" {
				// prev was already written; emit only the tail
				if len(joined) >= len(prev) {
					line = joined[len(prev):]
				} else {
					line = joined
				}
			} else {
				line = joined
			}
			var ph Hits
			line, ph = s.scrubPatterns(line, class)
			total.add(ph)
			if _, werr := bw.WriteString(line); werr != nil {
				return total, werr
			}
			prev = strings.TrimRight(line, "\r\n")
			if len(prev) > 512 {
				prev = prev[len(prev)-512:]
			}
		}
		if err == io.EOF {
			return total, nil
		}
		if err != nil {
			return total, err
		}
	}
}

func (s *Redactor) scrubExact(line string) (string, Hits) {
	var h Hits
	for _, sec := range s.exact {
		if n := strings.Count(line, sec); n > 0 {
			h.Exact += n
			line = strings.ReplaceAll(line, sec, keyMask)
		}
	}
	return line, h
}

func (s *Redactor) scrubPatterns(line string, class Class) (string, Hits) {
	var h Hits
	// Task content is the case author's source material: only exact runner
	// secrets (handled by scrubExact) may be rewritten there. Pattern and IP
	// rules exist for artifacts the RUN produced (trajectories, logs).
	if class == Task {
		return line, h
	}
	all := append(append([]*regexp.Regexp{}, patterns...), s.extra...)
	for _, re := range all {
		if n := len(re.FindAllStringIndex(line, -1)); n == 0 {
			continue
		} else {
			h.Pattern += n
		}
		// Groups 1+2 (key name + separator, where the pattern has them) are
		// kept; only the value is masked. Patterns without groups replace
		// the whole match, as before.
		line = re.ReplaceAllString(line, "${1}${2}"+keyMask)
	}
	if s.RedactIPs[class] {
		line = s.scrubIPs(line, &h)
	}
	return line, h
}

// scrubIPs masks tokens the stdlib validates as IP addresses. A token that
// fails ParseAddr and ParseAddrPort is not an address and is left alone --
// that is the whole fix for version strings: "2.4.2.418" (418 > 255),
// "1.26.3.12.3" (five parts) and the "e:26.3.12.3" fragment of an image tag
// all fail to parse, so changelogs and Dockerfiles keep their versions
// while "dial 10.0.0.9:8080" still loses the address.
func (s *Redactor) scrubIPs(line string, h *Hits) string {
	var b strings.Builder
	pos := 0
	i := 0
	for i < len(line) {
		if !isIPTokChar(line[i]) {
			i++
			continue
		}
		j := i
		for j < len(line) && isIPTokChar(line[j]) {
			j++
		}
		// An address-shaped run embedded in a longer word is not an address:
		// 'e' is a hex digit, so the "::e" of C++ scope resolution
		// ("ToYearImpl::execute") parses as IPv6 -- as does "::b" of
		// "st::bind_front" and "::c" of "ColumnString::create". A shipped
		// clickhouse trajectory (2026-08-31) lost hundreds of those to this.
		// Require non-word characters on both sides of the token.
		if (i > 0 && isWordChar(line[i-1])) || (j < len(line) && isWordChar(line[j])) {
			i = j
			continue
		}
		// Trailing punctuation ("routed to 10.0.0.9.") is not part of the
		// address; trim it before asking the parser.
		tok := strings.TrimRight(line[i:j], ".:")
		// A dotted quad right after a version marker is a release number,
		// not an address ("Version 4.2.1.9" in a shipped clickhouse
		// trajectory, 2026-08-31). IPs are still masked wherever else they
		// appear -- publication must not carry ANY address, public included.
		if strings.Contains(tok, ".") && !strings.Contains(tok, ":") && isVersionContext(line, i) {
			i = j
			continue
		}
		if rep, ok := s.maskIP(tok, h); ok {
			b.WriteString(line[pos:i])
			b.WriteString(rep)
			pos = i + len(tok)
		}
		i = j
	}
	b.WriteString(line[pos:])
	return b.String()
}

// maskIP returns the replacement for tok when tok is a non-allowlisted
// address. AddrPort keeps the ":port" suffix readable -- only the address
// is sensitive. Every parseable address is masked, public ones included:
// artifacts published to HF/GitHub must not carry any address at all --
// a public IP can still name the runner's or the proxy's infrastructure.
func (s *Redactor) maskIP(tok string, h *Hits) (string, bool) {
	if !strings.ContainsAny(tok, ".:") {
		return "", false // a plain number or hex word is never an address
	}
	if a, err := netip.ParseAddr(tok); err == nil {
		if s.ipAllow[a.String()] {
			return "", false
		}
		h.IP++
		return ipMask, true
	}
	if ap, err := netip.ParseAddrPort(tok); err == nil {
		if s.ipAllow[ap.Addr().String()] {
			return "", false
		}
		h.IP++
		return ipMask + ":" + strconv.Itoa(int(ap.Port())), true
	}
	return "", false
}

// isVersionContext reports whether the token starting at i is preceded by a
// version marker -- "Version 4.2.1.9", "release 1.2.3.4" -- in which case a
// dotted quad is a release number, not an address. Callers apply it to
// dotted quads only; IPv6-shaped tokens have no version-string doppelganger.
func isVersionContext(line string, i int) bool {
	k := i - 1
	for k >= 0 && (line[k] == ' ' || line[k] == '\t') {
		k--
	}
	end := k + 1
	for k >= 0 && isWordChar(line[k]) {
		k--
	}
	switch strings.ToLower(line[k+1 : end]) {
	case "version", "ver", "release":
		return true
	}
	return false
}

func isWordChar(c byte) bool {
	return c == '_' ||
		(c >= '0' && c <= '9') || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}
