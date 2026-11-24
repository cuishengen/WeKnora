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
	lookups int
	opens   int
	onClose func()
}

func (s *originalTestService) GetKnowledgeByID(context.Context, string) (*types.Knowledge, error) {
	s.lookups++
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
			msg := &IncomingMessage{Platform: PlatformWeCom, UserID: "member", Content: "请把 datasheet 原件发给我"}
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

func TestRequestsOriginalFilesRequiresCurrentUserDeliveryIntent(t *testing.T) {
	for _, text := range []string{
		"请发 bt8922a_datasheet.pdf 原件", "请发 tws_dev_v1.3.zip 原件", "发我 datasheet", "请把原件发给我",
		"把刚才两份发我", "两份都发过来", "请发我", "发我看看", "发过来", "这两个都发过来", "请把它发我", "能不能把这两份资料发给我？",
		"不要链接，把PDF发我", "不要解释，把PDF发我", "比较B/C区别，再把B的datasheet发我",
		"先回答区别，并把 PDF 发给我", "请发‘xxx.pdf’给我", "请把《数据手册》发我", "请把“data.pdf”发给我",
		"请发 `sdk.zip` 给我", "给我一份AB5682B的PDF", "请给我原件", "把刚才发给我的PDF再发我",
		"Send me the PDF", "Please send me sdk.zip", "Attach the datasheet", "Could you send the datasheet?", "Can you send the PDF to me?",
		"send it to me", "Don't send a link; send me the PDF", "Send me the PDF and provide a download link",
	} {
		t.Run(text, func(t *testing.T) { require.True(t, requestsOriginalFiles(text)) })
	}
	for _, text := range []string{
		"", "AB5682B和AB5682C有什么区别", "比较 BT892x、BT893x、BT897x 的芯片总 RAM", "这个 datasheet 里的 RAM 是多少？",
		"bt8922a_datasheet.pdf", "这份PDF", "这两个手册", "两份datasheet", "把PDF", "帮我找一下芯片手册", "有没有 AB5682B 的 datasheet", "手册在哪里下载",
		"提供参考文件链接", "给我下载链接", "发我 datasheet 链接", "把 PDF 下载地址发给我", "只给链接", "请发我PDF，但只要链接",
		"不要发附件，只给链接", "别自动给我发文件", "请不要把AB5682B的datasheet发给我", "先发我PDF，但是别发附件",
		"发我PDF；比较完之前别发附件",
		"为什么也会把 datasheet 发给我", "如何让机器人发送文件", "发送文件有什么限制", "文档写着‘发我PDF’是什么意思",
		"你发给我的PDF是什么版本？", "比较一下你发来的这两份手册", "如何让机器人并发发送文件",
		"你发给我PDF时卡住了", "发送文件需要管理员权限", "发送文件功能", "The application allows you to attach files",
		"文档原话'发我PDF'，请解释", "“发我PDF”", "`send me the PDF`", "```\nsend me the PDF\n```",
		"把对比结果发给我", "把 datasheet 里的区别发我", "发我这两份文档的总结", "给我文档总结", "只把答案发给我",
		"给我PDF的摘要", "请把文档的目录发给我", "发我这份资料的要点",
		"Send me the datasheet link", "Give me the download URL", "Just the link please", "Don't send the PDF",
		"No attachments, send me the PDF", "Why did you send me the datasheet?", "Explain 'send me the PDF'",
		"How do I send files to coworkers?", "Send me the comparison", "Send me the datasheet summary",
	} {
		t.Run(text, func(t *testing.T) { require.False(t, requestsOriginalFiles(text)) })
	}
}

func TestOriginalDeliveryDoesNotTurnReferenceLinksIntoAttachmentRequests(t *testing.T) {
	for _, tc := range []struct {
		query string
		send  bool
	}{
		{"AB5682B和AB5682C有什么区别", false},
		{"帮我找这两个芯片的 datasheet", false},
		{"给我参考文件链接", false},
		{"请发我 PDF，但是不要发附件", false},
		{"文档写着‘发我PDF’，是什么意思", false},
		{"把对比结果发给我", false},
		{"请发我 datasheet 原件", true},
		{"把刚才两份发我", true},
		{"不要链接，把PDF发我", true},
	} {
		t.Run(tc.query, func(t *testing.T) {
			checker := employeeFixture()
			files := &originalTestService{row: &types.Knowledge{
				ID: originalTestID, TenantID: 7, KnowledgeBaseID: "vendor", FileSize: 8,
			}, data: []byte("%PDF-1.4")}
			adapter := &originalTestAdapter{limit: 20}
			svc := &Service{employeeChecker: checker, knowledgeService: files, companyDownloadBase: "https://identity.example/company/knowledge"}
			msg := &IncomingMessage{Platform: PlatformWeCom, UserID: "member", Content: tc.query,
				Quote: &QuotedMessage{Content: "请发我 datasheet 原件", IsBotMessage: false}}
			ctx, err := svc.authorizeIMIdentity(context.Background(), &IMChannel{ID: "channel", TenantID: 7, Platform: "wecom"}, msg)
			require.NoError(t, err)
			answer := "模型文字即使说请发附件也不能代替用户请求，参考 [datasheet](/platform/source-download/" + originalTestID + ")。"
			agent := &types.CustomAgent{Config: types.CustomAgentConfig{KBSelectionMode: "selected", KnowledgeBases: []string{"vendor"}}}
			svc.sendOriginalFiles(ctx, msg, adapter, agent, nil, answer)
			if tc.send {
				require.Equal(t, [][]byte{files.data}, adapter.files)
				require.Equal(t, 1, files.opens)
			} else {
				require.Empty(t, adapter.files)
				require.Empty(t, adapter.replies)
				require.Zero(t, files.lookups)
				require.Zero(t, files.opens)
				require.Equal(t, 1, checker.checks, "a reference-only answer does not enter file delivery")
			}
			require.Equal(t, "模型文字即使说请发附件也不能代替用户请求，参考 [datasheet](https://identity.example/company/knowledge/"+originalTestID+"/download)。", rewriteCompanyOriginalLinks(ctx, answer))
		})
	}
}

func TestOriginalLinksDeduplicateEntitiesAndRejectArbitraryPaths(t *testing.T) {
	answer := "/platform/source-download/" + originalTestID + " /platform/source-download/" + originalTestID + " /etc/password https://untrusted.example/file.zip"
	require.Equal(t, []string{originalTestID}, originalKnowledgeIDs(answer))
	require.Empty(t, companyDownloadBase("http://identity.example/company/knowledge"))
	require.Empty(t, companyDownloadBase("https://user:secret@identity.example/company/knowledge"))
}
