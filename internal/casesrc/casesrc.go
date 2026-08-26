// Package casesrc turns "where a case lives" into "which bytes it is".
//
// The content hash is the version: it is the only honest answer to "which case
// produced this number", and it is what makes a rerun comparable to the run
// before it.
package casesrc

import (
	"archive/tar"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/andreasfoo/rollout-man/internal/casedef"
	"github.com/andreasfoo/rollout-man/internal/cmdrun"
	"github.com/andreasfoo/rollout-man/internal/config"
	"github.com/andreasfoo/rollout-man/internal/fail"
)

type Case struct {
	Label    string // human-readable: the path in the repo
	SHA256   string // the version
	Dir      string // where it is on disk right now
	PinnedAt string // the immutable ref this resolved to
	Config   *casedef.TaskConfig
}

type Resolver struct {
	Cmds    *cmdrun.Runner
	TempDir string
}

// Expand turns the submission's case list into the actual cases. A local path
// may be a glob, so pointing at a directory of cases is one line rather than
// one line per case -- which is how people actually have them.
//
// Only local paths expand: a glob over a git or hub repository would mean
// fetching it first to find out what is in there, and a submission should say
// what it runs without a network round trip.
func Expand(refs []config.CaseRef, defaults config.CaseRef, near string) ([]config.CaseRef, error) {
	var out []config.CaseRef
	for _, raw := range refs {
		ref := raw.Merge(defaults)
		if (ref.Source != "" && ref.Source != "local") || !strings.ContainsAny(ref.Path, "*?[") {
			out = append(out, raw)
			continue
		}
		matches, err := glob(ref.Path, near)
		if err != nil {
			return nil, err
		}
		if len(matches) == 0 {
			return nil, fail.New(fail.Host, "no case directories match "+ref.Path)
		}
		for _, m := range matches {
			one := raw
			one.Path = m
			out = append(out, one)
		}
	}
	return out, nil
}

// glob matches from the working directory, then from beside the submission
// file. Trying both is what lets one file be run from the repository root and
// from its own directory, which is otherwise a coin flip nobody should have to
// remember.
func glob(pattern, near string) ([]string, error) {
	try := []string{pattern}
	if near != "" && !filepath.IsAbs(pattern) {
		try = append(try, filepath.Join(near, pattern))
	}
	for _, p := range try {
		hits, err := filepath.Glob(p)
		if err != nil {
			return nil, fail.New(fail.Host, "bad case pattern "+pattern+": "+err.Error())
		}
		var dirs []string
		for _, h := range hits {
			// A match is a case only if it looks like one. A glob over a
			// directory picks up README files and stray archives otherwise,
			// and the failure would surface much later as "parse task.toml".
			if fi, err := os.Stat(h); err == nil && fi.IsDir() {
				if _, err := os.Stat(filepath.Join(h, "task.toml")); err == nil {
					dirs = append(dirs, h)
				}
			}
		}
		if len(dirs) > 0 {
			sort.Strings(dirs) // the trial list must not depend on readdir order
			return dirs, nil
		}
	}
	return nil, nil
}

// Resolve fetches the case if needed, hashes it, and reads its task.toml.
func (r *Resolver) Resolve(ctx context.Context, ref config.CaseRef) (*Case, error) {
	dir, pinned, err := r.fetch(ctx, ref)
	if err != nil {
		return nil, err
	}
	sha, err := HashDir(dir)
	if err != nil {
		return nil, fail.Wrap(fail.Host, "hash case", err)
	}
	if ref.SHA256 != "" && ref.SHA256 != sha {
		return nil, fail.New(fail.Host,
			fmt.Sprintf("sha256 mismatch for %s: declared %s, got %s", ref.Label(), ref.SHA256, sha))
	}
	cfg, err := casedef.Load(dir)
	if err != nil {
		return nil, fail.Wrap(fail.Host, "parse task.toml", err)
	}
	return &Case{Label: ref.Label(), SHA256: sha, Dir: dir, PinnedAt: pinned, Config: cfg}, nil
}

