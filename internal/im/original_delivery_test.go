package im

import (
	"bytes"
	"context"
	"io"
	"testing"

	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
	"github.com/stretchr/testify/require"
)

const originalTestID = "12345678-1234-1234-1234-123456789abc"

type originalTestService struct {
	interfaces.KnowledgeService
	row     *types.Knowledge
	data    []byte
	opens   int
	onClose func()
}

func (s *originalTestService) GetKnowledgeByID(context.Context, string) (*types.Knowledge, error) {
	return s.row, nil
}
func (s *originalTestService) GetKnowledgeFile(context.Context, string) (io.ReadCloser, string, error) {
	s.opens++
	return &originalReadCloser{Reader: bytes.NewReader(s.data), close: s.onClose}, "datasheet.pdf", nil
}

type originalReadCloser struct {
	*bytes.Reader
	close func()
}

func (r *originalReadCloser) Close() error {
	if r.close != nil {
		r.close()
	}
	return nil
}

type originalTestAdapter struct {
	Adapter
	files   [][]byte
	replies []string
	limit   int64
}

func (a *originalTestAdapter) MaxFileSize() int64 { return a.limit }
func (a *originalTestAdapter) SendFile(_ context.Context, _ *IncomingMessage, _ string, data []byte) error {
	a.files = append(a.files, data)
	return nil
}
func (a *originalTestAdapter) SendReply(_ context.Context, _ *IncomingMessage, reply *ReplyMessage) error {
	a.replies = append(a.replies, reply.Content)
	return nil
}

func TestOriginalDeliveryRechecksEmployeeAndScopesBeforeReadingAndSending(t *testing.T) {
	for _, scenario := range []string{"allowed", "other-kb", "other-tenant", "disabled-download", "metadata-too-large", "bytes-too-large", "revoked-after-read"} {
		t.Run(scenario, func(t *testing.T) {
			checker := employeeFixture()
			row := &types.Knowledge{ID: originalTestID, TenantID: 7, KnowledgeBaseID: "vendor", FileSize: 8}
			files := &originalTestService{row: row, data: []byte("%PDF-1.4")}
			adapter := &originalTestAdapter{limit: 20}
			svc := &Service{employeeChecker: checker, knowledgeService: files, companyDownloadBase: "https://identity.example/company/knowledge"}
			channel := &IMChannel{ID: "channel", TenantID: 7, Platform: "wecom"}
			msg := &IncomingMessage{Platform: PlatformWeCom, UserID: "member"}
			ctx, err := svc.authorizeIMIdentity(context.Background(), channel, msg)
			require.NoError(t, err)
			switch scenario {
			case "other-kb":
				row.KnowledgeBaseID = "warehouse"
			case "other-tenant":
				row.TenantID = 99
			case "disabled-download":
				checker.downloads = false
			case "metadata-too-large":
				row.FileSize = 21
			case "bytes-too-large":
				files.data = make([]byte, 21)
			case "revoked-after-read":
				files.onClose = func() { checker.allowed = false }
			}
			answer := "[original](/platform/source-download/" + originalTestID + ")"
			svc.sendOriginalFiles(ctx, msg, adapter, &types.CustomAgent{Config: types.CustomAgentConfig{KBSelectionMode: "selected", KnowledgeBases: []string{"vendor"}}}, nil, answer)
			if scenario == "allowed" {
				require.Equal(t, [][]byte{files.data}, adapter.files)
				require.GreaterOrEqual(t, checker.checks, 3)
			} else {
				require.Empty(t, adapter.files)
				if scenario != "bytes-too-large" && scenario != "revoked-after-read" {
					require.Zero(t, files.opens)
				}
			}
			require.Equal(t, "[original](https://identity.example/company/knowledge/"+originalTestID+"/download)", rewriteCompanyOriginalLinks(ctx, answer))
		})
	}
}

func TestOriginalLinksDeduplicateEntitiesAndRejectArbitraryPaths(t *testing.T) {
	answer := "/platform/source-download/" + originalTestID + " /platform/source-download/" + originalTestID + " /etc/password https://untrusted.example/file.zip"
	require.Equal(t, []string{originalTestID}, originalKnowledgeIDs(answer))
	require.Empty(t, companyDownloadBase("http://identity.example/company/knowledge"))
	require.Empty(t, companyDownloadBase("https://user:secret@identity.example/company/knowledge"))
}
