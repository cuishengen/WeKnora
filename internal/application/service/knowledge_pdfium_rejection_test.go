package service

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/Tencent/WeKnora/internal/types"
	"github.com/stretchr/testify/require"
)

func TestConvertUsesStructuredPDFiumInputRejectionOnlyForStagedDocuments(t *testing.T) {
	const detail = "Failed to load document (PDFium: Data format error)."
	for _, tc := range []struct {
		name, kind, source, code string
		ordinary, wantRejected   bool
		getErr, rpcErr           error
	}{
		{name: "malformed", kind: "document_content", source: "pdfium", code: "pdf_invalid_format", wantRejected: true},
		{name: "password", kind: "document_content", source: "pdfium", code: "pdf_password_required", wantRejected: true},
		{name: "security", kind: "document_content", source: "pdfium", code: "pdf_unsupported_security", wantRejected: true},
		{name: "empty PDF", kind: "document_content", source: "pdfium", code: "pdf_empty_content", wantRejected: true},
		{name: "same error text without metadata"},
		{name: "unknown kind", kind: "operational", source: "pdfium", code: "pdf_invalid_format"},
		{name: "unknown source", kind: "document_content", source: "other", code: "pdf_invalid_format"},
		{name: "unknown code", kind: "document_content", source: "pdfium", code: "pdf_file_access"},
		{name: "transport", kind: "document_content", source: "pdfium", code: "pdf_invalid_format", rpcErr: context.DeadlineExceeded},
		{name: "storage", kind: "document_content", source: "pdfium", code: "pdf_invalid_format", getErr: errors.New("storage unavailable")},
		{name: "ordinary upload", kind: "document_content", source: "pdfium", code: "pdf_invalid_format", ordinary: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tracker, _ := setupSpanTrackerTest(t)
			ctx := types.WithTaskRetryMetadata(context.Background(), 3, 3)
			knowledge := &types.Knowledge{ID: "doc", TenantID: 1, KnowledgeBaseID: "kb", FilePath: "doc.pdf",
				ParseStatus: types.ParseStatusProcessing, IsStaged: !tc.ordinary}
			_, attempt, err := tracker.OpenAttempt(ctx, knowledge.ID, "")
			require.NoError(t, err)
			ctx = withAttempt(ctx, attempt)
			svc := &knowledgeService{
				repo: &embedFailureKnowledgeRepo{knowledge: knowledge}, spanTracker: tracker,
				tenantService: &stubTenantServiceForModelDelete{}, fileSvc: spanRetryFileService{getErr: tc.getErr},
				documentReader: &spanRetryDocReader{result: &types.ReadResult{Error: detail, Metadata: map[string]string{
					"error_kind": tc.kind, "error_source": tc.source, "error_code": tc.code,
				}}, err: tc.rpcErr},
			}
			result, err := svc.convert(ctx, types.DocumentProcessPayload{FilePath: "doc.pdf", FileName: "doc.pdf", FileType: "pdf"},
				&types.KnowledgeBase{ID: "kb"}, knowledge, types.EffectiveProcessConfig{ChunkingConfig: types.ChunkingConfig{
					ParserEngineRules: []types.ParserEngineRule{{FileTypes: []string{"pdf"}, Engine: "builtin"}},
				}}, true)
			require.Nil(t, result)
			if tc.rpcErr != nil || tc.getErr != nil {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
			require.Equal(t, types.ParseStatusFailed, knowledge.ParseStatus)
			require.Equal(t, tc.wantRejected, strings.HasPrefix(knowledge.ErrorMessage, stagedDocumentRejectionPrefix))
		})
	}
}
