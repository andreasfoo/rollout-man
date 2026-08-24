// Package archive packs a directory into one file.
//
// The format is a real decision, not a preference. `tar` is what a dataset hub
// reads as WebDataset -- the viewer, load_dataset and streaming all understand
// it. `zip` has no builder in the datasets library at all, so a directory of
// zips publishes as a pile of opaque blobs rather than a dataset. Both are
// available because "I want one archive per directory" is a legitimate ask;
// only one of them stays a dataset.
package archive

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"time"
)

var zeroTime = time.Unix(0, 0).UTC()

// Pack writes dir into an archive at dst. include, when non-empty, keeps only
// files whose base name matches one of the given patterns.
//
// Entries are sorted and their timestamps zeroed, so packing the same bytes
// twice produces the same archive. An archive that changes when nothing changed
// is a diff on every publish and a second copy in LFS.
func Pack(dst, dir, format string, include []string) (int, error) {
	files, err := collect(dir, include)
	if err != nil {
		return 0, err
	}
	f, err := os.Create(dst)
	if err != nil {
		return 0, err
	}
	defer f.Close()

	switch format {
	case "zip":
		return len(files), writeZip(f, dir, files)
	case "tar":
		return len(files), writeTar(f, dir, files)
	case "tar.gz":
		gz := gzip.NewWriter(f)
		if err := writeTar(gz, dir, files); err != nil {
			return 0, err
		}
		return len(files), gz.Close()
	}
	return 0, fmt.Errorf("unknown archive format %q", format)
}

func collect(dir string, include []string) ([]string, error) {
	var out []string
	err := filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		if len(include) > 0 && !matches(filepath.Base(p), include) {
			return nil
		}
		rel, rerr := filepath.Rel(dir, p)
		if rerr != nil {
			return rerr
		}
		out = append(out, rel)
		return nil
	})
	sort.Strings(out)
	return out, err
}

func matches(name string, pats []string) bool {
	for _, p := range pats {
		if ok, _ := filepath.Match(p, name); ok {
			return true
		}
	}
	return false
}

func writeTar(w io.Writer, dir string, files []string) error {
	tw := tar.NewWriter(w)
	for _, rel := range files {
		b, err := os.ReadFile(filepath.Join(dir, rel))
		if err != nil {
			return err
		}
		if err := tw.WriteHeader(&tar.Header{
			Name: rel, Mode: 0o644, Size: int64(len(b)),
			ModTime: zeroTime, Format: tar.FormatUSTAR,
		}); err != nil {
			return err
		}
		if _, err := tw.Write(b); err != nil {
			return err
		}
	}
	return tw.Close()
}

func writeZip(w io.Writer, dir string, files []string) error {
	zw := zip.NewWriter(w)
	for _, rel := range files {
		b, err := os.ReadFile(filepath.Join(dir, rel))
		if err != nil {
			return err
		}
		h := &zip.FileHeader{Name: rel, Method: zip.Deflate}
		h.SetMode(0o644)
		h.Modified = zeroTime
		fw, err := zw.CreateHeader(h)
		if err != nil {
			return err
		}
		if _, err := fw.Write(b); err != nil {
			return err
		}
	}
	return zw.Close()
}
