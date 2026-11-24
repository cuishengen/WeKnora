package im

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"

	agenttools "github.com/Tencent/WeKnora/internal/agent/tools"
	"github.com/Tencent/WeKnora/internal/companyidentity"
	"github.com/Tencent/WeKnora/internal/types"
)

type employeeChecker interface {
	LookupWeCom(context.Context, string) (*companyidentity.Member, error)
	Check(context.Context, companyidentity.Binding) (*companyidentity.Member, error)
	AllowedKnowledgeBaseIDs() []string
	AuthorizeKnowledgeBases([]string) error
	OriginalDownloadsAllowed() bool
}

type companyIMContextKey struct{}
type companyIMSubject struct {
	channel *IMChannel
	member  *companyidentity.Member
}

func isCompanyIM(channel *IMChannel) bool {
	return channel != nil && (channel.Platform == string(PlatformWeCom) || channel.Platform == string(PlatformWeChat))
}

// authorizeIMIdentity consumes only adapter-verified sender IDs. Website OpenID,
// iLink IDs and WeCom IDs belong to different namespaces and are never guessed.
func (s *Service) authorizeIMIdentity(ctx context.Context, channel *IMChannel, msg *IncomingMessage) (context.Context, error) {
	if !isCompanyIM(channel) {
		return withIMIdentity(ctx, channel, msg), nil
	}
	if s.employeeChecker == nil || msg == nil || msg.UserID == "" || string(msg.Platform) != channel.Platform || msg.ChatType == ChatTypeGroup {
		return nil, companyidentity.ErrDenied
	}
	if s.db != nil {
		current, err := s.GetChannelByID(channel.ID)
		if err != nil || current == nil || !current.Enabled || current.TenantID != channel.TenantID || current.Platform != channel.Platform ||
			current.AgentID != channel.AgentID || current.BotIdentity != channel.BotIdentity {
			return nil, companyidentity.ErrDenied
		}
		channel = current
	}
	var member *companyidentity.Member
	var err error
	if msg.Platform == PlatformWeCom {
		member, err = s.employeeChecker.LookupWeCom(ctx, msg.UserID)
	} else {
		var credentials struct {
			EmployeeBindings map[string]companyidentity.Binding `json:"employee_bindings"`
			EmployeeIssuer   string                             `json:"employee_issuer"`
			EmployeeSubject  string                             `json:"employee_subject"`
			OwnerUserID      string                             `json:"ilink_user_id"`
		}
		if json.Unmarshal(channel.Credentials, &credentials) != nil {
			return nil, companyidentity.ErrDenied
		}
		binding, bound := credentials.EmployeeBindings[msg.UserID]
		if !bound && credentials.OwnerUserID == msg.UserID && credentials.EmployeeIssuer != "" && credentials.EmployeeSubject != "" {
			binding = companyidentity.Binding{Issuer: credentials.EmployeeIssuer, Subject: credentials.EmployeeSubject}
			bound = true
		}
		if !bound {
			return nil, companyidentity.ErrDenied
		}
		member, err = s.employeeChecker.Check(ctx, binding)
	}
	if err != nil || member == nil || member.Subject == "" || member.Issuer == "" || member.CreatedTime == "" {
		return nil, companyidentity.ErrDenied
	}
	if previous, ok := ctx.Value(companyIMContextKey{}).(companyIMSubject); ok &&
		(previous.member.Subject != member.Subject || previous.member.Issuer != member.Issuer || previous.member.CreatedTime != member.CreatedTime) {
		return nil, companyidentity.ErrDenied
	}
	ids := s.employeeChecker.AllowedKnowledgeBaseIDs()
	if s.employeeChecker.AuthorizeKnowledgeBases(ids) != nil {
		return nil, companyidentity.ErrDenied
	}
	digest := sha256.Sum256([]byte(member.Issuer + "\n" + member.Subject + "\n" + member.CreatedTime))
	userID := "company-im-" + hex.EncodeToString(digest[:])
	ctx = context.WithValue(ctx, types.TenantIDContextKey, channel.TenantID)
	ctx = context.WithValue(ctx, types.UserIDContextKey, userID)
	ctx = context.WithValue(ctx, types.TenantRoleContextKey, types.TenantRoleViewer)
	ctx = types.WithCaller(ctx, types.Caller{TenantID: channel.TenantID, UserID: userID, Role: types.TenantRoleViewer})
	ctx = types.WithPrincipal(ctx, types.Principal{Type: types.PrincipalIMUser, ID: fmt.Sprintf("%d:%s:%s", channel.TenantID, channel.ID, userID)})
	ctx = types.WithPrincipalAuthSource(ctx, types.AuthSourceIM)
	ctx = types.WithTenantAPIKeyScope(ctx, types.TenantAPIKeyScope{
		KnowledgeBaseIDs: types.StringArray(ids),
		Capabilities:     types.StringArray{string(types.APIKeyCapabilityRetrieve), string(types.APIKeyCapabilityChat)},
	})
	locale := channel.Locale
	if locale == "" {
		locale = types.DefaultLanguage()
	}
	ctx = context.WithValue(ctx, types.LanguageContextKey, locale)
	ctx = types.WithMCPOAuthNonInteractive(ctx)
	ctx = context.WithValue(ctx, companyDownloadBaseKey{}, s.companyDownloadBase)
	return context.WithValue(ctx, companyIMContextKey{}, companyIMSubject{channel: channel, member: member}), nil
}

