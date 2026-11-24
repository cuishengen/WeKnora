package mcpserver

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/Tencent/WeKnora/internal/companyidentity"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"
)

func TestOriginalDocumentReturnsProtectedPortalAndRejectsRevocationOrForeignLibrary(t *testing.T) {
	var disabled atomic.Bool
	status := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"active": !disabled.Load(), "allowed": !disabled.Load(),
			"issuer": "https://auth.ch-chip.com", "sub": "stable-employee", "wecomID": "ExactID", "createdTime": "2026-11-18"})
	}))
	defer status.Close()
	key := filepath.Join(t.TempDir(), "status-key")
	require.NoError(t, os.WriteFile(key, make([]byte, 32), 0600))
	t.Setenv("COMPANY_IDENTITY_STATUS_ORIGIN", status.URL)
	t.Setenv("COMPANY_IDENTITY_STATUS_KEY_FILE", key)
	t.Setenv("COMPANY_IDENTITY_KNOWLEDGE_BASE_IDS", "kb-own")
	t.Setenv("COMPANY_IDENTITY_ALLOW_ORIGINAL_DOWNLOADS", "true")
	t.Setenv("COMPANY_KNOWLEDGE_DOWNLOAD_BASE_URL", "https://auth.ch-chip.com/company/knowledge")
	srv, _, _, ep := newDocumentSearchFixture(t)
	ep.EmployeeBinding = &companyidentity.Binding{Issuer: "https://auth.ch-chip.com", Subject: "stable-employee"}
	ep.KnowledgeBaseIDs = types.StringArray{"kb-own"}
	for _, doc := range srv.knowledgeService.(*stubKnowledgeService).docs {
		doc.FileName = "original.pdf"
	}
	ctx := mcpCallContext(1, ep)
	call := func(id string) *mcp.CallToolResult {
		req := mcp.CallToolRequest{}
		req.Params.Arguments = map[string]any{"knowledge_id": id}
		result, err := srv.handleGetOriginalDocument(ctx, req)
		require.NoError(t, err)
		return result
	}
	result := call("doc-a")
	require.False(t, result.IsError)
	raw, err := json.Marshal(result)
	require.NoError(t, err)
	require.Contains(t, string(raw), "https://auth.ch-chip.com/company/knowledge/doc-a/download")
	require.NotContains(t, string(raw), "status-key")
	require.NotContains(t, string(raw), "Bearer")
	require.True(t, call("doc-other").IsError, "same-tenant sensitive library must not escape explicit endpoint scope")
	disabled.Store(true)
	require.True(t, call("doc-a").IsError, "an existing document id is not authority after employee revocation")
	disabled.Store(false)
	t.Setenv("COMPANY_IDENTITY_ALLOW_ORIGINAL_DOWNLOADS", "false")
	require.True(t, call("doc-a").IsError)
}
