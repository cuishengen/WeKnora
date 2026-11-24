package mcpserver

import (
	"context"
	"net/url"
	"os"
	"strings"

	"github.com/Tencent/WeKnora/internal/companyidentity"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/mark3labs/mcp-go/mcp"
)

func getOriginalDocumentTool() mcp.Tool {
	return mcp.NewTool(types.MCPEndpointToolGetOriginalDocument,
		mcp.WithDescription("Get a document's original file metadata and a company-login-protected download URL. "+
			"The URL checks the current employee and document permission again when opened; it is not public, "+
			"does not contain a token, and does not return file bytes. Requires an employee-bound endpoint "+
			"with explicit original-file access."),
		mcp.WithString("knowledge_id", mcp.Required(), mcp.Description("Document id from list_documents or search results")),
		readOnlyEndpointAnnotations(),
	)
}

func (s *Server) handleGetOriginalDocument(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	ep, err := endpointFromContext(ctx)
	if err != nil || ep.EmployeeBinding == nil {
		return mcp.NewToolResultError("an employee-bound endpoint is required"), nil
	}
	checker, err := companyidentity.FromEnv()
	if err != nil || !checker.OriginalDownloadsAllowed() {
		return mcp.NewToolResultError("original-file access is not enabled"), nil
	}
	if err := checker.AuthorizeKnowledgeBases(ep.KnowledgeBaseIDs); err != nil {
		return mcp.NewToolResultError("knowledge scope is not allowed"), nil
	}
	if _, err := checker.Check(ctx, *ep.EmployeeBinding); err != nil {
		return mcp.NewToolResultError("employee access could not be verified"), nil
	}
	id, err := req.RequireString("knowledge_id")
	if err != nil {
		return mcp.NewToolResultError("knowledge_id is required"), nil
	}
	document, kb, err := s.knowledgeInScope(ctx, ep, id)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	if document.FileName == "" {
		return mcp.NewToolResultError("this document has no original file"), nil
	}
	base := strings.TrimRight(strings.TrimSpace(os.Getenv("COMPANY_KNOWLEDGE_DOWNLOAD_BASE_URL")), "/")
	u, err := url.Parse(base)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return mcp.NewToolResultError("company download portal is not configured"), nil
	}
	return jsonResult(map[string]any{
		"document":               summarizeKnowledge(document),
		"knowledge_base":         map[string]string{"id": kb.ID, "name": kb.Name},
		"download_url":           base + "/" + url.PathEscape(document.ID) + "/download",
		"requires_company_login": true,
	})
}
