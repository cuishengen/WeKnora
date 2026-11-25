package im

import (
	"context"
	"io"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/Tencent/WeKnora/internal/logger"
	"github.com/Tencent/WeKnora/internal/types"
)

var originalDownloadRE = regexp.MustCompile(`(?:https?://[^/\s)"']+)?/platform/source-download/([0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12})`)

const originalRequestFileTarget = `文件|附件|原件|手册|文档|资料|压缩包|固件|代码|它(?:们)?|(?:这|那)(?:个|份|两份|几份|两个|几个|些)|(?:两|三|几|[0-9]+)份|\.[a-z][a-z0-9]{0,7}\b|\b(?:pdf|zip|rar|7z|sdk|files?|attachments?|originals?|manuals?|documents?|datasheets?|it|them)\b`
const originalRequestPrefix = `(?:请(?:你)?|麻烦(?:你)?|帮我|再|直接|都|并且|并|只|仅|(?:你|您)?(?:可以|能不能|能否)|\s)*`

var (
	originalRequestContextRE  = regexp.MustCompile("(?s)```.*?```|`[^`]*`|“[^”]*”|‘[^’]*’|《[^》]*》|\"[^\"]*\"|(?:^|[^A-Za-z0-9_])'[^'\\r\\n]*'|(?:发(?:送)?|传(?:送)?)(?:给我|我|过来|来)(?:过)?的")
	originalRequestClauseRE   = regexp.MustCompile(`[\n,;，；。!?！？]+|并且|然后|另外|同时|\b(?:and|then)\b`)
	originalRequestLinkRE     = regexp.MustCompile(`链接|地址|网址|\b(?:links?|urls?)\b`)
	originalRequestOnlyLinkRE = regexp.MustCompile(`(?:只|仅)[^\n,;，；。!?！？]{0,32}(?:链接|地址|网址|文字|文本|回答|答案)|\b(?:only|just)\s+(?:(?:send|give|provide|show)\s+(?:me\s+)?)?(?:the\s+)?(?:(?:download|reference|file)\s+)?(?:links?|urls?|text|answers?)\b|\b(?:links?|urls?)\s+only\b`)
	originalRequestNegativeRE = regexp.MustCompile(`(?:不(?:要|用|必|需要|想)|别|无需|勿|禁止|取消|停止)(?:再|自动|直接|给我|向我|帮我|为我|任何|这些|那些|\s)*(?:发(?:送|给|我|来|过来)?|传(?:送|给|我|来)?|转发|附上|附件|原件|文件|资料|手册|pdf|zip)|(?:不(?:要|用|必|需要|想)|别|无需|勿|禁止)(?:把|将)[^\n,;，；。!?！？]{0,64}(?:发(?:送)?(?:给我|我|过来|来)|传(?:送)?(?:给我|我|过来|来))|\b(?:don't|do\s+not|never|no\s+need\s+to|stop|avoid|cancel)\s+(?:\w+\s+){0,3}(?:send|attach|forward|deliver)\b|\b(?:no|without)\s+(?:(?:any|more)\s+)?(?:attachments?|files?|originals?)\b`)
	originalRequestMetaRE     = regexp.MustCompile(`为什么|为何|怎么|如何|怎样|什么|哪些|哪个|哪份|多少|是否|是不是|了吗|比较|对比|区别|差异|^(?:你|您)(?:之前|刚才|上次|已经)?(?:发(?:送)?|传(?:送)?)(?:给我|我|来)|\b(?:why|how|what|which|when|meaning|compare|comparison|differences?)\b`)
	originalRequestDeliveryRE = regexp.MustCompile(`^` + originalRequestPrefix + `(?:发送|发|传送|传|转发|给我|附上)|^` + originalRequestPrefix + `(?:把|将|(?:这|那)(?:个|份|两个|几份|些)|(?:两|三|几|[0-9]+)份)[^\n,;，；。!?！？]{0,80}(?:(?:发(?:送)?|传(?:送)?|转发)(?:给我|我|过来|来)|给我)|^(?:(?:please|could\s+you|can\s+you|would\s+you)\s+)*(?:send|forward|deliver|attach)\b`)
	originalRequestFileRE     = regexp.MustCompile(originalRequestFileTarget)
	originalRequestTargetRE   = regexp.MustCompile(`(?:` + originalRequestFileTarget + `)\s*(?:(?:都|再|直接|先)\s*)?(?:(?:发(?:送)?|传(?:送)?|转发)(?:给我|我|过来|来)|给我)?(?:吧|一下|看看|吗|谢谢|\s|to\s+me|please)*$`)
	originalRequestBareRE     = regexp.MustCompile(`^(?:请|麻烦|帮我|再|都|直接|把|将|\s)*(?:(?:发(?:送)?|传(?:送)?)(?:给我|我|过来|来)|给我(?:发(?:送)?|传(?:送)?))(?:吧|一下|一份|看看|\s)*$|^(?:please\s+)?(?:send|forward|deliver)\s+(?:it|them)(?:\s+to\s+me)?(?:\s+please)?$`)
)

