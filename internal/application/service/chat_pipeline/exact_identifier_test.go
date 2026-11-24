package chatpipeline

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/Tencent/WeKnora/internal/reranking"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/stretchr/testify/require"
)

func TestSingleExactIdentifierPreservesTheRequestedPart(t *testing.T) {
	for _, tt := range []struct{ query, rewrite, want string }{
		{"MX1248A的库存还有多少", "", "MX1248A"},
		{"MX1248A 和 mx1248a 的库存", "", "mx1248a"},
		{"MX1248A 的库存", "MX1248B 的库存", "MX1248A"},
		{"那库存还有多少", "MX1248A 的库存还有多少", "MX1248A"},
		{"MX1248A-B 的规格", "", "MX1248A-B"},
		{"MX124x 的规格", "", ""},
		{"MX124* 的规格", "", ""},
		{"比较 MX1248A 和 MX1248B", "", ""},
		{"仓库的库存汇总", "", ""},
	} {
		t.Run(tt.query, func(t *testing.T) {
			cm := &types.ChatManage{PipelineRequest: types.PipelineRequest{Query: tt.query},
				PipelineState: types.PipelineState{RewriteQuery: tt.rewrite}}
			require.Equal(t, tt.want, singleExactIdentifier(cm))
		})
	}
}

func TestExactIdentifierRequiresCompletePartNumber(t *testing.T) {
	match := exactIdentifierMatcher("MX1248A")
	for _, text := range []string{"物料 MX1248A 库存", "MX1248A的数量", "| mx1248a | 17 |"} {
		require.True(t, match.MatchString(text), text)
	}
	for _, text := range []string{"MX1248B", "MX1248A1", "MX1248A-B", "MX1248A_B", "MX1248AX", "ZMX1248A"} {
		require.False(t, match.MatchString(text), text)
	}
}

type exactIdentifierKBService struct {
	recordingSearchKnowledgeBaseService
	original []*types.SearchResult
	literal  []*types.SearchResult
	kbIDs    []string
	callers  []interface{}
}

func (s *exactIdentifierKBService) HybridSearch(ctx context.Context, id string, params types.SearchParams) ([]*types.SearchResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.params = append(s.params, params)
	s.kbIDs = append(s.kbIDs, id)
	s.callers = append(s.callers, ctx.Value(types.TenantIDContextKey))
	if params.DisableVectorMatch && params.QueryText == "MX1248A" {
		return s.literal, nil
	}
	return s.original, nil
}

func TestExactIdentifierRecallKeepsAuthorizedTargetsAndSwitches(t *testing.T) {
	for _, target := range []*types.SearchTarget{
		{Type: types.SearchTargetTypeKnowledgeBase, KnowledgeBaseID: "kb-shared", TenantID: 2},
		{Type: types.SearchTargetTypeKnowledge, KnowledgeBaseID: "kb-shared", TenantID: 2,
			KnowledgeIDs: []string{"chosen-doc"}, ScopeTagIDs: []string{"selected-document-tag"}},
		{Type: types.SearchTargetTypeKnowledgeBase, KnowledgeBaseID: "kb-shared", TenantID: 2,
			TagIDs: []string{"selected-faq-tag"}, ScopeTagIDs: []string{"selected-faq-tag"}},
	} {
		t.Run(string(target.Type)+strings.Join(target.TagIDs, ",")+strings.Join(target.KnowledgeIDs, ","), func(t *testing.T) {
			svc := &exactIdentifierKBService{literal: []*types.SearchResult{
				{ID: "exact", Content: "| MX1248A | 17 |"},
				{ID: "suffix", Content: "| MX1248A-B | 200 |"},
				{ID: "title-only", KnowledgeTitle: "MX1248A", Content: "MX1248B"},
				{ID: "faq", Content: "MX1248A unrelated question", ChunkType: string(types.ChunkTypeFAQ), Score: 1},
			}}
			cm := &types.ChatManage{PipelineRequest: types.PipelineRequest{TenantID: 1,
				Query: "MX1248A的库存还有多少", SearchTargets: types.SearchTargets{target},
				EmbeddingTopK: 8, KeywordThreshold: .8},
				PipelineState: types.PipelineState{RewriteQuery: "MX1248A 的剩余库存"}}
			ctx := context.WithValue(context.Background(), types.TenantIDContextKey, uint64(1))
			got := (&PluginSearch{knowledgeBaseService: svc}).searchExactIdentifier(ctx, cm)
			require.Len(t, got, 1)
			require.Equal(t, "exact", got[0].ID)
			require.Equal(t, []string{"kb-shared"}, svc.kbIDs)
			require.Equal(t, []interface{}{uint64(1)}, svc.callers, "keep the original caller for authorization")
			require.Len(t, svc.params, 1)
			params := svc.params[0]
			require.Equal(t, target.KnowledgeIDs, params.KnowledgeIDs)
			require.Equal(t, target.TagIDs, params.TagIDs)
			require.Equal(t, target.ScopeTagIDs, params.ScopeTagIDs)
			require.Empty(t, params.KnowledgeBaseIDs)
			require.Equal(t, "MX1248A", params.QueryText)
			require.True(t, params.DisableVectorMatch)
			require.False(t, params.DisableKeywordsMatch)
			require.True(t, params.SkipContextEnrichment)
			require.Zero(t, params.KeywordThreshold)
			require.Zero(t, svc.embedCalls)
			require.Equal(t, 8, cm.EmbeddingTopK)
			require.Equal(t, .8, cm.KeywordThreshold)
			require.Equal(t, "MX1248A 的剩余库存", cm.RewriteQuery)

			cm.DisableKeywordsMatch = true
			require.Empty(t, (&PluginSearch{knowledgeBaseService: svc}).searchExactIdentifier(ctx, cm))
			require.Len(t, svc.params, 1, "vector-only requests must not gain a keyword path")
		})
	}
}

