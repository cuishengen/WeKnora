package dto

import (
	"encoding/json"
	"testing"

	"github.com/Tencent/WeKnora/internal/types"
	"github.com/stretchr/testify/assert"
)

func TestDataSourceResponse_OmitsCredentials(t *testing.T) {
	cfg := types.DataSourceConfig{
		Type: "github",
		Credentials: map[string]interface{}{
			"token": "ghp-secret-do-not-leak",
		},
		ResourceIDs: []string{"repo-1"},
		Settings:    map[string]interface{}{"branch": "main"},
	}
	blob, _ := cfg.ToJSON()
	ds := &types.DataSource{
		ID:     "ds-1",
		Name:   "github-prod",
		Type:   "github",
		Config: blob,
	}
	body, err := json.Marshal(NewDataSourceResponse(ds))
	assert.NoError(t, err)
	s := string(body)
	assert.NotContains(t, s, "ghp-secret-do-not-leak")
	// The inner config object must not carry the credentials map (the
	// DataSourceConfigDTO type omits it structurally).
	var raw map[string]json.RawMessage
	assert.NoError(t, json.Unmarshal(body, &raw))
	if cfgRaw, ok := raw["config"]; ok {
		var inner map[string]json.RawMessage
		assert.NoError(t, json.Unmarshal(cfgRaw, &inner))
		_, hasCredsInConfig := inner["credentials"]
		assert.False(t, hasCredsInConfig,
			"credentials map must not appear inside the config DTO")
	}
	// Top-level credentials map is just the "configured?" indicator,
	// replaces the removed GET /credentials endpoint.
	assert.Contains(t, s, `"credentials":{"credentials":{"configured":true}}`)
	// Non-secret config fields pass through.
	assert.Contains(t, s, "repo-1")
	assert.Contains(t, s, "branch")
	assert.Contains(t, s, "main")
}

func TestDataSourceResponses_OmitsLastSyncCursor(t *testing.T) {
	ds := &types.DataSource{
		ID:             "ds-1",
		Name:           "github-prod",
		Type:           "github",
		LastSyncCursor: types.JSON(`{"page":42}`),
		LastSyncResult: types.JSON(`{"total":10}`),
	}

	list := NewDataSourceResponses([]*types.DataSource{ds})
	assert.Len(t, list, 1)
	got := list[0]
	assert.Empty(t, got.LastSyncCursor)

	body, err := json.Marshal(got)
	assert.NoError(t, err)
	var raw map[string]json.RawMessage
	assert.NoError(t, json.Unmarshal(body, &raw))
	assert.NotContains(t, raw, "last_sync_cursor")

	// Every other field must match the single-entity conversion.
	want := NewDataSourceResponse(ds)
	assert.Equal(t, json.RawMessage(`{"page":42}`), want.LastSyncCursor)
	want.LastSyncCursor = nil
	assert.Equal(t, want, got)
}

func TestDataSourceResponse_NilSafe(t *testing.T) {
	assert.Nil(t, NewDataSourceResponse(nil))
	assert.Equal(t, []*DataSourceResponse{}, NewDataSourceResponses(nil))
}

func TestNASListKeepsProgressWithoutFileCursor(t *testing.T) {
	ds := &types.DataSource{Type: types.ConnectorTypeDokuWikiFilesystem,
		LastSyncCursor: types.JSON(`{"connector_cursor":{"inventory":{"source_files":8,"images":3,"attachments":2},"files":{"pages/a.txt":"hash","media/a.PNG":"hash","media/b.zip":"hash"},"failed":{"media/b.pdf":"reason"}}}`)}
	list := NewDataSourceResponses([]*types.DataSource{ds})
	assert.Empty(t, list[0].LastSyncCursor)
	assert.Equal(t, &DataSourceProgress{Total: 8, Completed: 3, Images: 3, ImageDone: 1, Attachments: 2, AttachmentDone: 1, Failed: 1}, list[0].SourceProgress)
	body, err := json.Marshal(list)
	assert.NoError(t, err)
	assert.NotContains(t, string(body), "media/a.PNG")
	assert.Contains(t, string(body), `"source_progress"`)
}

