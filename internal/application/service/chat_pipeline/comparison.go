package chatpipeline

import (
	"context"
	"regexp"
	"strings"

	"github.com/Tencent/WeKnora/internal/searchutil"
	"github.com/Tencent/WeKnora/internal/types"
)

var comparisonIdentifier = regexp.MustCompile(`(?i)\b[a-z]{2,8}[0-9]{2,10}[a-z0-9_*-]*\b`)
var comparisonTechnicalTerm = regexp.MustCompile(`\b[A-Z][A-Z0-9]{1,11}\b`)

// A supplemental literal lookup preserves technical abbreviations across
// language differences between the question and the source. The full question
// remains in the primary searches and answer, including every condition.
func comparisonTerms(query string, objects []string) string {
	query, _, _ = strings.Cut(query, "，")
	seen := map[string]bool{}
	for _, object := range objects {
		seen[strings.ToUpper(object)] = true
	}
	var terms []string
	for _, term := range comparisonTechnicalTerm.FindAllString(query, -1) {
		if !seen[term] {
			seen[term] = true
			terms = append(terms, term)
		}
		if len(terms) == 3 {
			break
		}
	}
	return strings.Join(terms, " ")
}

// Keep literal product identifiers, without embedding any model names or facts.
func comparisonObjects(query string) []string {
	var result []string
	seen := map[string]bool{}
	for _, id := range comparisonIdentifier.FindAllString(query, -1) {
		key := strings.ToLower(id)
		if !seen[key] {
			result = append(result, id)
			seen[key] = true
		}
		if len(result) == 6 {
			break
		}
	}
	if len(result) < 2 {
		return nil
	}
	return result
}

func comparisonPrefix(object string) string {
	return strings.TrimRight(strings.ToLower(object), "x*")
}

// Run each subject with the original predicate and exactly the already-authorized
// targets. Filename resolution only narrows an unrestricted KB, never widens a
// selected document/tag scope or reads another tenant's sources.
func (p *PluginSearch) searchComparison(ctx context.Context, cm *types.ChatManage) []*types.SearchResult {
	objects := comparisonObjects(cm.Query)
	terms := comparisonTerms(cm.Query, objects)
	var result []*types.SearchResult
	for _, object := range objects {
		query := cm.RewriteQuery
		if query == "" {
			query = cm.Query
		}
		for _, other := range objects {
			if !strings.EqualFold(other, object) {
				query = regexp.MustCompile("(?i)"+regexp.QuoteMeta(other)).ReplaceAllString(query, " ")
			}
		}
		copyCM := *cm
		copyCM.RewriteQuery = query
		copyCM.EmbeddingTopK = min(max(6, cm.EmbeddingTopK/len(objects)), 12)
		found, err := p.searchByTargets(ctx, &copyCM)
		if err == nil {
			result = append(result, found...)
		}
		if p.knowledgeService == nil {
			continue
		}
		for _, target := range cm.SearchTargets {
			if target == nil || target.Type != types.SearchTargetTypeKnowledgeBase || len(target.TagIDs) > 0 || len(target.ScopeTagIDs) > 0 {
				continue
			}
			tenant := target.TenantID
			if tenant == 0 {
				tenant = cm.TenantID
			}
			files, _, _, err := p.knowledgeService.SearchKnowledgeForScopes(ctx,
				[]types.KnowledgeSearchScope{{TenantID: tenant, KBID: target.KnowledgeBaseID}}, comparisonPrefix(object), 0, 30, nil)
			if err != nil {
				continue
			}
			var ids []string
			for _, file := range files {
				if file.KnowledgeBaseID == target.KnowledgeBaseID && file.TenantID == tenant && file.ParseStatus == types.ParseStatusCompleted && file.SourceReference == nil {
					ids = append(ids, file.ID)
				}
			}
			if len(ids) == 0 {
				continue
			}
			// The document IDs already identify this family. Repeating its
			// wildcard name in BM25 favors directory pages over the requested fact.
			scopedQuery := regexp.MustCompile("(?i)"+regexp.QuoteMeta(object)).ReplaceAllString(query, " ")
			found, err := p.knowledgeBaseService.HybridSearch(ctx, target.KnowledgeBaseID, types.SearchParams{
				QueryText: scopedQuery, KnowledgeIDs: ids, MatchCount: copyCM.EmbeddingTopK, SkipContextEnrichment: true,
				DisableVectorMatch: cm.DisableVectorMatch, DisableKeywordsMatch: cm.DisableKeywordsMatch,
			})
			if err == nil {
				result = append(result, found...)
			}
			if terms != "" && !cm.DisableKeywordsMatch {
				literal, err := p.knowledgeBaseService.HybridSearch(ctx, target.KnowledgeBaseID, types.SearchParams{
					QueryText: terms, KnowledgeIDs: ids, MatchCount: copyCM.EmbeddingTopK,
					DisableVectorMatch: true, SkipContextEnrichment: true,
				})
				if err == nil {
					result = append(result, literal...)
				}
			}
		}
	}
	return result
}

// Reserve evidence per named subject before the final global cap. This keeps a
// high-scoring family's many pages from crowding another subject out entirely.
func balancedComparison(results []*types.SearchResult, objects []string, topK int) []*types.SearchResult {
	if len(objects) < 2 || topK <= 0 {
		return searchutil.TopKKeepingKept(results, topK)
	}
	var out []*types.SearchResult
	seen := map[string]bool{}
	add := func(item *types.SearchResult) bool {
		if item == nil || seen[item.ID] {
			return false
		}
		seen[item.ID] = true
		out = append(out, item)
		return true
	}
	perObject := max(1, min(3, topK/len(objects)))
	for _, object := range objects {
		kept := 0
		for _, item := range results {
			if len(out) >= topK || kept >= perObject {
				break
			}
			if item != nil && strings.Contains(strings.ToLower(item.KnowledgeTitle), comparisonPrefix(object)) && add(item) {
				kept++
			}
		}
	}
	for _, item := range results {
		if len(out) < topK || searchutil.IsKeptOutsideTopK(item) {
			add(item)
		}
	}
	return out
}
