package chatpipeline

import (
	"context"
	"regexp"
	"strings"
	"sync"

	"github.com/Tencent/WeKnora/internal/searchutil"
	"github.com/Tencent/WeKnora/internal/types"
)

const maxExactIdentifierEvidence = 3

// A full model/part number needs a literal retrieval path as well as semantic
// retrieval. Family wildcards and multi-model comparisons use comparison.go.
func singleExactIdentifier(cm *types.ChatManage) string {
	query := cm.Query
	ids := comparisonIdentifier.FindAllStringIndex(query, -1)
	if len(ids) == 0 {
		query = cm.RewriteQuery
		ids = comparisonIdentifier.FindAllStringIndex(query, -1)
	}
	var identifier string
	for _, bounds := range ids {
		id := query[bounds[0]:bounds[1]]
		if bounds[1] < len(query) && query[bounds[1]] == '*' {
			return ""
		}
		if identifier != "" && !strings.EqualFold(identifier, id) {
			return ""
		}
		identifier = id
	}
	if strings.HasSuffix(strings.ToLower(identifier), "x") || strings.Contains(identifier, "*") {
		return ""
	}
	return identifier
}

// Identifier boundaries include suffix separators: AB1234A must not stand in
// for AB1234A1, AB1234A-B or AB1234A_B. Chinese prose is a valid delimiter.
func exactIdentifierMatcher(identifier string) *regexp.Regexp {
	if identifier == "" {
		return nil
	}
	return regexp.MustCompile(`(?i)(^|[^a-z0-9_*-])` + regexp.QuoteMeta(identifier) + `($|[^a-z0-9_*-])`)
}

// Use precisely the same authorized targets and search service as the primary
// query. No filename lookup, document enumeration, or unscoped fallback runs.
func (p *PluginSearch) searchExactIdentifier(ctx context.Context, cm *types.ChatManage) []*types.SearchResult {
	identifier := singleExactIdentifier(cm)
	if identifier == "" || cm.DisableKeywordsMatch || len(cm.SearchTargets) == 0 {
		return nil
	}
	copyCM := *cm
	copyCM.RewriteQuery = identifier
	copyCM.DisableVectorMatch = true
	copyCM.KeywordThreshold = 0
	copyCM.EmbeddingTopK = 30
	var results []*types.SearchResult
	var mu sync.Mutex
	for _, target := range cm.SearchTargets {
		if target == nil || target.KnowledgeBaseID == "" ||
			(target.Type != types.SearchTargetTypeKnowledgeBase && target.Type != types.SearchTargetTypeKnowledge) {
			continue
		}
		// Per-target retrieval also preserves logical tag scopes instead of
		// grouping them into an unrestricted multi-KB keyword search.
		if err := p.searchSingleTarget(ctx, &copyCM, target, identifier, nil, true, &mu, &results); err != nil {
			pipelineWarn(ctx, "Search", "exact_identifier_lookup", map[string]interface{}{"error": err.Error()})
		}
	}
	match := exactIdentifierMatcher(identifier)
	var exact []*types.SearchResult
	for _, r := range results {
		// Titles and generated questions cannot prove that a specific row or
		// passage actually contains the requested part number.
		// FAQ confidence can trigger a direct answer. A part-number-only
		// keyword score must not bypass its full-question confidence check.
		if r != nil && r.ChunkType != string(types.ChunkTypeFAQ) && match.MatchString(r.Content) {
			exact = append(exact, r)
		}
	}
	return exact
}

// Reserve a small part of the existing context budget for complete identifiers.
// This does not turn a literal match into an answer: predicates, dates, units,
// and missing fields still have to be supported by the retained source text.
func exactIdentifierTopK(results []*types.SearchResult, identifier string, topK int) []*types.SearchResult {
	if topK <= 0 {
		return results
	}
	match := exactIdentifierMatcher(identifier)
	if match == nil {
		return searchutil.TopKKeepingKept(results, topK)
	}
	var out []*types.SearchResult
	seen := make(map[string]bool)
	add := func(r *types.SearchResult) {
		if r != nil && !seen[r.ID] {
			seen[r.ID] = true
			out = append(out, r)
		}
	}
	for _, r := range results {
		if len(out) >= min(maxExactIdentifierEvidence, topK) {
			break
		}
		if r != nil && !searchutil.IsKeptOutsideTopK(r) && match.MatchString(r.Content) {
			add(r)
		}
	}
	for _, r := range results {
		if !searchutil.IsKeptOutsideTopK(r) && len(out) < topK {
			add(r)
		}
	}
	for _, r := range results {
		if searchutil.IsKeptOutsideTopK(r) {
			add(r)
		}
	}
	return out
}

// Reranking can discard literal evidence at its candidate cap, threshold or MMR
// cut. Reconsider at most three original exact passages before the pipeline's
// context cut; ranked copies win over their original retrieval copies.
func retainExactIdentifierEvidence(ranked, candidates []*types.SearchResult, identifier string, topK int) []*types.SearchResult {
	match := exactIdentifierMatcher(identifier)
	if match == nil {
		return ranked
	}
	pool := append([]*types.SearchResult(nil), ranked...)
	seen := make(map[string]bool)
	exactCount := 0
	for _, r := range ranked {
		if r != nil {
			seen[r.ID] = true
			if match.MatchString(r.Content) {
				exactCount++
			}
		}
	}
	for _, r := range candidates {
		if exactCount >= maxExactIdentifierEvidence {
			break
		}
		if r != nil && r.ChunkType != string(types.ChunkTypeFAQ) && !seen[r.ID] && match.MatchString(r.Content) {
			pool = append(pool, r)
			seen[r.ID] = true
			exactCount++
		}
	}
	return exactIdentifierTopK(pool, identifier, topK)
}
