package dokuwiki

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"path"
	"strings"

	"github.com/Tencent/WeKnora/internal/datasource"
	"github.com/Tencent/WeKnora/internal/types"
)

func (s *sourceFS) hashFile(ctx context.Context, e entry) (string, error) {
	f, err := s.root.Open(e.rel)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	buffer := make([]byte, 1024*1024)
	for {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		n, err := f.Read(buffer)
		if n > 0 {
			_, _ = h.Write(buffer[:n])
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", err
		}
	}
	current, err := s.noLinks(e.rel)
	if err != nil || !sameEntry(e.info, current) {
		return "", datasource.ErrFetchFailed
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// OpenReference is reached after knowledge/KB authorization. Recheck the
// server namespace policy, ACL and indexed version before serving the original.
func (c *Connector) OpenReference(ctx context.Context, ref *types.SourceFileReference) (io.ReadCloser, string, error) {
	if ref == nil || !safePath(ref.RelativePath) || !strings.HasPrefix(ref.RelativePath, "media/") || !validDigest(ref.SHA256) {
		return nil, "", datasource.ErrInvalidConfig
	}
	cfg, err := c.parseConfig(&types.DataSourceConfig{Settings: map[string]interface{}{"source_alias": ref.Alias}, ResourceIDs: []string{"namespace:"}})
	if err != nil {
		return nil, "", err
	}
	if err := cfg.authorize(ctx); err != nil {
		return nil, "", err
	}
	ns := strings.ReplaceAll(path.Dir(strings.TrimPrefix(ref.RelativePath, "media/")), "/", ":")
	if ns == "." {
		ns = ""
	}
	if !cfg.allowed(ns) {
		return nil, "", datasource.ErrInvalidConfig
	}
	s, err := openSource(cfg.source)
	if err != nil {
		return nil, "", err
	}
	defer s.root.Close()
	info, err := s.noLinks(ref.RelativePath)
	if err != nil || !info.Mode().IsRegular() || info.Size() != ref.Size {
		return nil, "", datasource.ErrFetchFailed
	}
	hash, err := s.hashFile(ctx, entry{rel: ref.RelativePath, info: info})
	if err != nil || hash != ref.SHA256 {
		return nil, "", fmt.Errorf("source attachment changed; synchronize its new version before downloading")
	}
	f, err := s.root.Open(ref.RelativePath)
	if err != nil {
		return nil, "", err
	}
	opened, err := f.Stat()
	if err != nil || !sameEntry(info, opened) {
		f.Close()
		return nil, "", datasource.ErrFetchFailed
	}
	if err := s.healthy(); err != nil {
		f.Close()
		return nil, "", err
	}
	return f, path.Base(ref.RelativePath), nil
}

func referenceItem(cfg *selection, e entry, hash string) types.FetchedItem {
	item := fetchedItem(cfg, e, nil, hash)
	item.SourceReference = &types.SourceFileReference{Alias: cfg.alias, RelativePath: e.rel, SHA256: hash, Size: e.info.Size()}
	item.FileName += ".catalog.txt"
	item.ContentType = "text/plain"
	item.Content = []byte(fmt.Sprintf("原件附件目录：%s\n来源目录：%s\n原始大小：%d 字节\n来源：%s\n仅登记原件供查找和下载，未解压、未读取压缩包内部文件，也不能据此回答包内技术内容。\n", path.Base(e.rel), e.namespace, e.info.Size(), item.URL))
	return item
}

func imageFile(ext string) bool {
	switch ext {
	case "png", "jpg", "jpeg", "gif", "bmp", "tiff", "webp":
		return true
	}
	return false
}
