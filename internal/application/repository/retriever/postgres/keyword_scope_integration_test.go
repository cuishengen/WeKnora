package postgres

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/Tencent/WeKnora/internal/types"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	gormpostgres "gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// Exercise the real BM25 plan: SQL mocks cannot detect score-plan regressions.
// The isolated schema and all synthetic rows are rolled back; business tables
// are never changed. Requires a disposable database with pg_search installed.
func TestKeywordExclusionPreservesScopeAndRanking(t *testing.T) {
	dsn := os.Getenv("WEKNORA_RETRIEVER_BM25_TEST_DSN")
	if dsn == "" {
		t.Skip("set WEKNORA_RETRIEVER_BM25_TEST_DSN for real BM25 validation")
	}
	db, err := gorm.Open(gormpostgres.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	require.NoError(t, err)
	pool, err := db.DB()
	require.NoError(t, err)
	defer pool.Close()
	tx := db.Begin()
	require.NoError(t, tx.Error)
	defer tx.Rollback()
	schema := "keyword_scope_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	require.NoError(t, tx.Exec("CREATE SCHEMA "+schema).Error)
	require.NoError(t, tx.Exec("SET LOCAL search_path = "+schema+", public").Error)
	require.NoError(t, tx.Exec(`CREATE TABLE embeddings (
		id bigserial PRIMARY KEY, content text, source_id text, source_type integer,
		chunk_id text, knowledge_id text, knowledge_base_id text, tag_id text, is_enabled boolean)`).Error)
	require.NoError(t, tx.Exec(`CREATE TABLE knowledges (id text PRIMARY KEY, is_staged boolean, deleted_at timestamptz)`).Error)
	require.NoError(t, tx.Exec(`CREATE INDEX scope_search_idx ON embeddings USING bm25 (id, content) WITH (key_field='id')`).Error)
	require.NoError(t, tx.Exec(`INSERT INTO knowledges VALUES ('allowed',false,NULL),('staged',true,NULL),('deleted',false,now())`).Error)
	rows := []struct {
		source, chunk, doc, kb, tag string
		enabled                     interface{}
	}{
		{"chunk-1", "chunk-1", "allowed", "kb", "tag", true},
		{"chunk-1-question", "chunk-1", "allowed", "kb", "tag", true},
		{"chunk-2", "chunk-2", "allowed", "kb", "tag", nil},
		{"wrong-kb", "wrong-kb", "allowed", "other", "tag", true},
		{"wrong-doc", "wrong-doc", "other", "kb", "tag", true},
		{"wrong-tag", "wrong-tag", "allowed", "kb", "other", true},
		{"disabled", "disabled", "allowed", "kb", "tag", false},
		{"staged", "staged", "staged", "kb", "tag", true},
		{"deleted", "deleted", "deleted", "kb", "tag", true},
	}
	for _, row := range rows {
		require.NoError(t, tx.Exec(`INSERT INTO embeddings
			(content,source_id,source_type,chunk_id,knowledge_id,knowledge_base_id,tag_id,is_enabled)
			VALUES (?,?,?,?,?,?,?,?)`, "scopeprobe technical fact", row.source, int(types.ChunkSourceType),
			row.chunk, row.doc, row.kb, row.tag, row.enabled).Error)
	}
	repo := NewPostgresRetrieveEngineRepository(tx)
	params := types.RetrieveParams{RetrieverType: types.KeywordsRetrieverType, Query: "scopeprobe", TopK: 10,
		KnowledgeBaseIDs: []string{"kb"}, KnowledgeIDs: []string{"allowed"}, TagIDs: []string{"tag"},
		ExcludeGeneratedQuestionKBIDs: []string{"kb"}}
	for _, topK := range []int{1, 10} {
		params.TopK = topK
		result, err := repo.Retrieve(context.Background(), params)
		require.NoError(t, err)
		require.Len(t, result, 1)
		require.Len(t, result[0].Results, min(topK, 2))
		for _, hit := range result[0].Results {
			require.Contains(t, []string{"chunk-1", "chunk-2"}, hit.SourceID)
			require.Equal(t, "allowed", hit.KnowledgeID)
			require.Equal(t, "kb", hit.KnowledgeBaseID)
			require.Greater(t, hit.Score, float64(0))
		}
	}
	params.KnowledgeIDs = []string{"absent"}
	result, err := repo.Retrieve(context.Background(), params)
	require.NoError(t, err)
	for _, set := range result {
		require.Empty(t, set.Results)
	}
}
