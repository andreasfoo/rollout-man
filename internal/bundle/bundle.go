// Package bundle packs a trial's artifacts into one archive.
package bundle

import (
	"archive/tar"
	"compress/gzip"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
)

// Create packs the named files from dir into dst. format is advisory: tar.zst
// is used when a zstd binary exists, otherwise it falls back to tar.gz and the
// caller is told which was produced.
func Create(dir string, names []string, dst, format string) (string, error) {
	if format == "tar.zst" {
		if _, err := exec.LookPath("zstd"); err != nil {
			format = "tar.gz"
			dst = trimExt(dst) + ".tar.gz"
		}
	}
	sort.Strings(names)
	f, err := os.Create(dst)
	if err != nil {
		return "", err
	}
	defer f.Close()

	var w io.WriteCloser
	switch format {
	case "tar.zst":
		cmd := exec.Command("zstd", "-q", "-o", dst)
		pr, pw := io.Pipe()
		cmd.Stdin = pr
		if err := cmd.Start(); err != nil {
			return "", err
		}
		w = pw
		defer func() { pw.Close(); cmd.Wait() }()
	default:
		w = gzip.NewWriter(f)
		defer w.Close()
	}

	tw := tar.NewWriter(w)
	for _, n := range names {
		p := filepath.Join(dir, n)
		fi, err := os.Stat(p)
		if err != nil {
			continue // an optional artifact that this trial did not produce
		}
		hdr, err := tar.FileInfoHeader(fi, "")
		if err != nil {
			return "", err
		}
		hdr.Name = n
		hdr.Uid, hdr.Gid, hdr.Uname, hdr.Gname = 0, 0, "", ""
		if err := tw.WriteHeader(hdr); err != nil {
			return "", err
		}
		src, err := os.Open(p)
		if err != nil {
			return "", err
		}
		_, err = io.Copy(tw, src)
		src.Close()
		if err != nil {
			return "", err
		}
	}
	if err := tw.Close(); err != nil {
		return "", fmt.Errorf("close tar: %w", err)
	}
	return dst, nil
}

func trimExt(p string) string {
	for _, e := range []string{".tar.zst", ".tar.gz", ".tgz"} {
		if len(p) > len(e) && p[len(p)-len(e):] == e {
			return p[:len(p)-len(e)]
		}
	}
	return p
}
