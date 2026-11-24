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
	identity, company := ctx.Value(companyIMContextKey{}).(companyIMSubject)
	sender, supported := adapter.(FileSender)
	if !company || !supported || s.employeeChecker == nil || !s.employeeChecker.OriginalDownloadsAllowed() || s.knowledgeService == nil {
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
