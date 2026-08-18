// Package resolve turns a case location into content: fetch, hash, store,
// parse task.toml. The content hash is the version.
package resolve

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/andreasfoo/rollout-man/internal/cas"
	"github.com/andreasfoo/rollout-man/internal/casedef"
	"github.com/andreasfoo/rollout-man/internal/cmdrun"
	"github.com/andreasfoo/rollout-man/internal/failure"
	"github.com/andreasfoo/rollout-man/internal/spec"
)

type State string

const (
	Resolving State = "RESOLVING"
	Ready     State = "READY"
	Admitted  State = "ADMITTED"
	Rejected  State = "REJECTED"
	Invalid   State = "INVALID"
)

type CaseVersion struct {
	SHA256   string
	Label    string
	Source   spec.CaseRef
	PinnedAt string // the immutable ref this resolved to
	Dir      string // unpacked, under work/cases/<sha>
	Cfg      *casedef.TaskConfig
	State    State
	Error    string
	CacheHit bool
}

type Resolver struct {
	Runner   *cmdrun.Runner
	Store    *cas.Store
	WorkRoot string
	Log      func(string, ...any)
}

func (r *Resolver) logf(f string, a ...any) {
	if r.Log != nil {
		r.Log(f, a...)
	}
}

func (r *Resolver) Resolve(ctx context.Context, ref spec.CaseRef) (*CaseVersion, error) {
	cv := &CaseVersion{Label: ref.Label(), Source: ref, State: Resolving, PinnedAt: ref.Ref}

	// Fast path: an explicit sha256 already in the store needs no fetch.
	if ref.SHA256 != "" && r.Store.Has(ref.SHA256) {
		cv.SHA256, cv.CacheHit = ref.SHA256, true
		if err := r.materialise(cv); err != nil {
			return r.invalid(cv, err)
		}
		return cv, nil
	}

	src, cleanup, err := r.fetch(ctx, ref, cv)
	if cleanup != nil {
		defer cleanup()
	}
	if err != nil {
		return r.invalid(cv, err)
	}

	tmpTar, sha, err := cas.PackDir(src, r.tmp())
	if err != nil {
		return r.invalid(cv, failure.Wrap(failure.HostError, "pack case", err))
	}
	cv.SHA256 = sha
	if ref.SHA256 != "" && ref.SHA256 != sha {
		os.Remove(tmpTar)
		return r.invalid(cv, failure.New(failure.HostError,
			fmt.Sprintf("sha256 mismatch: declared %s, got %s", ref.SHA256, sha)))
	}
	cv.CacheHit = r.Store.Has(sha)
	if err := r.Store.Put(tmpTar, sha); err != nil {
		return r.invalid(cv, failure.Wrap(failure.HostError, "store case", err))
	}
	if err := r.materialise(cv); err != nil {
		return r.invalid(cv, err)
	}
	return cv, nil
}

func (r *Resolver) fetch(ctx context.Context, ref spec.CaseRef, cv *CaseVersion) (string, func(), error) {
	switch ref.Source {
	case "local":
		p := ref.Path
		if !filepath.IsAbs(p) {
			abs, err := filepath.Abs(p)
			if err != nil {
				return "", nil, err
			}
			p = abs
		}
		if fi, err := os.Stat(p); err != nil || !fi.IsDir() {
			return "", nil, failure.New(failure.HostError, "local case path is not a directory: "+p)
		}
		cv.PinnedAt = "local"
		return p, nil, nil

	case "git":
		if !r.Runner.Has(cmdName(ref.Fetch, "source_git")) {
			return "", nil, failure.New(failure.HostError, "no source_git command configured")
		}
		dir, err := os.MkdirTemp(r.tmp(), "git-*")
		if err != nil {
			return "", nil, err
		}
		cleanup := func() { os.RemoveAll(dir) }
		_, err = r.Runner.Run(ctx, cmdName(ref.Fetch, "source_git"), map[string]string{
			"GitRepo": ref.Repo, "GitRef": ref.Ref, "LocalPath": dir,
		})
		if err != nil {
			return "", cleanup, failure.Wrap(failure.HostError, "git fetch", err)
		}
		cv.PinnedAt = pinGit(dir, ref.Ref)
		sub := dir
		if ref.Path != "" {
			sub = filepath.Join(dir, ref.Path)
			if fi, err := os.Stat(sub); err != nil || !fi.IsDir() {
				return "", cleanup, failure.New(failure.HostError, "path not found in repo: "+ref.Path)
			}
		}
		return sub, cleanup, nil

	case "object":
		if !r.Runner.Has(cmdName(ref.Fetch, "storage_download")) {
			return "", nil, failure.New(failure.ObjectStoreError, "no storage_download command configured")
		}
		dir, err := os.MkdirTemp(r.tmp(), "obj-*")
		if err != nil {
			return "", nil, err
		}
		cleanup := func() { os.RemoveAll(dir) }
		arc := filepath.Join(dir, "case.tar")
		if _, err := r.Runner.Run(ctx, cmdName(ref.Fetch, "storage_download"), map[string]string{
			"Key": ref.Key, "LocalPath": arc,
		}); err != nil {
			return "", cleanup, failure.Wrap(failure.ObjectStoreError, "download", err)
		}
		out := filepath.Join(dir, "unpacked")
		if err := os.MkdirAll(out, 0o755); err != nil {
			return "", cleanup, err
		}
		if err := cas.Unpack(arc, out); err != nil {
			return "", cleanup, failure.Wrap(failure.HostError, "unpack object", err)
		}
		cv.PinnedAt = ref.Key
		return out, cleanup, nil
	}
	return "", nil, failure.New(failure.HostError, "unknown case source "+strconv(ref.Source))
}

// materialise unpacks the CAS object and parses task.toml.
func (r *Resolver) materialise(cv *CaseVersion) error {
	dir := filepath.Join(r.WorkRoot, "cases", cv.SHA256)
	if _, err := os.Stat(filepath.Join(dir, "task.toml")); err != nil {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return failure.Wrap(failure.HostError, "case dir", err)
		}
		if err := cas.Unpack(r.Store.Path(cv.SHA256), dir); err != nil {
			return failure.Wrap(failure.HostError, "unpack case", err)
		}
	}
	cv.Dir = dir
	cfg, err := casedef.Load(dir)
	if err != nil {
		return failure.Wrap(failure.HostError, "parse task.toml", err)
	}
	cv.Cfg = cfg
	cv.State = Ready
	return nil
}

func (r *Resolver) invalid(cv *CaseVersion, err error) (*CaseVersion, error) {
	cv.State = Invalid
	cv.Error = err.Error()
	return cv, err
}

func (r *Resolver) tmp() string {
	d := filepath.Join(r.WorkRoot, "tmp")
	os.MkdirAll(d, 0o755)
	return d
}

func cmdName(explicit, def string) string {
	if explicit != "" {
		return explicit
	}
	return def
}

func strconv(s string) string {
	if s == "" {
		return "(empty)"
	}
	return s
}

// pinGit records the immutable commit a mutable ref resolved to.
func pinGit(dir, ref string) string {
	b, err := os.ReadFile(filepath.Join(dir, ".git", "HEAD"))
	if err != nil {
		return ref
	}
	s := strings.TrimSpace(string(b))
	if strings.HasPrefix(s, "ref: ") {
		if rb, err := os.ReadFile(filepath.Join(dir, ".git", strings.TrimPrefix(s, "ref: "))); err == nil {
			s = strings.TrimSpace(string(rb))
		}
	}
	if len(s) >= 7 && !strings.Contains(s, " ") {
		return s
	}
	return ref
}
