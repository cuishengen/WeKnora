package im

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	agenttools "github.com/Tencent/WeKnora/internal/agent/tools"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/stretchr/testify/require"
)

const channelGrantTestKB = "33333333-3333-4333-8333-333333333333"

func channelGrantFixture(t *testing.T) (*Service, *IMChannel, *IncomingMessage, companyChannelGrant, string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("private grant files require the deployment's POSIX owner-only permissions")
	}
	channel := &IMChannel{ID: "11111111-1111-4111-8111-111111111111", TenantID: 7,
		AgentID: "22222222-2222-4222-8222-222222222222", Platform: "wecom", BotIdentity: "wecom:ws:test-bot",
		Credentials: types.JSON(`{"company_channel_grant_required":true}`)}
	grant := companyChannelGrant{ChannelID: channel.ID, TenantID: channel.TenantID,
		BotIdentity: channel.BotIdentity, AgentID: channel.AgentID,
		Members:          []companyChannelMember{{Issuer: "https://identity.example", Subject: "employee-1"}},
		KnowledgeBaseIDs: []string{channelGrantTestKB}}
	path := filepath.Join(t.TempDir(), "channel-grants.json")
	t.Setenv("COMPANY_IM_CHANNEL_GRANTS_FILE", path)
	writeChannelGrant(t, path, []companyChannelGrant{grant})
	return &Service{employeeChecker: employeeFixture()}, channel,
		&IncomingMessage{Platform: PlatformWeCom, UserID: "ExactEmployeeID", ChatType: ChatTypeDirect}, grant, path
}

func writeChannelGrant(t *testing.T, path string, grants []companyChannelGrant) {
	t.Helper()
	raw, err := json.Marshal(map[string]any{"version": 1, "channels": grants})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, raw, 0600))
	require.NoError(t, os.Chmod(path, 0600))
}

func TestLegacyCompanyIMDoesNotReadChannelGrantFile(t *testing.T) {
	for _, corrupt := range []bool{false, true} {
		t.Run(map[bool]string{false: "missing", true: "invalid"}[corrupt], func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "grants.json")
			if corrupt {
				require.NoError(t, os.WriteFile(path, []byte(`not-json`), 0600))
			}
			t.Setenv("COMPANY_IM_CHANNEL_GRANTS_FILE", path)
			svc := &Service{employeeChecker: employeeFixture()}
			ctx, err := svc.authorizeIMIdentity(context.Background(), &IMChannel{ID: "legacy", TenantID: 7, Platform: "wecom"},
				&IncomingMessage{Platform: PlatformWeCom, UserID: "employee", ChatType: ChatTypeDirect})
			require.NoError(t, err)
			scope, _ := types.TenantAPIKeyScopeFromContext(ctx)
			require.Equal(t, types.StringArray{"vendor"}, scope.KnowledgeBaseIDs)
			require.Error(t, types.AuthorizeTenantAPIKeyKnowledgeBases(ctx, channelGrantTestKB))
		})
	}
}

func TestChannelGrantRestrictsCreatorLibrariesAndAgentTools(t *testing.T) {
	svc, channel, msg, _, _ := channelGrantFixture(t)
	ctx, err := svc.authorizeIMIdentity(context.Background(), channel, msg)
	require.NoError(t, err)
	require.NoError(t, types.AuthorizeTenantAPIKeyKnowledgeBases(ctx, channelGrantTestKB))
	require.Error(t, types.AuthorizeTenantAPIKeyKnowledgeBases(ctx, "vendor"))
	require.Equal(t, []string{"vendor"}, svc.employeeChecker.AllowedKnowledgeBaseIDs(), "never expand MCP or portal deployment scope")
	agent := &types.CustomAgent{Config: types.CustomAgentConfig{KBSelectionMode: "all", SystemPrompt: "inventory",
		AllowedTools: []string{agenttools.ToolSearchKnowledge, agenttools.ToolReadDocument, agenttools.ToolListDocuments,
			agenttools.ToolReadFile, agenttools.ToolShellExec, agenttools.ToolCallMCPTool}, WebSearchEnabled: true, WebFetchEnabled: true}}
	scoped := companyIMAgent(ctx, agent)
	require.Equal(t, []string{channelGrantTestKB}, scoped.Config.KnowledgeBases)
	require.Equal(t, "selected", scoped.Config.KBSelectionMode)
	require.Equal(t, []string{agenttools.ToolSearchKnowledge, agenttools.ToolReadDocument, agenttools.ToolListDocuments}, scoped.Config.AllowedTools)
	require.False(t, scoped.Config.WebSearchEnabled)
	require.False(t, scoped.Config.WebFetchEnabled)
	require.Equal(t, "none", scoped.Config.MCPSelectionMode)
	require.Contains(t, scoped.Config.SystemPrompt, "本渠道未开放原件预览、下载或附件发送")
	require.NotContains(t, scoped.Config.SystemPrompt, "original_download 完整链接")
	require.True(t, agent.Config.WebSearchEnabled, "do not mutate the stored agent")
	agent.Config.KBSelectionMode, agent.Config.KnowledgeBases = "selected", []string{"vendor"}
	require.Equal(t, "none", companyIMAgent(ctx, agent).Config.KBSelectionMode)
	svc.employeeChecker.(*testEmployeeChecker).member.Subject = "another-employee"
	_, err = svc.authorizeIMIdentity(context.Background(), channel, msg)
	require.Error(t, err)
}

