package chatpipeline

import (
	"context"
	"strings"
	"testing"

	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
	"github.com/stretchr/testify/require"
)

func TestComparisonEvidenceSurvivesOneFamilyDominatingScores(t *testing.T) {
	objects := comparisonObjects("比较 AB571x、AB572x、AB573x 的总RAM")
	require.Equal(t, []string{"AB571x", "AB572x", "AB573x"}, objects)
	results := []*types.SearchResult{{ID: "a1", KnowledgeTitle: "AB5712A.pdf", Score: 1}, {ID: "a2", KnowledgeTitle: "AB5712A.pdf", Score: .9}, {ID: "a3", KnowledgeTitle: "AB5713B.pdf", Score: .8}, {ID: "b", KnowledgeTitle: "ab5721a_datasheet.pdf", Score: .3}, {ID: "c", KnowledgeTitle: "AB5732B.pdf", Score: .2}}
	kept := balancedComparison(results, objects, 3)
	require.Len(t, kept, 3)
	require.Equal(t, "a1", kept[0].ID)
	require.Equal(t, "b", kept[1].ID)
	require.Equal(t, "c", kept[2].ID)
	require.Empty(t, comparisonObjects("BT897x 的总RAM是多少"))
	require.Empty(t, comparisonObjects("比较 V017 和 V018 的区别"))
	require.Equal(t, "ab571", comparisonPrefix("AB571x"))
}

type comparisonScopeService struct {
	interfaces.KnowledgeService
	called bool
}

func (s *comparisonScopeService) SearchKnowledgeForScopes(context.Context, []types.KnowledgeSearchScope, string, int, int, []string) ([]*types.Knowledge, bool, int64, error) {
	s.called = true
	return nil, false, 0, nil
}

func TestComparisonPreservesExplicitDocumentsAndPredicate(t *testing.T) {
	kbs := &recordingSearchKnowledgeBaseService{}
	files := &comparisonScopeService{}
	plugin := &PluginSearch{knowledgeBaseService: kbs, knowledgeService: files}
	cm := &types.ChatManage{PipelineRequest: types.PipelineRequest{
		Query: "比较 AB571x 和 AB572x 在双MIC降噪方案下的 RAM", EmbeddingTopK: 12, DisableVectorMatch: true,
		SearchTargets: types.SearchTargets{{Type: types.SearchTargetTypeKnowledge, KnowledgeBaseID: "kb-1", KnowledgeIDs: []string{"chosen-document"}}},
	}, PipelineState: types.PipelineState{RewriteQuery: "比较 ab571x 和 ab572x 在双MIC降噪方案下的 RAM"}}
	plugin.searchComparison(context.Background(), cm)
	require.False(t, files.called, "filename resolution must not widen a user-selected scope")
	require.Len(t, kbs.params, 2)
	for i, query := range kbs.params {
		require.Equal(t, []string{"chosen-document"}, query.KnowledgeIDs)
		require.Contains(t, query.QueryText, "双MIC降噪方案")
		require.Contains(t, query.QueryText, "RAM")
		other := "ab572x"
		if i == 1 {
			other = "ab571x"
		}
		require.NotContains(t, strings.ToLower(query.QueryText), other)
	}
}

func TestComparisonAuxiliaryRecallExcludesNegatedClauseOnly(t *testing.T) {
	query := "比较 AB571x、AB572x 的 RAM，只比较芯片内置总量，不要把 cache 另加进去"
	require.Equal(t, "比较 AB571x、AB572x 的 RAM，只比较芯片内置总量", comparisonRecallQuery(query))
	require.Equal(t, "Compare AB571x and AB572x RAM", comparisonRecallQuery("Compare AB571x and AB572x RAM, excluding cache"))
	positive := "比较 AB571x、AB572x 的 RAM，在双MIC降噪方案下"
	require.Equal(t, positive, comparisonRecallQuery(positive))
	require.Equal(t, "不要比较 cache，只比较 RAM", comparisonRecallQuery("不要比较 cache，只比较 RAM"))
}
