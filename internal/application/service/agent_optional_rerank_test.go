package service

import (
	"context"
	"errors"
	"testing"

	"github.com/Tencent/WeKnora/internal/config"
	"github.com/Tencent/WeKnora/internal/event"
	"github.com/Tencent/WeKnora/internal/models/rerank"
	"github.com/Tencent/WeKnora/internal/sandbox"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
	"github.com/stretchr/testify/require"
)

var errOptionalRerankNextStage = errors.New("test: reached attachment staging after model resolution")

type optionalRerankModels struct {
	*stubModelService
	configuredCalls []string
}

func (s *optionalRerankModels) GetRerankModel(_ context.Context, id string) (rerank.Reranker, error) {
	s.configuredCalls = append(s.configuredCalls, id)
	return nil, errors.New("configured reranker unavailable")
}

type optionalRerankStager struct{ interfaces.AgentService }

func (*optionalRerankStager) sessionSandboxInputStore(context.Context, string, string) (sandbox.SessionFileStore, error) {
	return nil, errOptionalRerankNextStage
}

func (*optionalRerankStager) stageSessionAttachments(context.Context, string, string, uint64, types.MessageAttachments, sandbox.WorkspaceLayout) ([]stagedSessionAttachment, error) {
	return nil, errors.New("unexpected attachment staging")
}

type optionalRerankKBs struct {
	interfaces.KnowledgeBaseService
}

func (*optionalRerankKBs) GetKnowledgeBasesByIDsOnly(context.Context, []string) ([]*types.KnowledgeBase, error) {
	return []*types.KnowledgeBase{{ID: "vendor", TenantID: 7}}, nil
}

func TestAgentQAOptionalRerankDoesNotRejectSearchButExplicitBadModelFails(t *testing.T) {
	for _, modelID := range []string{"", "configured-missing-reranker"} {
		t.Run(modelID, func(t *testing.T) {
			models := &optionalRerankModels{stubModelService: &stubModelService{
				chatModel: &fakeAgentChatModel{}, modelsByID: map[string]*types.Model{
					"chat": {ID: "chat", TenantID: 7, Type: types.ModelTypeKnowledgeQA, Status: types.ModelStatusActive},
				}}}
			svc := &sessionService{cfg: &config.Config{Conversation: &config.ConversationConfig{}, Tenant: &config.TenantConfig{}},
				modelService: models, agentService: &optionalRerankStager{}, knowledgeBaseService: &optionalRerankKBs{},
				webSearchProviderRepo: &sharedAgentWebSearchRepo{}}
			ctx := types.WithCaller(context.WithValue(context.Background(), types.TenantIDContextKey, uint64(7)),
				types.Caller{TenantID: 7, UserID: "employee", Role: types.TenantRoleViewer})
			ctx = context.WithValue(ctx, types.TenantInfoContextKey, &types.Tenant{ID: 7})
			err := svc.AgentQA(ctx, &types.QARequest{Session: &types.Session{ID: "optional-rerank", TenantID: 7},
				Query: "find the original datasheet", KnowledgeBaseIDs: []string{"vendor"}, TurnLeaseHeld: true, SharedAgentReadOnly: true,
				CustomAgent: &types.CustomAgent{ID: "custom", TenantID: 7, Config: types.CustomAgentConfig{
					ModelID: "chat", RerankModelID: modelID, KBSelectionMode: "selected", KnowledgeBases: []string{"vendor"},
					AllowedTools: []string{"search_knowledge", "read_document"}, SkillsSelectionMode: "none", MCPSelectionMode: "none",
				}}}, event.NewEventBus())
			if modelID == "" {
				require.ErrorIs(t, err, errOptionalRerankNextStage)
				require.Empty(t, models.configuredCalls, "an absent optional reranker must not invoke a model")
			} else {
				require.ErrorContains(t, err, "configured reranker unavailable")
				require.Equal(t, []string{modelID}, models.configuredCalls)
			}
		})
	}
}

type optionalRerankShareKBs struct {
	interfaces.KnowledgeBaseRepository
}

func (*optionalRerankShareKBs) GetKnowledgeBaseByIDs(context.Context, []string) ([]*types.KnowledgeBase, error) {
	return nil, nil
}

type optionalRerankShareOrg struct {
	interfaces.OrganizationRepository
}

func (*optionalRerankShareOrg) GetByID(context.Context, string) (*types.Organization, error) {
	return nil, errOptionalRerankNextStage
}

func TestShareAgentWithSearchDoesNotRequireOptionalRerank(t *testing.T) {
	svc := &agentShareService{agentRepo: &builtinShareAgentRepo{agent: &types.CustomAgent{ID: "custom", TenantID: 7,
		Config: types.CustomAgentConfig{ModelID: "chat", KBSelectionMode: "all", AllowedTools: []string{"search_knowledge"}}}},
		kbRepo: &optionalRerankShareKBs{}, orgRepo: &optionalRerankShareOrg{}}
	ctx := context.WithValue(context.Background(), types.TenantRoleContextKey, types.TenantRoleAdmin)
	_, err := svc.ShareAgent(ctx, "custom", "organization", "employee", 7, types.OrgRoleViewer)
	require.ErrorIs(t, err, errOptionalRerankNextStage, "missing optional rerank must not stop the normal share checks")
}