func TestLiteralRecallSurvivesBothPipelineTopKCuts(t *testing.T) {
	svc := &exactIdentifierKBService{
		original: []*types.SearchResult{{ID: "near", Content: "MX1248B inventory", Score: .9},
			{ID: "other", Content: "MX1248C inventory", Score: .8}},
		literal: []*types.SearchResult{{ID: "exact", Content: "MX1248A inventory", Score: .01}},
	}
	cm := &types.ChatManage{PipelineRequest: types.PipelineRequest{Query: "MX1248A的库存还有多少",
		SearchTargets: types.SearchTargets{{Type: types.SearchTargetTypeKnowledgeBase, KnowledgeBaseID: "kb-1"}},
		EmbeddingTopK: 2, RerankTopK: 1},
		PipelineState: types.PipelineState{RewriteQuery: "MX1248A的库存还有多少"}}
	next := func() *PluginError { return nil }
	require.Nil(t, (&PluginSearch{knowledgeBaseService: svc}).OnEvent(context.Background(), types.CHUNK_SEARCH, cm, next))
	require.Len(t, cm.SearchResult, 3)
	selected := (&PluginMerge{}).selectInputResults(context.Background(), cm)
	require.Len(t, selected, 1)
	require.Equal(t, "exact", selected[0].ID)
	// Exercise the final cap independently of the earlier Merge cap.
	cm.MergeResult = append([]*types.SearchResult(nil), cm.SearchResult...)
	require.Nil(t, (&PluginFilterTopK{}).OnEvent(context.Background(), types.FILTER_TOP_K, cm, next))
	require.Len(t, cm.MergeResult, 1)
	require.Equal(t, "exact", cm.MergeResult[0].ID)
}

func TestRerankCannotEraseExactIdentifierEvidence(t *testing.T) {
	for _, exactScore := range []float64{.01, .4} {
		t.Run(fmt.Sprint(exactScore), func(t *testing.T) {
			cm := rerankChatManage(.3)
			cm.Query = "MX1248A的库存还有多少"
			cm.RewriteQuery = cm.Query
			cm.RerankTopK = 1
			cm.SearchResult = []*types.SearchResult{
				{ID: "near", Content: "MX1248B inventory", Score: .9},
				{ID: "exact", Content: "MX1248A inventory", Score: .1},
			}
			plugin := &PluginRerank{modelService: &rerankOnlyModelService{reranker: &fixedReranker{scores: []float64{.9, exactScore}}}}
			require.Nil(t, plugin.OnEvent(context.Background(), types.CHUNK_RERANK, cm, func() *PluginError { return nil }))
			require.Len(t, cm.RerankResult, 1)
			require.Equal(t, "exact", cm.RerankResult[0].ID)
			require.Equal(t, 1, cm.RerankDiagnostics.ResultCount)
		})
	}
}

