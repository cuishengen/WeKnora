package dokuwiki

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Tencent/WeKnora/internal/datasource"
	"github.com/Tencent/WeKnora/internal/types"
)

type recordingHandler struct {
	items       []types.FetchedItem
	checkpoints []*types.SyncCursor
	failAt      int
	onEmit      func(types.FetchedItem)
	emitError   func(types.FetchedItem) error
}

func (h *recordingHandler) Emit(_ context.Context, item types.FetchedItem) error {
	if h.emitError != nil {
		if err := h.emitError(item); err != nil {
			return err
		}
	}
	if h.failAt > 0 && len(h.items)+1 == h.failAt {
		return errors.New("ingestion failed")
	}
	h.items = append(h.items, item)
	if h.onEmit != nil {
		h.onEmit(item)
	}
	return nil
}
func (h *recordingHandler) Checkpoint(_ context.Context, cursor *types.SyncCursor) error {
	h.checkpoints = append(h.checkpoints, cursor)
	return nil
}

func fixture(t *testing.T) (*Connector, *types.DataSourceConfig, string) {
	t.Helper()
	root := t.TempDir()
	put(t, root, "pages/release/start.txt", []byte("====== Release ======\n[[release:chips|Chips]]\n"))
	put(t, root, "pages/release/chips/spec.txt", []byte("====== 芯片 ======\n^ 型号 ^ RAM ^\n| 示例 | 128KB |\n"))
	put(t, root, "media/release/chips/manual.pdf", []byte("%PDF-1.7\noriginal bytes\x00\xff"))
	put(t, root, "media/release/chips/archive.zip", []byte("unexpanded archive"))
	put(t, root, "media/release/chips/image.png", []byte("unrecognized image"))
	c, err := NewConnector(map[string]Source{"vendor-wiki": {DataRoot: root, PublicBaseURL: "https://example.test/wiki", Sentinel: "pages/release/start.txt"}})
	if err != nil {
		t.Fatal(err)
	}
	cfg := &types.DataSourceConfig{Type: types.ConnectorTypeDokuWikiFilesystem, Settings: map[string]interface{}{"source_alias": "vendor-wiki"}, ResourceIDs: []string{"namespace:"}}
	return c, cfg, root
}

func put(t *testing.T, root, rel string, data []byte) {
	t.Helper()
	p := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, data, 0600); err != nil {
		t.Fatal(err)
	}
}

func TestOriginalBytesIncrementalAndExclusionManifest(t *testing.T) {
	c, cfg, root := fixture(t)
	ctx := context.Background()
	items, cursor, err := c.FetchIncremental(ctx, cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 2 {
		t.Fatalf("got %d parsed candidates, want wiki table and PDF", len(items))
	}
	for _, item := range items {
		raw, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(item.Metadata["relative_path"])))
		if err != nil || string(raw) != string(item.Content) || digest(raw) != item.Metadata["sha256"] {
			t.Fatal("source payload/hash changed")
		}
		if item.IsDeleted || item.FileName != filepath.Base(item.Metadata["relative_path"]) || !strings.HasPrefix(item.URL, "https://example.test/wiki/") {
			t.Fatal("original name, provenance or deletion state is invalid")
		}
		if err := c.VerifyFetchedItem(ctx, cfg, &item); err != nil {
			t.Fatal(err)
		}
	}
	excluded := cursor.ConnectorCursor["excluded"].(map[string]interface{})
	if len(excluded) != 3 || excluded["media/release/chips/archive.zip"] != "unsupported_format_not_parsed" {
		t.Fatalf("exclusion manifest incomplete: %v", excluded)
	}
	unchanged, _, err := c.FetchIncremental(ctx, cfg, cursor)
	if err != nil || len(unchanged) != 0 {
		t.Fatalf("unchanged source reemitted: %d %v", len(unchanged), err)
	}
	put(t, root, "pages/release/chips/spec.txt", []byte("Changed specification, RAM now explicitly documented."))
	if err := c.VerifyFetchedItem(ctx, cfg, &items[0]); err == nil {
		t.Fatal("changed original passed publication guard")
	}
	changed, _, err := c.FetchIncremental(ctx, cfg, cursor)
	if err != nil || len(changed) != 1 || changed[0].ExternalID != items[0].ExternalID {
		t.Fatalf("stable identity/update mismatch: %d %v", len(changed), err)
	}
	if err := os.Remove(filepath.Join(root, "media/release/chips/manual.pdf")); err != nil {
		t.Fatal(err)
	}
	remaining, _, err := c.FetchIncremental(ctx, cfg, cursor)
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range remaining {
		if item.IsDeleted {
			t.Fatal("unsupported deletion emitted")
		}
	}
}