// A reference link is evidence, not permission to push a file. Only the current
// user's unquoted request can opt into delivery; history, Quote and model prose
// never do. Ambiguous requests keep the normal answer and authenticated links.
func requestsOriginalFiles(content string) bool {
	content = strings.ToLower(content)
	content = originalRequestContextRE.ReplaceAllStringFunc(content, func(quoted string) string {
		// A quoted filename can be the target of an outer request. Quoted
		// instructions and fenced examples cannot opt into delivery themselves.
		if strings.HasPrefix(quoted, "```") || originalRequestDeliveryRE.MatchString(strings.Trim(quoted, " \t\r\n`\"'“”‘’")) {
			return " "
		}
		if originalRequestFileRE.MatchString(quoted) {
			return " 文件 "
		}
		return " "
	})
	wantsFile := false
	for _, clause := range originalRequestClauseRE.Split(content, -1) {
		clause = strings.TrimSpace(clause)
		if clause == "" {
			continue
		}
		if originalRequestOnlyLinkRE.MatchString(clause) {
			return false
		}
		if originalRequestNegativeRE.MatchString(clause) {
			// "不要链接，把 PDF 发我" negates links, not file delivery.
			if originalRequestLinkRE.MatchString(clause) && !originalRequestFileRE.MatchString(clause) {
				continue
			}
			return false
		}
		if originalRequestMetaRE.MatchString(clause) || originalRequestLinkRE.MatchString(clause) {
			continue
		}
		if originalRequestBareRE.MatchString(clause) ||
			(originalRequestDeliveryRE.MatchString(clause) && originalRequestTargetRE.MatchString(clause)) {
			wantsFile = true
		}
	}
	return wantsFile
}

func originalKnowledgeIDs(answer string) []string {
	var ids []string
	seen := map[string]bool{}
	for _, match := range originalDownloadRE.FindAllStringSubmatch(answer, -1) {
		id := strings.ToLower(match[1])
		if !seen[id] {
			seen[id] = true
			ids = append(ids, id)
		}
	}
	return ids
}

func companyDownloadBase(raw string) string {
	u, err := url.Parse(strings.TrimRight(strings.TrimSpace(raw), "/"))
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return ""
	}
	return u.String()
}

func rewriteCompanyOriginalLinks(ctx context.Context, content string) string {
	base, _ := ctx.Value(companyDownloadBaseKey{}).(string)
	if base == "" {
		return content
	}
	return originalDownloadRE.ReplaceAllString(content, base+"/$1/download")
}

type companyDownloadBaseKey struct{}

// The model selects a knowledge entity, never a path or remote fetch URL. Each
// original is reopened through the normal knowledge service after a live
// employee check and the intersection of deployment and agent KB scopes.
func (s *Service) sendOriginalFiles(ctx context.Context, msg *IncomingMessage, adapter Adapter, agent *types.CustomAgent, kbIDs []string, answer string) {
	if msg == nil || !requestsOriginalFiles(msg.Content) {
		return
	}
	identity, company := ctx.Value(companyIMContextKey{}).(companyIMSubject)
	sender, supported := adapter.(FileSender)
	if !company || identity.scopeKey != "" || !supported || s.employeeChecker == nil || !s.employeeChecker.OriginalDownloadsAllowed() || s.knowledgeService == nil {
		return
	}
	ids := originalKnowledgeIDs(answer)
	if len(ids) == 0 {
		return
	}
	for _, id := range ids[:min(len(ids), 5)] {
		fileCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
		freshCtx, err := s.authorizeIMIdentity(fileCtx, identity.channel, msg)
		if err != nil {
			cancel()
			return
		}
		knowledge, err := s.knowledgeService.GetKnowledgeByID(freshCtx, id)
		if err != nil || knowledge == nil || knowledge.TenantID != identity.channel.TenantID ||
			!outboundKBInScope(agent, kbIDs, knowledge.KnowledgeBaseID) ||
			s.employeeChecker.AuthorizeKnowledgeBases([]string{knowledge.KnowledgeBaseID}) != nil ||
			types.AuthorizeTenantAPIKeyKnowledgeBases(freshCtx, knowledge.KnowledgeBaseID) != nil {
			cancel()
			continue
		}
		limit := sender.MaxFileSize()
		if limit <= 0 || limit > maxIMAttachmentBytes {
			cancel()
			continue
		}
		if knowledge.FileSize > limit {
			cancel()
			continue
		}
		reader, name, err := s.knowledgeService.GetKnowledgeFile(freshCtx, id)
		if err != nil {
			cancel()
			s.originalDeliveryFailure(ctx, msg, adapter)
			continue
		}
		data, readErr := io.ReadAll(io.LimitReader(reader, limit+1))
		closeErr := reader.Close()
		if readErr != nil || closeErr != nil {
			cancel()
			s.originalDeliveryFailure(ctx, msg, adapter)
			continue
		}
		if int64(len(data)) > limit || len(data) < 5 {
			cancel()
			continue
		}
		// Downloading a NAS original may take time; revoke between read and send.
		if _, err = s.authorizeIMIdentity(fileCtx, identity.channel, msg); err != nil {
			cancel()
			return
		}
		err = sender.SendFile(fileCtx, msg, name, data)
		cancel()
		if err != nil {
			logger.Warnf(ctx, "[IM] Original attachment delivery was not acknowledged")
			s.originalDeliveryFailure(ctx, msg, adapter)
		}
	}
}

func (s *Service) originalDeliveryFailure(ctx context.Context, msg *IncomingMessage, adapter Adapter) {
	_ = adapter.SendReply(ctx, msg, &ReplyMessage{Content: "附件发送未获确认，请使用上面的原件下载入口；如需重试，请重新发送请求。", IsFinal: true})
}