func TestRerankCandidateCapCannotEraseExactIdentifierEvidence(t *testing.T) {
	cm := rerankChatManage(.3)
	cm.Query, cm.RewriteQuery, cm.RerankTopK = "MX1248A 的库存", "MX1248A 的库存", 1
	cm.SearchResult = nil
	scores := make([]float64, reranking.DefaultMaxCandidates)
	for i := range scores {
		scores[i] = .9
		cm.SearchResult = append(cm.SearchResult, &types.SearchResult{
			ID: fmt.Sprint(i), Content: fmt.Sprintf("other inventory record %d", i), Score: .9,
		})
	}
	cm.SearchResult = append(cm.SearchResult, &types.SearchResult{ID: "exact", Content: "MX1248A inventory", Score: .01})
	plugin := &PluginRerank{modelService: &rerankOnlyModelService{reranker: &fixedReranker{scores: scores}}}
	require.Nil(t, plugin.OnEvent(context.Background(), types.CHUNK_RERANK, cm, func() *PluginError { return nil }))
	require.Equal(t, reranking.DefaultMaxCandidates, cm.RerankDiagnostics.CandidateCount)
	require.Len(t, cm.RerankResult, 1)
	require.Equal(t, "exact", cm.RerankResult[0].ID)
}

func TestExactIdentifierRetentionIsBoundedAndNeverSubstitutesSuffixes(t *testing.T) {
	ranked := []*types.SearchResult{{ID: "near", Content: "MX1248A-B inventory", Score: .9}}
	unknown := retainExactIdentifierEvidence(ranked, []*types.SearchResult{
		{ID: "other-suffix", Content: "MX1248A1 inventory"},
	}, "MX1248A", 5)
	require.Equal(t, ranked, unknown, "missing exact source must not promote related part numbers")
	var many []*types.SearchResult
	for _, id := range []string{"one", "two", "three", "four", "five"} {
		many = append(many, &types.SearchResult{ID: id, Content: "MX1248A details"})
	}
	kept := retainExactIdentifierEvidence(ranked, many, "MX1248A", 10)
	require.Len(t, kept, 4, "three reserved exact passages and the existing ranked passage")
	require.Equal(t, "near", kept[3].ID)
	require.Equal(t, many, exactIdentifierTopK(many, "MX1248A", 0))
}

func TestExactIdentifierRetentionPreservesImageEvidenceOutsideTopK(t *testing.T) {
	results := []*types.SearchResult{
		{ID: "ranked", Content: "inventory overview", Score: .9},
		{ID: "exact", Content: "MX1248A inventory", Score: .1},
		{ID: "image", Content: "MX1248A image", Metadata: map[string]string{types.MetadataKeptBy: types.KeptByImageVector}},
	}
	kept := exactIdentifierTopK(results, "MX1248A", 2)
	require.Len(t, kept, 3)
	require.Equal(t, "exact", kept[0].ID)
	require.Equal(t, "ranked", kept[1].ID)
	require.Equal(t, "image", kept[2].ID)
}

func TestExactIdentifierRetentionCannotBypassFAQConfidence(t *testing.T) {
	ranked := []*types.SearchResult{{ID: "document", Content: "inventory overview", Score: .5}}
	faq := &types.SearchResult{ID: "faq", Content: "MX1248A unrelated question", ChunkType: string(types.ChunkTypeFAQ), Score: 1}
	require.Equal(t, ranked, retainExactIdentifierEvidence(ranked, []*types.SearchResult{faq}, "MX1248A", 5))
}

func TestApproximateTableDedupKeepsTheOnlyExactPartNumber(t *testing.T) {
	// One cell differs in otherwise nearly identical, sufficiently large tables.
	var table strings.Builder
	for i := 0; i < 100; i++ {
		fmt.Fprintf(&table, " field%03d", i)
	}
	boilerplate := table.String()
	results := []*types.SearchResult{
		{ID: "near", Content: boilerplate + " MX1248B", Score: .9},
		{ID: "exact", Content: boilerplate + " MX1248A", Score: .1},
	}
	require.Len(t, removePartialOverlaps(context.Background(), results), 1, "exercise an actual approximate-overlap collision")
	kept := removePartialOverlaps(context.Background(), results, "MX1248A")
	require.Len(t, kept, 2)
}
