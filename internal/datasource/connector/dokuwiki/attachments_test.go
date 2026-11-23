package dokuwiki

import (
	"context"
	"io"
	"strings"
	"testing"

	"github.com/Tencent/WeKnora/internal/types"
)

func TestAttachmentCatalogOriginalDownloadAndVersionGuards(t *testing.T) {
	c, cfg, root := fixture(t)
	delete(cfg.Settings, "file_types")
	items, cursor, err := c.FetchIncremental(context.Background(), cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	if cursor.ConnectorCursor["inventory"].(map[string]interface{})["source_files"].(float64) != 5 {
		t.Fatal("inventory did not account for every source file")
	}
	var attachment *types.FetchedItem
	for i := range items {
		if items[i].SourceReference != nil {
			attachment = &items[i]
		}
	}
	if attachment == nil || !strings.Contains(string(attachment.Content), "未解压") {
		t.Fatal("archive must have an explicit download-only catalogue")
	}
	if err := c.VerifyFetchedItem(context.Background(), cfg, attachment); err != nil {
		t.Fatal(err)
	}
	file, name, err := c.OpenReference(context.Background(), attachment.SourceReference)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(file)
	file.Close()
	if name != "archive.zip" || string(raw) != "unexpanded archive" {
		t.Fatal("download did not return the original bytes")
	}
	for _, rel := range []string{"../outside", "/etc/passwd", "pages/release/start.txt", "media/../private.zip"} {
		ref := *attachment.SourceReference
		ref.RelativePath = rel
		if _, _, err := c.OpenReference(context.Background(), &ref); err == nil {
			t.Fatalf("unsafe reference accepted: %s", rel)
		}
	}
	put(t, root, "media/release/chips/archive.zip", []byte("new archive version"))
	if _, _, err := c.OpenReference(context.Background(), attachment.SourceReference); err == nil {
		t.Fatal("old reference silently returned a new version")
	}
}

func TestSourceReferencesRespectCurrentNamespacePolicy(t *testing.T) {
	c, cfg, _ := fixture(t)
	delete(cfg.Settings, "file_types")
	items, _, err := c.FetchIncremental(context.Background(), cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range items {
		if item.SourceReference != nil {
			source := c.sources["vendor-wiki"]
			source.DeniedNamespaces = []string{"release:chips"}
			c.sources["vendor-wiki"] = source
			if _, _, err := c.OpenReference(context.Background(), item.SourceReference); err == nil {
				t.Fatal("restricted attachment was readable")
			}
		}
	}
}
