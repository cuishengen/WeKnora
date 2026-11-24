package chatpipeline

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
	"github.com/stretchr/testify/require"
)

type comparisonAttributeCall struct {
	kb     string
	params types.SearchParams
}

type comparisonAttributeKBService struct {
	interfaces.KnowledgeBaseService
	calls []comparisonAttributeCall
	hits  map[string][]*types.SearchResult
}

func (s *comparisonAttributeKBService) HybridSearch(_ context.Context, kb string, params types.SearchParams) ([]*types.SearchResult, error) {
	s.calls = append(s.calls, comparisonAttributeCall{kb, params})
	if len(params.KnowledgeIDs) != 1 {
		return nil, fmt.Errorf("expected one constrained document")
	}
	return s.hits[kb+"/"+params.KnowledgeIDs[0]], nil
}

func TestComparisonAttributeLookupCoversCopiesAndFamiliesWithinBudget(t *testing.T) {
	service := &comparisonAttributeKBService{hits: map[string][]*types.SearchResult{}}
	var seeds []*types.SearchResult
	add := func(id, title string) {
		seed := &types.SearchResult{ID: id + "-value", KnowledgeID: id, KnowledgeBaseID: "kb-1", KnowledgeTitle: title}
		seeds = append(seeds, seed)
		service.hits["kb-1/"+id] = []*types.SearchResult{
			{ID: id + "-retention", KnowledgeID: id, KnowledgeBaseID: "kb-1", KnowledgeTitle: title,
				Content: "Supply current during RAM retention 0.2mA", Score: 0.99},
			{ID: seed.ID, KnowledgeID: id, KnowledgeBaseID: "kb-1", KnowledgeTitle: title,
				Content: "Internal 128KB RAM for data and program", Score: 0.1},
		}
	}
	for i := 1; i <= 5; i++ {
		add(fmt.Sprintf("first-%d", i), fmt.Sprintf("zx410%dA_datasheet.pdf", i))
	}
	for i := 2; i <= 4; i++ {
		add(fmt.Sprintf("second-%d", i), fmt.Sprintf("zx420%dA_datasheet.pdf", i))
	}
	oldIndex := len(seeds)
	add("earlier-copy", "zx4201A_datasheet.pdf")
	add("later-copy", "zx4201A_datasheet.pdf")
	add("third-family", "zx4301A_datasheet.pdf")
	add("third-family-second", "zx4302A_datasheet.pdf")
	cm := &types.ChatManage{
		PipelineRequest: types.PipelineRequest{SearchTargets: types.SearchTargets{{
			Type: types.SearchTargetTypeKnowledgeBase, KnowledgeBaseID: "kb-1",
		}}},
		PipelineState: types.PipelineState{SearchResult: seeds},
	}
	plugin := &PluginSearch{knowledgeBaseService: service}
	results := plugin.searchComparisonDocumentAttributes(context.Background(), cm, nil,
		[]string{"ZX410x", "ZX420x", "ZX430x"}, "RAM")
	require.LessOrEqual(t, len(service.calls), 6)
	queried := map[string]bool{}
	for _, call := range service.calls {
		require.Equal(t, "RAM", call.params.QueryText, "do not add an answer value, unit or synonym to the query")
		require.Equal(t, 4, call.params.MatchCount)
		require.True(t, call.params.DisableVectorMatch)
		require.True(t, call.params.SkipContextEnrichment)
		queried[call.params.KnowledgeIDs[0]] = true
	}
	require.True(t, queried["earlier-copy"])
	require.True(t, queried["later-copy"])
	require.True(t, queried["third-family"], "the first family's many documents must not consume the budget")
	for _, result := range results {
		require.NotContains(t, result.ID, "retention")
		require.Equal(t, "true", result.Metadata[comparisonAttributeEvidenceKey])
	}
	require.Equal(t, "true", seeds[oldIndex].Metadata[comparisonAttributeEvidenceKey], "mark the original copy before later ID deduplication")
}