func imSessionMessage(ctx context.Context, channelID string, msg *IncomingMessage) *IncomingMessage {
	if _, ok := ctx.Value(companyIMContextKey{}).(companyIMSubject); !ok {
		return msg
	}
	copyMessage := *msg
	userID, _ := types.UserIDFromContext(ctx)
	copyMessage.UserID = userID + ":" + channelID
	return &copyMessage
}

// A full answer may finish long after the message was accepted. Recheck the
// directory immediately before disclosing it, without a background poller.
func (s *Service) authorizeIMAnswer(ctx context.Context, msg *IncomingMessage) (context.Context, error) {
	identity, ok := ctx.Value(companyIMContextKey{}).(companyIMSubject)
	if !ok {
		return ctx, nil
	}
	return s.authorizeIMIdentity(ctx, identity.channel, msg)
}

// Restrict the request clone, never the persisted agent. A newly added library
// cannot silently widen an existing company's IM channel.
func companyIMAgent(ctx context.Context, agent *types.CustomAgent) *types.CustomAgent {
	if _, ok := ctx.Value(companyIMContextKey{}).(companyIMSubject); !ok || agent == nil {
		return agent
	}
	copyAgent := *agent
	copyAgent.Config = agent.Config
	copyAgent.Config.AllowedTools = []string{}
	for _, name := range agenttools.EffectiveAllowedTools(agent.Config.AllowedTools) {
		switch name {
		case agenttools.ToolThinking, agenttools.ToolTodoWrite, agenttools.ToolAskUser,
			agenttools.ToolSearchKnowledge, agenttools.ToolReadDocument, agenttools.ToolListDocuments,
			agenttools.ToolQueryKnowledgeGraph, agenttools.ToolReadFile, agenttools.ToolWikiReadPage,
			agenttools.ToolWikiSearch, agenttools.ToolWikiReadIssue:
			copyAgent.Config.AllowedTools = append(copyAgent.Config.AllowedTools, name)
		}
	}
	copyAgent.Config.MCPSelectionMode = "none"
	copyAgent.Config.MCPServices = nil
	copyAgent.Config.SkillsSelectionMode = "none"
	copyAgent.Config.SelectedSkills = nil
	copyAgent.Config.AccessLevel = types.AssistantAccessReadOnly
	if scope, ok := types.TenantAPIKeyScopeFromContext(ctx); ok {
		copyAgent.Config.KnowledgeBases, _ = types.FilterKnowledgeBasesForTenantAPIKeyScope(ctx, nil, agent.Config.KnowledgeBases)
		if agent.Config.KBSelectionMode == "all" {
			copyAgent.Config.KnowledgeBases = append([]string(nil), scope.KnowledgeBaseIDs...)
		}
		if agent.Config.KBSelectionMode == "none" || len(copyAgent.Config.KnowledgeBases) == 0 {
			copyAgent.Config.KBSelectionMode = "none"
			copyAgent.Config.KnowledgeBases = nil
		} else {
			copyAgent.Config.KBSelectionMode = "selected"
		}
	}
	copyAgent.Config.SystemPrompt = strings.TrimSpace(copyAgent.Config.SystemPrompt + "\n\n" +
		"用户索取原始资料时，先检索并确认型号、文件名和版本。直接使用工具返回的 original_download 完整链接给出 [文件名](链接)，不要把 doc:// 文档句柄拼进下载地址，也不能编造ID或下载地址。" +
		"同系列存在多个型号或版本时先列出候选，必要时请用户选择。请说提供原件或下载入口，不要声称已经发送附件；文件实际发送及结果由系统处理。")
	return &copyAgent
}
