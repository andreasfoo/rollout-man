package redact

import (
	"bytes"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// ClassifyPath assigns the scrubbing tier for a trial-OUT_DIR-relative path.
// It is the single source of truth for the tiering: the redact action and the
// two-way audit must agree on a file's class or their outputs are not
// comparable.
//
//   - case/**            -> Task (case author's source material: exact runner
//     secrets only; pattern/IP rules rewrite version numbers and corrupt
//     binaries there)
//   - traj.jsonl, result.json (any depth) -> Distributable (leaves the team)
//   - everything else    -> Debug (logs for troubleshooting)
func ClassifyPath(rel string) Class {
	if rel == "case" || strings.HasPrefix(rel, "case"+string(filepath.Separator)) {
		return Task
	}
	switch filepath.Base(rel) {
	case "traj.jsonl", "result.json":
		return Distributable
	}
	return Debug
}

// Divergence is one line where the full rules masked something the exact
// secrets alone did not -- either a false positive to fix, or a policy-class
// hit (any IP address) to review.
type Divergence struct {
	Path  string
	Class Class
	Line  int // 1-based
	Full  string
	Exact string
}

// AuditReport summarizes a two-way audit of a tree.
type AuditReport struct {
	Scanned        int
	DivergentFiles int
	Samples        []Divergence
}

// Audit walks root and scrubs every text file twice -- full pattern/IP rules
// vs exact secrets only -- reporting every line where the outputs differ.
// This is the standing form of the two-way redact check: run it over real
// artifacts and any pattern-only mask location shows up for review, instead
// of relying on a fixed test corpus to anticipate every benign string.
//
// IP tiers are enabled for Distributable and Debug (matching the campaign's
// ips:{traj:true, logs:true}); Task content never sees pattern/IP rules, so
// its exact-only pass is identical by construction.
//
// maxPerFile caps the samples kept per divergent file (0 = no cap).
func Audit(root string, secrets []string, maxPerFile int) (*AuditReport, error) {
	full := New(secrets, nil, map[Class]bool{Distributable: true, Debug: true})
	exact := New(secrets, nil, nil)
	rep := &AuditReport{}
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !d.Type().IsRegular() {
			return err
		}
		if isBinary(p) {
			return nil
		}
		in, err := os.ReadFile(p)
		if err != nil {
			return nil // unreadable (container-owned); not scrubbable either
		}
		rel, _ := filepath.Rel(root, p)
		class := ClassifyPath(rel)
		rep.Scanned++
		var fb, eb bytes.Buffer
		if _, err := full.Scrub(bytes.NewReader(in), &fb, class); err != nil {
			return nil
		}
		if _, err := exact.Scrub(bytes.NewReader(in), &eb, class); err != nil {
			return nil
		}
		if fb.String() == eb.String() {
			return nil
		}
		rep.DivergentFiles++
		fl, el := strings.Split(fb.String(), "\n"), strings.Split(eb.String(), "\n")
		kept := 0
		for i := 0; i < len(fl) && i < len(el); i++ {
			if fl[i] == el[i] {
				continue
			}
			if maxPerFile > 0 && kept >= maxPerFile {
				break
			}
			kept++
			rep.Samples = append(rep.Samples, Divergence{
				Path: rel, Class: class, Line: i + 1, Full: fl[i], Exact: el[i],
			})
		}
		return nil
	})
	return rep, err
}