func TestComparisonAttributeLookupPreservesSelectedDocumentsAndTagScopes(t *testing.T) {
	service := &comparisonAttributeKBService{hits: map[string][]*types.SearchResult{
		"kb-1/selected": {{ID: "selected-value", KnowledgeBaseID: "kb-1", KnowledgeID: "selected", Content: "RAM: 24KB"}},
		"kb-2/tagged": {
			{ID: "wrong-kb", KnowledgeBaseID: "kb-other", KnowledgeID: "tagged", Content: "RAM: 99KB"},
			{ID: "tagged-value", KnowledgeBaseID: "kb-2", KnowledgeID: "tagged", Content: "RAM: 48KB"},
		},
	}}
	cm := &types.ChatManage{
		PipelineRequest: types.PipelineRequest{SearchTargets: types.SearchTargets{
			{Type: types.SearchTargetTypeKnowledge, KnowledgeBaseID: "kb-1", KnowledgeIDs: []string{"selected"},
				TagIDs: []string{"physical-1"}, ScopeTagIDs: []string{"logical-1"}},
			{Type: types.SearchTargetTypeKnowledgeBase, KnowledgeBaseID: "kb-1", ScopeTagIDs: []string{"unresolved-logical"}},
			{Type: types.SearchTargetTypeKnowledgeBase, KnowledgeBaseID: "kb-2", TagIDs: []string{"physical-2"}, ScopeTagIDs: []string{"logical-2"}},
		}},
		PipelineState: types.PipelineState{SearchResult: []*types.SearchResult{
			{ID: "seed-selected", KnowledgeBaseID: "kb-1", KnowledgeID: "selected", KnowledgeTitle: "ZX4101A.pdf"},
			{ID: "seed-unselected", KnowledgeBaseID: "kb-1", KnowledgeID: "unselected", KnowledgeTitle: "ZX4101B.pdf"},
			{ID: "seed-tagged", KnowledgeBaseID: "kb-2", KnowledgeID: "tagged", KnowledgeTitle: "ZX4201A.pdf"},
			{ID: "seed-other-kb", KnowledgeBaseID: "kb-other", KnowledgeID: "selected", KnowledgeTitle: "ZX4201A.pdf"},
		}},
	}
	plugin := &PluginSearch{knowledgeBaseService: service}
	results := plugin.searchComparisonDocumentAttributes(context.Background(), cm, nil, []string{"ZX410x", "ZX420x"}, "RAM")
	require.Len(t, service.calls, 2)
	require.Len(t, results, 2)
	for _, call := range service.calls {
		suffix := "1"
		if call.kb == "kb-2" {
			suffix = "2"
		}
		require.Equal(t, []string{"physical-" + suffix}, call.params.TagIDs)
		require.Equal(t, []string{"logical-" + suffix}, call.params.ScopeTagIDs)
		require.NotContains(t, call.params.KnowledgeIDs, "unselected")
	}
	for _, result := range results {
		require.NotEqual(t, "wrong-kb", result.ID)
	}
}

