// Package cas is the content-addressed store. Packing is deterministic so
// identical trees always hash identically.
package cas

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

var zeroTime = time.Unix(0, 0).UTC()

type Store struct{ Root string }

func New(root string) (*Store, error) {
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, err
	}
	return &Store{Root: root}, nil
}

func (s *Store) Path(sha string) string {
	if len(sha) < 4 {
		return filepath.Join(s.Root, sha)
	}
	return filepath.Join(s.Root, sha[:2], sha[2:4], sha)
}

func (s *Store) Has(sha string) bool {
	_, err := os.Stat(s.Path(sha))
	return err == nil
}

func (s *Store) Put(tmpTar, sha string) error {
	dst := s.Path(sha)
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	if s.Has(sha) {
		return os.Remove(tmpTar)
	}
	return os.Rename(tmpTar, dst)
}

// PackDir writes a deterministic tar of dir and returns its path and sha256.
func PackDir(dir, tmpDir string) (string, string, error) {
	var entries []string
	err := filepath.Walk(dir, func(p string, fi os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		if fi.IsDir() && (rel == ".git" || strings.HasPrefix(rel, ".git"+string(filepath.Separator))) {
			return filepath.SkipDir
		}
		entries = append(entries, rel)
		return nil
	})
	if err != nil {
		return "", "", err
	}
	sort.Strings(entries)

	f, err := os.CreateTemp(tmpDir, "pack-*.tar")
	if err != nil {
		return "", "", err
	}
	defer f.Close()
	h := sha256.New()
	tw := tar.NewWriter(io.MultiWriter(f, h))

	for _, rel := range entries {
		full := filepath.Join(dir, rel)
		fi, err := os.Lstat(full)
		if err != nil {
			return "", "", err
		}
		link := ""
		if fi.Mode()&os.ModeSymlink != 0 {
			if link, err = os.Readlink(full); err != nil {
				return "", "", err
			}
		}
		hdr, err := tar.FileInfoHeader(fi, link)
		if err != nil {
			return "", "", err
		}
		hdr.Name = filepath.ToSlash(rel)
		hdr.Uid, hdr.Gid, hdr.Uname, hdr.Gname = 0, 0, "", ""
		hdr.ModTime = zeroTime
		hdr.AccessTime, hdr.ChangeTime = time.Time{}, time.Time{}
		hdr.Format = tar.FormatUSTAR
		if fi.Mode().IsRegular() && fi.Mode()&0o111 != 0 {
			hdr.Mode = 0o755
		} else if fi.Mode().IsRegular() {
			hdr.Mode = 0o644
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return "", "", err
		}
		if fi.Mode().IsRegular() {
			src, err := os.Open(full)
			if err != nil {
				return "", "", err
			}
			_, err = io.Copy(tw, src)
			src.Close()
			if err != nil {
				return "", "", err
			}
		}
	}
	if err := tw.Close(); err != nil {
		return "", "", err
	}
	return f.Name(), hex.EncodeToString(h.Sum(nil)), nil
}

// Unpack extracts a tar (optionally gzipped) into dst.
func Unpack(tarPath, dst string) error {
	f, err := os.Open(tarPath)
	if err != nil {
		return err
	}
	defer f.Close()
	var r io.Reader = f
	if strings.HasSuffix(tarPath, ".gz") || strings.HasSuffix(tarPath, ".tgz") {
		gz, err := gzip.NewReader(f)
		if err != nil {
			return err
		}
		defer gz.Close()
		r = gz
	}
	tr := tar.NewReader(r)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		name := filepath.Clean(hdr.Name)
		if strings.HasPrefix(name, "..") || filepath.IsAbs(name) {
			return fmt.Errorf("unsafe path in archive: %s", hdr.Name)
		}
		target := filepath.Join(dst, name)
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
		case tar.TypeSymlink:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			os.Remove(target)
			if err := os.Symlink(hdr.Linkname, target); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			out, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, os.FileMode(hdr.Mode))
			if err != nil {
				return err
			}
			if _, err := io.Copy(out, tr); err != nil {
				out.Close()
				return err
			}
			out.Close()
		}
	}
}

func FileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
