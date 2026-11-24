package im

import (
	"context"
	"errors"
	"fmt"
	"testing"

	agenttools "github.com/Tencent/WeKnora/internal/agent/tools"
	"github.com/Tencent/WeKnora/internal/companyidentity"
	"github.com/Tencent/WeKnora/internal/event"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/stretchr/testify/require"
)

type testEmployeeChecker struct {
	allowed     bool
	downloads   bool
	member      *companyidentity.Member
	lastWeComID string
	checks      int
}

func (c *testEmployeeChecker) LookupWeCom(_ context.Context, id string) (*companyidentity.Member, error) {
	c.lastWeComID = id
	c.checks++
	if !c.allowed {
		return nil, companyidentity.ErrDenied
	}
	return c.member, nil
}
func (c *testEmployeeChecker) Check(_ context.Context, b companyidentity.Binding) (*companyidentity.Member, error) {
	c.checks++
	if !c.allowed || b.Issuer != c.member.Issuer || b.Subject != c.member.Subject {
		return nil, companyidentity.ErrDenied
	}
	return c.member, nil
}
func (c *testEmployeeChecker) AllowedKnowledgeBaseIDs() []string { return []string{"vendor"} }
func (c *testEmployeeChecker) AuthorizeKnowledgeBases(ids []string) error {
	if len(ids) == 0 {
		return companyidentity.ErrDenied
	}
	for _, id := range ids {
		if id != "vendor" {
			return companyidentity.ErrDenied
		}
	}
	return nil
}
func (c *testEmployeeChecker) OriginalDownloadsAllowed() bool { return c.downloads }

func employeeFixture() *testEmployeeChecker {
	return &testEmployeeChecker{allowed: true, downloads: true, member: &companyidentity.Member{Issuer: "https://identity.example", Subject: "employee-1", CreatedTime: "12345"}}
}

func TestCompanyIMRequiresLiveEmployeeAndExactTransportIdentity(t *testing.T) {
	channel := &IMChannel{ID: "channel", TenantID: 7, Platform: "wecom"}
	msg := &IncomingMessage{Platform: PlatformWeCom, UserID: "ExactCase", ChatType: ChatTypeDirect}
	svc := &Service{}
	_, err := svc.authorizeIMIdentity(context.Background(), channel, msg)
	require.Error(t, err, "unconfigured directory must fail closed")
	checker := employeeFixture()
	svc.employeeChecker = checker
	ctx, err := svc.authorizeIMIdentity(context.Background(), channel, msg)
	require.NoError(t, err)
	require.Equal(t, "ExactCase", checker.lastWeComID)
	uid, _ := types.UserIDFromContext(ctx)
	require.NotEmpty(t, uid)
	require.False(t, types.IsSyntheticUserID(uid))
	require.Equal(t, types.TenantRoleViewer, types.TenantRoleFromContext(ctx))
	scope, ok := types.TenantAPIKeyScopeFromContext(ctx)
	require.True(t, ok)
	require.False(t, scope.FullAccess)
	require.Equal(t, types.StringArray{"vendor"}, scope.KnowledgeBaseIDs)
	require.Error(t, types.AuthorizeTenantAPIKeyKnowledgeBases(ctx, "warehouse"))
	msg.ChatType = ChatTypeGroup
	_, err = svc.authorizeIMIdentity(context.Background(), channel, msg)
	require.Error(t, err, "group must not expose personal employee grants")
	msg.ChatType = ChatTypeDirect
	checker.allowed = false
	_, err = svc.authorizeIMIdentity(ctx, channel, msg)
	require.Error(t, err, "revocation must be checked again, without a cached success")
}

func TestCompanyIMExplicitWeChatBindingAndSessionIsolation(t *testing.T) {
	checker := employeeFixture()
	svc := &Service{employeeChecker: checker}
	ch := &IMChannel{ID: "channel-1", TenantID: 7, Platform: "wechat", Credentials: types.JSON(`{"ilink_user_id":"wechat-sender","employee_issuer":"https://identity.example","employee_subject":"employee-1"}`)}
	msg := &IncomingMessage{Platform: PlatformWeChat, UserID: "wechat-sender"}
	ctx, err := svc.authorizeIMIdentity(context.Background(), ch, msg)
	require.NoError(t, err)
	bound := imSessionMessage(ctx, ch.ID, msg)
	require.NotEqual(t, msg.UserID, bound.UserID)
	require.Equal(t, "wechat-sender", msg.UserID, "delivery keeps the verified platform recipient")
	require.NotEqual(t, bound.UserID, imSessionMessage(ctx, "channel-2", msg).UserID)
	msg.UserID = "website-openid-or-another-wechat-user"
	_, err = svc.authorizeIMIdentity(context.Background(), ch, msg)
	require.Error(t, err, "website and iLink identifiers cannot be substituted")
	msg.UserID = "wechat-sender"
	checker.member = &companyidentity.Member{Issuer: "https://identity.example", Subject: "employee-1", CreatedTime: "recreated"}
	_, err = svc.authorizeIMIdentity(ctx, ch, msg)
	require.Error(t, err, "a recreated account cannot inherit a queued conversation")
}