func TestComparisonAttributeLookupDoesNotPromoteRetentionOrInventValues(t *testing.T) {
	service := &comparisonAttributeKBService{hits: map[string][]*types.SearchResult{
		"kb-1/member": {
			{ID: "faq", KnowledgeBaseID: "kb-1", KnowledgeID: "member", ChunkType: string(types.ChunkTypeFAQ), Content: "RAM: 512KB"},
			{ID: "retention", KnowledgeBaseID: "kb-1", KnowledgeID: "member", Content: "Supply current during RAM retention 0.2mA"},
			{ID: "missing", KnowledgeBaseID: "kb-1", KnowledgeID: "member", Content: "RAM capacity is not specified"},
		},
	}}
	cm := &types.ChatManage{
		PipelineRequest: types.PipelineRequest{SearchTargets: types.SearchTargets{{Type: types.SearchTargetTypeKnowledgeBase, KnowledgeBaseID: "kb-1"}}},
		PipelineState: types.PipelineState{SearchResult: []*types.SearchResult{{
			ID: "seed", KnowledgeID: "member", KnowledgeBaseID: "kb-1", KnowledgeTitle: "ZX4101A.pdf",
		}}},
	}
	plugin := &PluginSearch{knowledgeBaseService: service}
	objects := []string{"ZX410x", "ZX420x"}
	require.Empty(t, plugin.searchComparisonDocumentAttributes(context.Background(), cm, nil, objects, "RAM"))
	before := len(service.calls)
	cm.DisableKeywordsMatch = true
	require.Empty(t, plugin.searchComparisonDocumentAttributes(context.Background(), cm, nil, objects, "RAM"))
	require.Len(t, service.calls, before)
	cm.DisableKeywordsMatch = false
	require.Empty(t, plugin.searchComparisonDocumentAttributes(context.Background(), cm, nil, objects, ""))
	require.Len(t, service.calls, before)
}

func TestComparisonAttributeLookupGlobalBudgetStillGivesEachFamilyOneTurn(t *testing.T) {
	service := &comparisonAttributeKBService{hits: map[string][]*types.SearchResult{}}
	cm := &types.ChatManage{PipelineRequest: types.PipelineRequest{SearchTargets: types.SearchTargets{{
		Type: types.SearchTargetTypeKnowledgeBase, KnowledgeBaseID: "kb-1",
	}}}}
	var objects []string
	for family := 1; family <= 6; family++ {
		objects = append(objects, fmt.Sprintf("ZX%d0x", family))
		for member := 1; member <= 3; member++ {
			id := fmt.Sprintf("%d-%d", family, member)
			cm.SearchResult = append(cm.SearchResult, &types.SearchResult{
				ID: id, KnowledgeID: id, KnowledgeBaseID: "kb-1", KnowledgeTitle: fmt.Sprintf("ZX%d0%dA.pdf", family, member),
			})
		}
	}
	plugin := &PluginSearch{knowledgeBaseService: service}
	plugin.searchComparisonDocumentAttributes(context.Background(), cm, nil, objects, "RAM")
	require.Len(t, service.calls, 6)
	for family, call := range service.calls {
		require.True(t, strings.HasPrefix(call.params.KnowledgeIDs[0], fmt.Sprintf("%d-", family+1)))
	}
}

func TestBalancedComparisonReservesSubstantiveCopiesInsideExistingTopK(t *testing.T) {
	results := []*types.SearchResult{
		{ID: "retention", KnowledgeBaseID: "kb-1", KnowledgeID: "old", KnowledgeTitle: "ZX4101A.pdf", Score: 0.99},
		{ID: "other", KnowledgeBaseID: "kb-1", KnowledgeID: "other", KnowledgeTitle: "ZX4201A.pdf", Score: 0.9},
		{ID: "old-value", KnowledgeBaseID: "kb-1", KnowledgeID: "old", KnowledgeTitle: "ZX4101A.pdf", Score: 0.2,
			Metadata: map[string]string{comparisonAttributeEvidenceKey: "true"}},
		{ID: "new-value", KnowledgeBaseID: "kb-1", KnowledgeID: "new", KnowledgeTitle: "ZX4101A.pdf", Score: 0.1,
			Metadata: map[string]string{comparisonAttributeEvidenceKey: "true"}},
	}
	kept := balancedComparison(results, []string{"ZX410x", "ZX420x"}, 3)
	require.Len(t, kept, 3)
	require.Equal(t, "old-value", kept[0].ID)
	require.Equal(t, "other", kept[1].ID)
	kept = balancedComparison(results, []string{"ZX410x", "ZX420x"}, 4)
	require.Len(t, kept, 4)
	require.Equal(t, "old-value", kept[0].ID)
	require.Equal(t, "new-value", kept[1].ID)
	require.Equal(t, "other", kept[2].ID)
	require.False(t, strings.Contains(kept[0].ID, "retention"))
}