func TestRequiredChannelGrantRejectsMissingInvalidAndMismatchedPolicies(t *testing.T) {
	for _, scenario := range []string{"missing", "unset", "invalid", "public_file", "symlink", "empty_members", "empty_libraries", "duplicate_library", "duplicate_channel", "wrong_tenant", "wrong_bot", "wrong_agent", "missing_channel", "unknown_field"} {
		t.Run(scenario, func(t *testing.T) {
			svc, channel, msg, grant, path := channelGrantFixture(t)
			switch scenario {
			case "missing":
				require.NoError(t, os.Remove(path))
			case "unset":
				t.Setenv("COMPANY_IM_CHANNEL_GRANTS_FILE", "")
			case "invalid":
				require.NoError(t, os.WriteFile(path, []byte(`{`), 0600))
			case "public_file":
				require.NoError(t, os.Chmod(path, 0644))
			case "symlink":
				require.NoError(t, os.Symlink(path, path+".link"))
				t.Setenv("COMPANY_IM_CHANNEL_GRANTS_FILE", path+".link")
			case "unknown_field":
				raw, err := json.Marshal(map[string]any{"version": 1, "channels": []companyChannelGrant{grant}, "full_access": true})
				require.NoError(t, err)
				require.NoError(t, os.WriteFile(path, raw, 0600))
			default:
				switch scenario {
				case "empty_members":
					grant.Members = nil
				case "empty_libraries":
					grant.KnowledgeBaseIDs = nil
				case "duplicate_library":
					grant.KnowledgeBaseIDs = append(grant.KnowledgeBaseIDs, channelGrantTestKB)
				case "wrong_tenant":
					grant.TenantID++
				case "wrong_bot":
					grant.BotIdentity += "-different"
				case "wrong_agent":
					grant.AgentID = "44444444-4444-4444-8444-444444444444"
				case "missing_channel":
					grant.ChannelID = "55555555-5555-4555-8555-555555555555"
				}
				grants := []companyChannelGrant{grant}
				if scenario == "duplicate_channel" {
					grants = append(grants, grant)
				}
				writeChannelGrant(t, path, grants)
			}
			_, err := svc.authorizeIMIdentity(context.Background(), channel, msg)
			require.Error(t, err, "required channels must never fall back to vendor access")
		})
	}
}

func TestChannelGrantRechecksRevocationAndIsolatesChangedScopeHistory(t *testing.T) {
	svc, channel, msg, grant, path := channelGrantFixture(t)
	ctx, err := svc.authorizeIMIdentity(context.Background(), channel, msg)
	require.NoError(t, err)
	grant.KnowledgeBaseIDs = []string{"44444444-4444-4444-8444-444444444444"}
	writeChannelGrant(t, path, []companyChannelGrant{grant})
	_, err = svc.authorizeIMAnswer(ctx, msg)
	require.Error(t, err, "queued replies cannot cross a changed grant")
	fresh, err := svc.authorizeIMIdentity(context.Background(), channel, msg)
	require.NoError(t, err)
	require.NotEqual(t, imSessionMessage(ctx, channel.ID, msg).UserID, imSessionMessage(fresh, channel.ID, msg).UserID)
	require.NoError(t, os.Remove(path))
	_, err = svc.authorizeIMAnswer(fresh, msg)
	require.Error(t, err)
	_, err = svc.authorizeIMIdentity(context.Background(), channel, msg)
	require.Error(t, err, "removing the entry/file denies subsequent messages too")
}

func TestChannelGrantDoesNotEnableOriginalDelivery(t *testing.T) {
	svc, channel, msg, _, _ := channelGrantFixture(t)
	ctx, err := svc.authorizeIMIdentity(context.Background(), channel, msg)
	require.NoError(t, err)
	files := &originalTestService{row: &types.Knowledge{ID: originalTestID, TenantID: 7, KnowledgeBaseID: channelGrantTestKB}, data: []byte("inventory original")}
	svc.knowledgeService = files
	adapter := &originalTestAdapter{limit: maxIMAttachmentBytes}
	msg.Content = "把盘点表原件发给我"
	agent := &types.CustomAgent{Config: types.CustomAgentConfig{KBSelectionMode: "selected", KnowledgeBases: []string{channelGrantTestKB}}}
	svc.sendOriginalFiles(ctx, msg, adapter, agent, nil, "/platform/source-download/"+originalTestID)
	require.Zero(t, files.lookups, "query-only channel grants cannot use the deployment's original-file entitlement")
	require.Zero(t, files.opens)
	require.Empty(t, adapter.files)
}