func TestNASListSeparatesDeferredAndExcludedFromCompleted(t *testing.T) {
	ds := &types.DataSource{Type: types.ConnectorTypeDokuWikiFilesystem,
		LastSyncCursor: types.JSON(`{"connector_cursor":{"inventory":{"source_files":8,"images":3,"attachments":1},"files":{"pages/a.txt":"hash","media/a.zip":"hash"},"failed":{"media/b.pdf":"reason"},"deferred":{"media/c.pdf":{"sha256":"hash","reason":"vision_disabled"}},"excluded":{"media/a.PNG":"file_type_not_selected","media/b.jpg":"file_type_not_selected","media/c.webp":"file_type_not_selected","pages/empty.txt":"empty_page_not_parsed"},"excluded_counts":{"file_type_not_selected":3,"empty_page_not_parsed":1},"scan_complete":true}}`)}
	response := NewDataSourceResponses([]*types.DataSource{ds})[0]
	progress := response.SourceProgress
	assert.Equal(t, 8, progress.Total)
	assert.Equal(t, 2, progress.Completed)
	assert.Equal(t, 1, progress.Failed)
	assert.Equal(t, 1, progress.Deferred)
	assert.Equal(t, 4, progress.Excluded)
	assert.Equal(t, 3, progress.ExcludedImages)
	// An old usable publication is retained when the current source version
	// fails or awaits recognition; do not count that path as current success.
	var stored map[string]interface{}
	assert.NoError(t, json.Unmarshal(ds.LastSyncCursor, &stored))
	files := stored["connector_cursor"].(map[string]interface{})["files"].(map[string]interface{})
	files["media/b.pdf"], files["media/c.pdf"] = "previous-hash", "previous-hash"
	encoded, err := json.Marshal(stored)
	assert.NoError(t, err)
	ds.LastSyncCursor = types.JSON(encoded)
	assert.Equal(t, 2, NewDataSourceResponse(ds).SourceProgress.Completed)
	assert.NotNil(t, progress.ScanComplete)
	assert.True(t, *progress.ScanComplete)
	body, err := json.Marshal(response)
	assert.NoError(t, err)
	assert.NotContains(t, string(body), "media/c.pdf")
	assert.NotContains(t, string(body), "vision_disabled")
	assert.Contains(t, string(body), `"scan_complete":true`)
}

func TestNASProgressPreservesIncompleteAndLegacyScanState(t *testing.T) {
	for _, scan := range []string{"", `,"scan_complete":false`} {
		ds := &types.DataSource{Type: types.ConnectorTypeDokuWikiFilesystem,
			LastSyncCursor: types.JSON(`{"connector_cursor":{"inventory":{"source_files":1},"files":{},"failed":{}` + scan + `}}`)}
		response := NewDataSourceResponse(ds)
		assert.Zero(t, response.SourceProgress.Deferred)
		body, err := json.Marshal(response)
		assert.NoError(t, err)
		if scan == "" {
			assert.Nil(t, response.SourceProgress.ScanComplete)
			assert.NotContains(t, string(body), `"scan_complete"`)
		} else {
			assert.NotNil(t, response.SourceProgress.ScanComplete)
			assert.False(t, *response.SourceProgress.ScanComplete)
			assert.Contains(t, string(body), `"scan_complete":false`)
		}
	}
	assert.Nil(t, NewDataSourceResponse(&types.DataSource{Type: types.ConnectorTypeRSS}).SourceProgress)
}

func TestDataSourceResponse_RSSFeedURLsFromCredentials(t *testing.T) {
	cfg := types.DataSourceConfig{
		Type: types.ConnectorTypeRSS,
		Credentials: map[string]interface{}{
			"feed_urls": "https://example.com/a.xml\nhttps://example.com/b.xml",
		},
		ResourceIDs: []string{"https://example.com/a.xml"},
	}
	blob, _ := cfg.ToJSON()
	ds := &types.DataSource{
		ID:     "ds-rss",
		Name:   "my-rss",
		Type:   types.ConnectorTypeRSS,
		Config: blob,
	}
	resp := NewDataSourceResponse(ds)
	assert.NotNil(t, resp.Config)
	assert.Equal(t, "https://example.com/a.xml\nhttps://example.com/b.xml", resp.Config.Settings["feed_urls"])
	assert.False(t, resp.Credentials["credentials"].Configured)
	body, err := json.Marshal(resp)
	assert.NoError(t, err)
	s := string(body)
	assert.Contains(t, s, "https://example.com/a.xml")
	assert.NotContains(t, s, "Bearer secret")
}

func TestDataSourceResponse_RSSAuthHeadersConfigured(t *testing.T) {
	cfg := types.DataSourceConfig{
		Type: types.ConnectorTypeRSS,
		Credentials: map[string]interface{}{
			"auth_headers": "Authorization: Bearer secret",
		},
		Settings: map[string]interface{}{
			"feed_urls": "https://example.com/feed.xml",
		},
	}
	blob, _ := cfg.ToJSON()
	ds := &types.DataSource{
		ID:     "ds-rss-auth",
		Name:   "my-rss",
		Type:   types.ConnectorTypeRSS,
		Config: blob,
	}
	resp := NewDataSourceResponse(ds)
	assert.True(t, resp.Credentials["credentials"].Configured)
	body, err := json.Marshal(resp)
	assert.NoError(t, err)
	assert.NotContains(t, string(body), "Bearer secret")
}

func TestDataSourceResponse_NoConfig(t *testing.T) {
	ds := &types.DataSource{ID: "x", Name: "x"}
	body, err := json.Marshal(NewDataSourceResponse(ds))
	assert.NoError(t, err)
	// No config jsonb stored → no config object in the response.
	assert.NotContains(t, string(body), `"config":`)
}