func TestCompanyIMAgentIsBoundedWithoutMutatingStoredConfig(t *testing.T) {
	svc := &Service{employeeChecker: employeeFixture()}
	ctx, err := svc.authorizeIMIdentity(context.Background(), &IMChannel{ID: "channel", TenantID: 7, Platform: "wecom"}, &IncomingMessage{Platform: PlatformWeCom, UserID: "member"})
	require.NoError(t, err)
	agent := &types.CustomAgent{Config: types.CustomAgentConfig{KBSelectionMode: "all", SystemPrompt: "original",
		AllowedTools: []string{agenttools.ToolSearchKnowledge, agenttools.ToolShellExec, agenttools.ToolWikiWritePage, agenttools.ToolCallMCPTool}}}
	scoped := companyIMAgent(ctx, agent)
	require.Equal(t, []string{"vendor"}, scoped.Config.KnowledgeBases)
	require.Equal(t, "selected", scoped.Config.KBSelectionMode)
	require.Equal(t, "all", agent.Config.KBSelectionMode)
	require.Equal(t, "original", agent.Config.SystemPrompt)
	require.Equal(t, []string{agenttools.ToolSearchKnowledge}, scoped.Config.AllowedTools)
	require.Equal(t, "none", scoped.Config.MCPSelectionMode)
	require.Equal(t, types.AssistantAccessReadOnly, scoped.Config.AccessLevel)
	agent.Config.KBSelectionMode = "selected"
	agent.Config.KnowledgeBases = []string{"warehouse"}
	require.Equal(t, "none", companyIMAgent(ctx, agent).Config.KBSelectionMode)
}

func TestCompanyIMRechecksBeforeDelayedFullAnswerWithoutPolling(t *testing.T) {
	checker := employeeFixture()
	svc := &Service{employeeChecker: checker}
	channel := &IMChannel{ID: "channel", TenantID: 7, Platform: "wecom"}
	msg := &IncomingMessage{Platform: PlatformWeCom, UserID: "member"}
	ctx, err := svc.authorizeIMIdentity(context.Background(), channel, msg)
	require.NoError(t, err)
	require.Equal(t, 1, checker.checks)
	checker.allowed = false // employee revoked while a queued/long answer runs
	_, err = svc.authorizeIMAnswer(ctx, msg)
	require.Error(t, err)
	require.Equal(t, 2, checker.checks, "only message acceptance and delivery boundary query the directory")
}

type revokeDuringIMQA struct {
	*fullOutputSessionService
	checker *testEmployeeChecker
}

func (s *revokeDuringIMQA) KnowledgeQA(ctx context.Context, req *types.QARequest, bus *event.EventBus) error {
	s.checker.allowed = false
	return s.fullOutputSessionService.KnowledgeQA(ctx, req, bus)
}

func TestCompanyIMDoesNotDeliverQAResultAfterEmployeeRevocation(t *testing.T) {
	for _, progressFails := range []bool{false, true} {
		t.Run(fmt.Sprint(progressFails), func(t *testing.T) {
			service, adapter, qa, _ := newFullOutputHarness("restricted original manufacturer evidence")
			checker := employeeFixture()
			service.employeeChecker = checker
			service.sessionService = &revokeDuringIMQA{fullOutputSessionService: qa, checker: checker}
			if progressFails {
				adapter.startErr = errors.New("progress unavailable")
			}
			channel := &IMChannel{ID: "channel", TenantID: 7, Platform: "wecom"}
			msg := &IncomingMessage{Platform: PlatformWeCom, UserID: "member", Content: "question"}
			ctx, err := service.authorizeIMIdentity(context.Background(), channel, msg)
			require.NoError(t, err)
			err = service.handleMessageFullOutput(ctx, msg, &types.Session{ID: "session"}, nil, nil, nil, nil, adapter, adapter, "user", nil)
			require.Error(t, err)
			require.NotContains(t, adapter.finalContent, "restricted original manufacturer evidence")
			require.NotContains(t, adapter.plainContent, "restricted original manufacturer evidence")
			require.Zero(t, adapter.plainReplies)
			require.Equal(t, 2, checker.checks)
		})
	}
}
