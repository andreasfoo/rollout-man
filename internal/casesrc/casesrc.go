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
	}
	return "", "", fail.New(fail.Host, "unknown case source "+ref.Source)
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
		if fi.IsDir() && (rel == ".git" || strings.HasPrefix(rel, ".git"+string(filepath.Separator))) {
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