func (r *Resolver) fetch(ctx context.Context, ref config.CaseRef) (dir, pinned string, err error) {
	switch ref.Source {
	case "", "local":
		p, err := filepath.Abs(ref.Path)
		if err != nil {
			return "", "", err
		}
		if fi, err := os.Stat(p); err != nil || !fi.IsDir() {
			return "", "", fail.New(fail.Host, "local case path is not a directory: "+p)
		}
		return p, "local", nil

	case "git":
		cmd := ref.Fetch
		if cmd == "" {
			cmd = "source_git"
		}
		dst, err := os.MkdirTemp(r.TempDir, "case-*")
		if err != nil {
			return "", "", err
		}
		if _, err := r.Cmds.Run(ctx, cmd, map[string]string{
			"GitRepo": ref.Repo, "GitRef": ref.Ref, "LocalPath": dst,
		}); err != nil {
			return "", "", fail.Wrap(fail.Host, "git fetch", err)
		}
		sub := dst
		if ref.Path != "" {
			sub = filepath.Join(dst, ref.Path)
			if fi, err := os.Stat(sub); err != nil || !fi.IsDir() {
				return "", "", fail.New(fail.Host, "path not found in repo: "+ref.Path)
			}
		}
		return sub, pinGit(dst, ref.Ref), nil

	case "hf":
		// A dataset repo on a hub is a git repo with a download command in
		// front of it, so it is the same shape as git: a command fetches it,
		// and the content hash -- not the ref -- is still the version.
		cmd := ref.Fetch
		if cmd == "" {
			cmd = "source_hf"
		}
		dst, err := os.MkdirTemp(r.TempDir, "case-*")
		if err != nil {
			return "", "", err
		}
		rev := ref.Ref
		if rev == "" {
			rev = "main"
		}
		if _, err := r.Cmds.Run(ctx, cmd, map[string]string{
			"HfRepo": ref.Repo, "HfRevision": rev, "LocalPath": dst, "HfPath": ref.Path,
		}); err != nil {
			return "", "", fail.Wrap(fail.Host, "hf fetch", err)
		}
		sub := dst
		if ref.Path != "" {
			sub = filepath.Join(dst, ref.Path)
			if fi, err := os.Stat(sub); err != nil || !fi.IsDir() {
				return "", "", fail.New(fail.Host, "path not found in dataset repo: "+ref.Path)
			}
		}
		return sub, "hf:" + ref.Repo + "@" + rev, nil
	}
	return "", "", fail.New(fail.Host,
		"unknown case source "+ref.Source+" (local, git, hf)")
}

var zeroTime = time.Unix(0, 0).UTC()

// HashDir hashes a directory tree deterministically: sorted entries, no
// timestamps, no ownership. The same content must hash the same on any machine
// on any day, or the version means nothing.
func HashDir(dir string) (string, error) {
	var rels []string
	err := filepath.Walk(dir, func(p string, fi os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil || rel == "." {
			return err
		}
		// Case-forge keeps mutable workflow state and Harbor trial artifacts beside
		// the case definition. They are evidence for the factory, not input bytes
		// for the task, and may include container-owned unreadable files. Excluding
		// them keeps a local case resolvable after an oracle/agent run.
		if fi.IsDir() && ignoredCaseRuntimeDir(rel) {
			return filepath.SkipDir
		}
		rels = append(rels, rel)
		return nil
	})
	if err != nil {
		return "", err
	}
	sort.Strings(rels)

	h := sha256.New()
	tw := tar.NewWriter(h)
	for _, rel := range rels {
		full := filepath.Join(dir, rel)
		fi, err := os.Lstat(full)
		if err != nil {
			return "", err
		}
		link := ""
		if fi.Mode()&os.ModeSymlink != 0 {
			if link, err = os.Readlink(full); err != nil {
				return "", err
			}
		}
		hdr, err := tar.FileInfoHeader(fi, link)
		if err != nil {
			return "", err
		}
		hdr.Name = filepath.ToSlash(rel)
		hdr.Uid, hdr.Gid, hdr.Uname, hdr.Gname = 0, 0, "", ""
		hdr.ModTime, hdr.AccessTime, hdr.ChangeTime = zeroTime, time.Time{}, time.Time{}
		hdr.Format = tar.FormatUSTAR
		if fi.Mode().IsRegular() {
			hdr.Mode = 0o644
			if fi.Mode()&0o111 != 0 {
				hdr.Mode = 0o755
			}
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return "", err
		}
		if fi.Mode().IsRegular() {
			f, err := os.Open(full)
			if err != nil {
				return "", err
			}
			_, err = io.Copy(tw, f)
			f.Close()
			if err != nil {
				return "", err
			}
		}
	}
	if err := tw.Close(); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func ignoredCaseRuntimeDir(rel string) bool {
	first := strings.SplitN(filepath.ToSlash(rel), "/", 2)[0]
	return first == ".git" || first == ".factory" || first == "trials"
}

func pinGit(dir, ref string) string {
	b, err := os.ReadFile(filepath.Join(dir, ".git", "HEAD"))
	if err != nil {
		return ref
	}
	s := strings.TrimSpace(string(b))
	if rest, ok := strings.CutPrefix(s, "ref: "); ok {
		if rb, err := os.ReadFile(filepath.Join(dir, ".git", rest)); err == nil {
			s = strings.TrimSpace(string(rb))
		}
	}
	if len(s) >= 7 && !strings.Contains(s, " ") {
		return s
	}
	return ref
}