func TestCheckpointOnlyAcknowledgesSuccessfulEmit(t *testing.T) {
	c, cfg, _ := fixture(t)
	h := &recordingHandler{failAt: 2}
	_, err := c.FetchStream(context.Background(), cfg, nil, h)
	if err == nil || len(h.items) != 1 || len(h.checkpoints) != 1 {
		t.Fatalf("failure not propagated: %v", err)
	}
	checkpoint := h.checkpoints[0]
	if len(checkpoint.ConnectorCursor["files"].(map[string]interface{})) != 1 || checkpoint.ConnectorCursor["scan_complete"] != false {
		t.Fatal("checkpoint acknowledged failed/unscanned content")
	}
	resume := &recordingHandler{}
	if _, err := c.FetchStream(context.Background(), cfg, checkpoint, resume); err != nil {
		t.Fatal(err)
	}
	if len(resume.items) != 1 || resume.items[0].ExternalID == h.items[0].ExternalID {
		t.Fatal("resume lost or repeated successful publication")
	}
}

func TestDocumentContentFailureContinuesAndRetriesWithoutAcknowledgement(t *testing.T) {
	c, cfg, root := fixture(t)
	ctx := context.Background()
	_, baseline, err := c.FetchIncremental(ctx, cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	const failedPath = "pages/release/chips/spec.txt"
	oldHash := baseline.ConnectorCursor["files"].(map[string]interface{})[failedPath]
	put(t, root, failedPath, []byte("Changed specification rejected by its parser."))
	put(t, root, "media/release/chips/later.txt", []byte("This later document must still be ingested."))
	h := &recordingHandler{emitError: func(item types.FetchedItem) error {
		if item.Metadata["relative_path"] == failedPath {
			return &datasource.DocumentParseFailure{KnowledgeID: "candidate", Reason: "malformed document"}
		}
		return nil
	}}
	next, err := c.FetchStream(ctx, cfg, baseline, h)
	if err != nil || len(h.items) != 1 || h.items[0].FileName != "later.txt" {
		t.Fatalf("content failure blocked later document: %v", err)
	}
	if next.ConnectorCursor["files"].(map[string]interface{})[failedPath] != oldHash {
		t.Fatal("failed update acknowledged a new hash")
	}
	if next.ConnectorCursor["failed"].(map[string]interface{})[failedPath] != "malformed document" {
		t.Fatal("failure manifest missing")
	}
	if next.ConnectorCursor["scan_complete"] != true {
		t.Fatal("completed traversal was not recorded")
	}
	retry := &recordingHandler{}
	recovered, err := c.FetchStream(ctx, cfg, next, retry)
	if err != nil || len(retry.items) != 1 || retry.items[0].Metadata["relative_path"] != failedPath {
		t.Fatalf("failed source was not retried exclusively: %v", err)
	}
	if len(recovered.ConnectorCursor["failed"].(map[string]interface{})) != 0 {
		t.Fatal("successful retry retained stale failure")
	}
}

func TestPathsNamespacesAndServerPolicy(t *testing.T) {
	c, cfg, root := fixture(t)
	ctx := context.Background()
	for _, id := range []string{"../../secret", "namespace:../secret", "namespace:release::chips", "namespace:release/../secret", "namespace:C:\\secret"} {
		cfg.ResourceIDs = []string{id}
		if err := c.Validate(ctx, cfg); err == nil {
			t.Fatalf("accepted traversal resource %q", id)
		}
	}
	cfg.ResourceIDs = []string{"namespace:release:chips"}
	resources, err := c.ListResources(ctx, cfg, "namespace:release")
	if err != nil || len(resources) != 1 || resources[0].ExternalID != "namespace:release:chips" {
		t.Fatalf("namespace tree: %v %v", resources, err)
	}
	cfg.Settings["data_root"] = root
	if err := c.Validate(ctx, cfg); err == nil {
		t.Fatal("accepted browser host path")
	}
	delete(cfg.Settings, "data_root")
	cfg.Settings["source_alias"] = "unknown"
	if err := c.Validate(ctx, cfg); err == nil {
		t.Fatal("accepted unlisted alias")
	}
	cfg.Settings["source_alias"] = "vendor-wiki"
	src := c.sources["vendor-wiki"]
	src.DeniedNamespaces = []string{"release:chips"}
	c.sources["vendor-wiki"] = src
	items, _, err := c.FetchIncremental(ctx, cfg, nil)
	if err != nil || len(items) != 0 {
		t.Fatalf("server deny bypassed: %d %v", len(items), err)
	}
}

func TestMissingMountSymlinkAndChangingScanFailClosed(t *testing.T) {
	t.Run("sentinel", func(t *testing.T) {
		c, cfg, root := fixture(t)
		if err := os.Remove(filepath.Join(root, "pages/release/start.txt")); err != nil {
			t.Fatal(err)
		}
		h := &recordingHandler{}
		if _, err := c.FetchStream(context.Background(), cfg, nil, h); err == nil || len(h.items) != 0 {
			t.Fatal("missing mount sentinel was accepted")
		}
	})
	t.Run("symlink", func(t *testing.T) {
		c, cfg, root := fixture(t)
		if err := os.Symlink(t.TempDir(), filepath.Join(root, "media", "escape")); err != nil {
			t.Skip("symlink privilege unavailable")
		}
		h := &recordingHandler{}
		if _, err := c.FetchStream(context.Background(), cfg, nil, h); err == nil || len(h.items) != 0 {
			t.Fatal("symlink was accepted")
		}
	})
	t.Run("mutating scan", func(t *testing.T) {
		c, cfg, root := fixture(t)
		h := &recordingHandler{onEmit: func(types.FetchedItem) { put(t, root, "media/new.png", []byte("arrived during scan")) }}
		if _, err := c.FetchStream(context.Background(), cfg, nil, h); err == nil {
			t.Fatal("unstable scan marked complete")
		}
		for _, cursor := range h.checkpoints {
			if cursor.ConnectorCursor["scan_complete"] == true {
				t.Fatal("unstable scan checkpoint marked complete")
			}
		}
	})
}

func TestACLChangeStopsFetchAndPublication(t *testing.T) {
	c, cfg, root := fixture(t)
	aclPath := filepath.Join(t.TempDir(), "acl.auth.php")
	acl := []byte("* @ALL 0\n* @developer 1\n")
	if err := os.WriteFile(aclPath, acl, 0600); err != nil {
		t.Fatal(err)
	}
	src := c.sources["vendor-wiki"]
	src.ACLPath, src.ExpectedACLSHA256 = aclPath, digest(acl)
	c.sources["vendor-wiki"] = src
	items, _, err := c.FetchIncremental(context.Background(), cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(aclPath, []byte("* @ALL 0\n* @developer 0\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := c.Validate(context.Background(), cfg); err == nil {
		t.Fatal("changed ACL passed validation")
	}
	if _, err := c.ListResources(context.Background(), cfg, ""); err == nil {
		t.Fatal("changed ACL exposed resource tree")
	}
	if err := c.VerifyFetchedItem(context.Background(), cfg, &items[0]); err == nil {
		t.Fatal("changed ACL passed publication guard")
	}
	put(t, root, "pages/release/chips/spec.txt", []byte("irrelevant changed bytes"))
	h := &recordingHandler{}
	if _, err := c.FetchStream(context.Background(), cfg, nil, h); err == nil || len(h.items) != 0 {
		t.Fatal("changed ACL emitted content")
	}
}
