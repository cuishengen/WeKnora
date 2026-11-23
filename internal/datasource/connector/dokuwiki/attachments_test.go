package dokuwiki

import (
	"context"
	"encoding/json"
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

func TestFilesystemSourceIsBoundToItsAuthorizedWorkspace(t *testing.T) {
	c, cfg, _ := fixture(t)
	source := c.sources["vendor-wiki"]
	source.AllowedTenantIDs = []uint64{10000}
	c.sources["vendor-wiki"] = source
	owner := context.WithValue(context.Background(), types.TenantIDContextKey, uint64(10000))
	other := context.WithValue(context.Background(), types.TenantIDContextKey, uint64(10001))
	if err := c.Validate(owner, cfg); err != nil {
		t.Fatal(err)
	}
	if err := c.Validate(other, cfg); err == nil {
		t.Fatal("another workspace could read the mounted source")
	}
	if _, err := c.ListResources(other, cfg, ""); err == nil {
		t.Fatal("another workspace could list the mounted source")
	}
	if _, _, err := c.FetchIncremental(other, cfg, nil); err == nil {
		t.Fatal("another workspace could ingest the mounted source")
	}
	delete(cfg.Settings, "file_types")
	items, _, err := c.FetchIncremental(owner, cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range items {
		if item.SourceReference != nil {
			if _, _, err := c.OpenReference(other, item.SourceReference); err == nil {
				t.Fatal("another workspace could download the source original")
			}
		}
	}
	raw, _ := json.Marshal(map[string]Source{"vendor-wiki": source})
	t.Setenv(SourcesEnv, string(raw))
	if _, err := NewConnectorFromEnv(); err != nil {
		t.Fatal(err)
	}
	source.AllowedTenantIDs = nil
	raw, _ = json.Marshal(map[string]Source{"vendor-wiki": source})
	t.Setenv(SourcesEnv, string(raw))
	if _, err := NewConnectorFromEnv(); err == nil {
		t.Fatal("product source silently accepted an unscoped workspace policy")
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
