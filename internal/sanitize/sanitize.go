// Package sanitize scrubs artifacts before they leave the runner.
//
// Key scrubbing is mandatory and applies to every class. IP scrubbing is
// tiered: on for artifacts that get distributed, off for the logs people
// debug with.
package sanitize

import (
	"bufio"
	"encoding/base64"
	"io"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strings"
)

type Class string

const (
	// Distributable leaves the team: trajectory, result.
	Distributable Class = "distributable"
	// Debug stays for troubleshooting: the various logs.
	Debug Class = "debug"
)

const (
	keyMask = "***REDACTED_KEY***"
	ipMask  = "***REDACTED_IP***"
)

var patterns = []*regexp.Regexp{
	regexp.MustCompile(`sk-[A-Za-z0-9_\-]{16,}`),
	regexp.MustCompile(`AKIA[0-9A-Z]{16}`),
	regexp.MustCompile(`gh[pousr]_[A-Za-z0-9]{20,}`),
	regexp.MustCompile(`(?i)(authorization|x-api-key|api[_-]?key|token)\s*[:=]\s*["']?[A-Za-z0-9._\-]{12,}["']?`),
	regexp.MustCompile(`(?i)bearer\s+[A-Za-z0-9._\-]{12,}`),
	// credential parameters carried inside pre-authorised URLs
	regexp.MustCompile(`(?i)(tempauth|authkey|sig|signature|x-amz-signature|x-amz-credential|access_token)=[^&\s"']+`),
}

var (
	ipv4 = regexp.MustCompile(`\b(?:\d{1,3}\.){3}\d{1,3}\b`)
	ipv6 = regexp.MustCompile(`\b(?:[0-9A-Fa-f]{1,4}:){2,7}[0-9A-Fa-f]{1,4}\b`)
)

type Hits struct {
	Exact   int `json:"exact"`
	Pattern int `json:"pattern"`
	IP      int `json:"ip"`
}

func (h *Hits) add(o Hits) { h.Exact += o.Exact; h.Pattern += o.Pattern; h.IP += o.IP }

type Sanitizer struct {
	exact     []string
	ipAllow   map[string]bool
	extra     []*regexp.Regexp
	RedactIPs map[Class]bool
}

// New builds a sanitizer. secrets are the plaintext values this runner handed
// to the trial; every encoding variant of each is scrubbed.
func New(secrets []string, extra []string, redactIPs map[Class]bool) *Sanitizer {
	s := &Sanitizer{
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

// ScrubFile rewrites path in place and reports what was hit.
func (s *Sanitizer) ScrubFile(path string, class Class) (Hits, error) {
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
func (s *Sanitizer) Scrub(r io.Reader, w io.Writer, class Class) (Hits, error) {
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

func (s *Sanitizer) scrubExact(line string) (string, Hits) {
	var h Hits
	for _, sec := range s.exact {
		if n := strings.Count(line, sec); n > 0 {
			h.Exact += n
			line = strings.ReplaceAll(line, sec, keyMask)
		}
	}
	return line, h
}

func (s *Sanitizer) scrubPatterns(line string, class Class) (string, Hits) {
	var h Hits
	all := append(append([]*regexp.Regexp{}, patterns...), s.extra...)
	for _, re := range all {
		line = re.ReplaceAllStringFunc(line, func(m string) string { h.Pattern++; return keyMask })
	}
	if s.RedactIPs[class] {
		repl := func(m string) string {
			if s.ipAllow[m] {
				return m
			}
			h.IP++
			return ipMask
		}
		line = ipv4.ReplaceAllStringFunc(line, repl)
		line = ipv6.ReplaceAllStringFunc(line, repl)
	}
	return line, h
}
