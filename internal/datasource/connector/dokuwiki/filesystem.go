package dokuwiki

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/Tencent/WeKnora/internal/datasource"
)

type sourceFS struct {
	root     *os.Root
	source   Source
	identity os.FileInfo
}

type entry struct {
	rel       string
	namespace string
	info      os.FileInfo
}

// os.Root contains opens even when a directory is replaced while scanning.
// Lstat checks additionally refuse links inside the root, not just escapes.
func openSource(src Source) (*sourceFS, error) {
	info, err := os.Lstat(src.DataRoot)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("%w: DokuWiki source mount is unavailable or is a symbolic link", datasource.ErrFetchFailed)
	}
	root, err := os.OpenRoot(src.DataRoot)
	if err != nil {
		return nil, fmt.Errorf("%w: cannot open DokuWiki source mount", datasource.ErrFetchFailed)
	}
	s := &sourceFS{root: root, source: src, identity: info}
	if err := s.healthy(); err != nil {
		root.Close()
		return nil, err
	}
	return s, nil
}

func (s *sourceFS) healthy() error {
	if err := checkACL(s.source); err != nil {
		return err
	}
	info, err := os.Lstat(s.source.DataRoot)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || !os.SameFile(s.identity, info) {
		return fmt.Errorf("%w: DokuWiki source mount changed or disappeared", datasource.ErrFetchFailed)
	}
	for _, dir := range []string{"pages", "media"} {
		info, err := s.noLinks(dir)
		if err != nil || !info.IsDir() {
			return fmt.Errorf("%w: DokuWiki pages/media directories are unavailable", datasource.ErrFetchFailed)
		}
	}
	info, err = s.noLinks(s.source.Sentinel)
	if err != nil || !info.Mode().IsRegular() || info.Size() == 0 {
		return fmt.Errorf("%w: DokuWiki nonempty mount sentinel is unavailable", datasource.ErrFetchFailed)
	}
	return nil
}

// The optional separately mounted ACL file is only fingerprinted. No user
// database or other conf files are opened; mapping ACLs to members is external.
func checkACL(src Source) error {
	if src.ACLPath == "" {
		return nil
	}
	before, err := os.Lstat(src.ACLPath)
	if err != nil || !before.Mode().IsRegular() || before.Size() > 1024*1024 {
		return fmt.Errorf("%w: DokuWiki ACL guard is unavailable", datasource.ErrFetchFailed)
	}
	r, err := os.OpenRoot(filepath.Dir(src.ACLPath))
	if err != nil {
		return fmt.Errorf("%w: DokuWiki ACL guard is unavailable", datasource.ErrFetchFailed)
	}
	defer r.Close()
	f, err := r.Open(filepath.Base(src.ACLPath))
	if err != nil {
		return fmt.Errorf("%w: DokuWiki ACL guard is unavailable", datasource.ErrFetchFailed)
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil || !sameEntry(before, opened) {
		return fmt.Errorf("%w: DokuWiki ACL guard changed", datasource.ErrFetchFailed)
	}
	b, err := io.ReadAll(io.LimitReader(f, 1024*1024+1))
	after, statErr := os.Lstat(src.ACLPath)
	sum := sha256.Sum256(b)
	if err != nil || statErr != nil || !sameEntry(before, after) ||
		int64(len(b)) != before.Size() || !strings.EqualFold(hex.EncodeToString(sum[:]), src.ExpectedACLSHA256) {
		return fmt.Errorf("%w: DokuWiki ACL changed; stop synchronization and review permissions", datasource.ErrFetchFailed)
	}
	return nil
}

func (s *sourceFS) noLinks(rel string) (os.FileInfo, error) {
	if !safePath(rel) {
		return nil, datasource.ErrInvalidConfig
	}
	var info os.FileInfo
	var err error
	parts := strings.Split(rel, "/")
	for i := range parts {
		info, err = s.root.Lstat(strings.Join(parts[:i+1], "/"))
		if err != nil || info.Mode()&os.ModeSymlink != 0 {
			return nil, fmt.Errorf("%w: DokuWiki source entry unavailable or symbolic link: %s", datasource.ErrFetchFailed, rel)
		}
	}
	return info, nil
}

func (s *sourceFS) inventory(ctx context.Context) ([]entry, error) {
	if err := s.healthy(); err != nil {
		return nil, err
	}
	var entries []entry
	for _, area := range []string{"pages", "media"} {
		err := fs.WalkDir(s.root.FS(), area, func(rel string, d fs.DirEntry, walkErr error) error {
			if err := ctx.Err(); err != nil {
				return err
			}
			if walkErr != nil {
				return fmt.Errorf("%w: DokuWiki directory scan failed", datasource.ErrFetchFailed)
			}
			info, err := s.noLinks(rel)
			if err != nil {
				return err
			}
			if d.IsDir() {
				return nil
			}
			if !info.Mode().IsRegular() {
				return fmt.Errorf("%w: DokuWiki source is not a regular file: %s", datasource.ErrFetchFailed, rel)
			}
			ns := path.Dir(strings.TrimPrefix(rel, area+"/"))
			if ns == "." {
				ns = ""
			}
			entries = append(entries, entry{rel: rel, namespace: strings.ReplaceAll(ns, "/", ":"), info: info})
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	return entries, nil
}

func sameEntry(a, b os.FileInfo) bool {
	return os.SameFile(a, b) && a.Mode() == b.Mode() && a.Size() == b.Size() && a.ModTime().Equal(b.ModTime())
}

func (s *sourceFS) read(e entry, maxBytes int64) ([]byte, error) {
	before, err := s.noLinks(e.rel)
	if err != nil || !sameEntry(e.info, before) {
		return nil, fmt.Errorf("%w: DokuWiki source changed before read: %s", datasource.ErrFetchFailed, e.rel)
	}
	f, err := s.root.Open(e.rel)
	if err != nil {
		return nil, fmt.Errorf("%w: cannot read DokuWiki source: %s", datasource.ErrFetchFailed, e.rel)
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil || !sameEntry(e.info, opened) {
		return nil, fmt.Errorf("%w: DokuWiki source changed during open: %s", datasource.ErrFetchFailed, e.rel)
	}
	b, err := io.ReadAll(io.LimitReader(f, maxBytes+1))
	if err != nil || int64(len(b)) > maxBytes {
		return nil, fmt.Errorf("%w: DokuWiki source read failed or exceeds file size limit: %s", datasource.ErrFetchFailed, e.rel)
	}
	after, err := s.noLinks(e.rel)
	if err != nil || !sameEntry(e.info, after) || int64(len(b)) != e.info.Size() {
		return nil, fmt.Errorf("%w: DokuWiki source changed during read: %s", datasource.ErrFetchFailed, e.rel)
	}
	return b, nil
}

func sameInventory(before, after []entry) bool {
	if len(before) != len(after) {
		return false
	}
	for i := range before {
		if before[i].rel != after[i].rel || !sameEntry(before[i].info, after[i].info) {
			return false
		}
	}
	return true
}
