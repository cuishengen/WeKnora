package chatpipeline

import (
	"context"
	"maps"
	"regexp"
	"sort"
	"strings"

	"github.com/Tencent/WeKnora/internal/searchutil"
	"github.com/Tencent/WeKnora/internal/types"
)

var comparisonIdentifier = regexp.MustCompile(`(?i)\b[a-z]{2,8}[0-9]{2,10}[a-z0-9_*-]*\b`)
var comparisonTechnicalTerm = regexp.MustCompile(`\b[A-Z][A-Z0-9]{1,11}\b`)

const comparisonAttributeEvidenceKey = "comparison_attribute_evidence"

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

// The pooled family search can retrieve a secondary mention of an attribute
// from one copy while another copy supplies its value. Recheck only documents
// already seen in this request, with a fixed budget and the original scopes.
func (p *PluginSearch) searchComparisonDocumentAttributes(ctx context.Context, cm *types.ChatManage,
	results []*types.SearchResult, objects []string, terms string,
) []*types.SearchResult {
	if terms == "" || cm.DisableKeywordsMatch || len(objects) < 2 {
		return nil
	}
	var names []string
	for _, term := range strings.Fields(terms) {
		names = append(names, regexp.QuoteMeta(term))
	}
	attribute := `(?:` + strings.Join(names, "|") + `)`
	// This is candidate priority, not value extraction. Require a direct
	// quantity/attribute relationship on one line, e.g. "12-bit ADC" or
	// "RAM: 512KB"; "RAM retention, current 0.2mA" does not qualify.
	value := regexp.MustCompile(`(?i)(?:\b\d+(?:\.\d+)?[ \t-]*[a-zµμ%]{1,8}[ \t]+` + attribute +
		`\b|\b` + attribute + `[ \t:=/-]*\d+(?:\.\d+)?(?:[ \t]*[a-zµμ%]{1,8})?\b)`)
	seeds := append(append([]*types.SearchResult(nil), cm.SearchResult...), results...)
	var documents []*types.SearchResult
	seen, copies := map[string]bool{}, map[string]int{}
	for _, r := range seeds {
		if r != nil && r.ChunkType != string(types.ChunkTypeFAQ) && r.KnowledgeBaseID != "" && r.KnowledgeID != "" && !seen[r.KnowledgeBaseID+"\x00"+r.KnowledgeID] {
			seen[r.KnowledgeBaseID+"\x00"+r.KnowledgeID] = true
			documents = append(documents, r)
			copies[r.KnowledgeBaseID+"\x00"+strings.ToLower(firstPipelineTitle(r))]++
		}
	}
	sort.SliceStable(documents, func(i, j int) bool {
		return copies[documents[i].KnowledgeBaseID+"\x00"+strings.ToLower(firstPipelineTitle(documents[i]))] >
			copies[documents[j].KnowledgeBaseID+"\x00"+strings.ToLower(firstPipelineTitle(documents[j]))]
	})
	seen = map[string]bool{}
	var out []*types.SearchResult
	queries := 0
	for round := 0; round < 2; round++ {
		for _, object := range objects {
			for _, doc := range documents {
				key := doc.KnowledgeBaseID + "\x00" + doc.KnowledgeID
				if queries >= 6 || seen[key] || !strings.Contains(strings.ToLower(firstPipelineTitle(doc)), comparisonPrefix(object)) {
					continue
				}
				var scope *types.SearchTarget
				for _, target := range cm.SearchTargets {
					if target == nil || target.KnowledgeBaseID != doc.KnowledgeBaseID {
						continue
					}
					if (target.Type == types.SearchTargetTypeKnowledge && containsID(target.KnowledgeIDs, doc.KnowledgeID)) ||
						(target.Type == types.SearchTargetTypeKnowledgeBase && (len(target.ScopeTagIDs) == 0 || len(target.TagIDs) > 0)) {
						scope = target
						break
					}
				}
				if scope == nil {
					continue
				}
				seen[key], queries = true, queries+1
				_, threshold := scope.RecallThresholds(cm.VectorThreshold, cm.KeywordThreshold)
				found, err := p.knowledgeBaseService.HybridSearch(ctx, scope.KnowledgeBaseID, types.SearchParams{
					QueryText: terms, KnowledgeIDs: []string{doc.KnowledgeID}, TagIDs: scope.TagIDs, ScopeTagIDs: scope.ScopeTagIDs,
					KeywordThreshold: threshold, MatchCount: 4, DisableVectorMatch: true, SkipContextEnrichment: true,
				})
				if err == nil {
					for _, hit := range found {
						if hit == nil || hit.ChunkType == string(types.ChunkTypeFAQ) || hit.KnowledgeBaseID != doc.KnowledgeBaseID || hit.KnowledgeID != doc.KnowledgeID || !value.MatchString(hit.Content) {
							continue
						}
						hit.Metadata = maps.Clone(hit.Metadata)
						if hit.Metadata == nil {
							hit.Metadata = map[string]string{}
						}
						hit.Metadata[comparisonAttributeEvidenceKey] = "true"
						for _, seed := range seeds {
							if seed != nil && seed.ID == hit.ID && seed.KnowledgeBaseID == hit.KnowledgeBaseID && seed.KnowledgeID == hit.KnowledgeID {
								seed.Metadata = hit.Metadata
							}
						}
						out = append(out, hit)
						break
					}
				}
				break
			}
		}
	}
	return out
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
	return append(result, p.searchComparisonDocumentAttributes(ctx, cm, result, objects, terms)...)
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
		documents := map[string]bool{}
		for _, item := range results {
			if item != nil && len(out) < topK && kept < perObject && item.Metadata[comparisonAttributeEvidenceKey] == "true" &&
				strings.Contains(strings.ToLower(firstPipelineTitle(item)), comparisonPrefix(object)) &&
				!documents[item.KnowledgeBaseID+"\x00"+item.KnowledgeID] && add(item) {
				documents[item.KnowledgeBaseID+"\x00"+item.KnowledgeID] = true
				kept++
			}
		}
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
