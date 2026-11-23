package service

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/Tencent/WeKnora/internal/application/repository"
	"github.com/Tencent/WeKnora/internal/datasource"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
	"github.com/hibiken/asynq"
	"github.com/stretchr/testify/require"
)

func readyNASCandidate(t *testing.T) (*replaceFileHarness, *types.DataSource, *types.Knowledge, map[string]string) {
	t.Helper()
	t.Setenv("RETRIEVE_DRIVER", "postgres")
	h, _, _, _ := datasourceReplacementFixture(t)
	h.svc.kbService = &reparseFailureKBService{kb: &types.KnowledgeBase{
		ID: "kb-1", TenantID: 7, Type: types.KnowledgeBaseTypeDocument, IndexingStrategy: types.DefaultIndexingStrategy(),
	}}
	metadata := map[string]string{"datasource_id": "ds-1", "external_id": "doc-1", "sha256": "new-sha256"}
	encoded, err := json.Marshal(metadata)
	require.NoError(t, err)
	now := time.Now()
	candidate := &types.Knowledge{
		ID: "staged", TenantID: 7, KnowledgeBaseID: "kb-1", Type: "file", Channel: types.ConnectorTypeDokuWikiFilesystem,
		FilePath: "staged/file.md", FileName: "doc.md", FileHash: md5Hex("new"), Metadata: types.JSON(encoded),
		IsStaged: true, ReplacesKnowledgeID: &h.original.ID, ParseStatus: types.ParseStatusCompleted,
		EnableStatus: "enabled", ProcessedAt: &now, StorageSize: 10,
	}
	require.NoError(t, h.svc.repo.CreateKnowledge(h.ctx, candidate))
	return h, &types.DataSource{ID: "ds-1", TenantID: 7, KnowledgeBaseID: "kb-1", Type: types.ConnectorTypeDokuWikiFilesystem}, candidate, metadata
}

func TestNASResumeCompletedCandidateRechecksSourceBeforePublication(t *testing.T) {
	h, ds, candidate, metadata := readyNASCandidate(t)
	item := &types.FetchedItem{ExternalID: "doc-1", FileName: "doc.md", Content: []byte("new")}
	checks := 0
	verify := func(context.Context) error { checks++; return errors.New("source ACL changed") }
	_, err := h.svc.StageDataSourceKnowledge(h.ctx, ds, item, metadata, nil, ds.Type, verify)
	require.ErrorContains(t, err, "source ACL changed")
	_, err = h.svc.repo.GetKnowledgeByID(h.ctx, 7, h.original.ID)
	require.NoError(t, err)
	require.Empty(t, h.store.deleted)
	_, err = h.svc.GetKnowledgeByID(h.ctx, candidate.ID)
	require.ErrorIs(t, err, repository.ErrKnowledgeNotFound)
	_, err = h.svc.GetKnowledgeByIDOnly(h.ctx, candidate.ID)
	require.ErrorIs(t, err, repository.ErrKnowledgeNotFound)
	_, _, err = h.svc.GetKnowledgeFile(h.ctx, candidate.ID)
	require.ErrorIs(t, err, repository.ErrKnowledgeNotFound)
	// A new sync process can resume an already completed hidden candidate,
	// without relying on a parse event or creating a second processing task.
	updated, err := h.svc.StageDataSourceKnowledge(h.ctx, ds, item, metadata, nil, ds.Type,
		func(context.Context) error { checks++; return nil })
	require.NoError(t, err)
	require.True(t, updated)
	require.Equal(t, 2, checks)
	_, err = h.svc.repo.GetKnowledgeByID(h.ctx, 7, h.original.ID)
	require.ErrorIs(t, err, repository.ErrKnowledgeNotFound)
	published, err := h.svc.repo.GetKnowledgeByID(h.ctx, 7, candidate.ID)
	require.NoError(t, err)
	require.False(t, published.IsStaged)
	require.Contains(t, h.store.deleted, h.original.FilePath)
}

type immediateStagedParser struct {
	t       *testing.T
	repo    interfaces.KnowledgeRepository
	oldID   string
	created string
}

func (q *immediateStagedParser) Enqueue(task *asynq.Task, _ ...asynq.Option) (*asynq.TaskInfo, error) {
	if task.Type() == types.TypeDocumentProcess {
		var payload types.DocumentProcessPayload
		require.NoError(q.t, json.Unmarshal(task.Payload(), &payload))
		_, err := q.repo.GetKnowledgeByID(context.Background(), 7, q.oldID)
		require.NoError(q.t, err, "old publication must survive while the candidate parses")
		row, err := q.repo.GetKnowledgeByID(context.Background(), 7, payload.KnowledgeID)
		require.NoError(q.t, err)
		require.True(q.t, row.IsStaged)
		now := time.Now()
		row.ParseStatus, row.EnableStatus, row.ProcessedAt, row.StorageSize = types.ParseStatusCompleted, "enabled", &now, 10
		require.NoError(q.t, q.repo.UpdateKnowledge(context.Background(), row))
		q.created = row.ID
	}
	return &asynq.TaskInfo{ID: "parse", Queue: types.QueueDefault}, nil
}

func TestNASFailedCandidateIsRecreatedOnRetry(t *testing.T) {
	h, ds, failed, metadata := readyNASCandidate(t)
	require.NoError(t, h.svc.repo.UpdateKnowledgeColumns(h.ctx, failed.ID, map[string]interface{}{
		"parse_status": types.ParseStatusFailed, "error_message": stagedDocumentRejectionPrefix + "invalid input",
	}))
	parser := &immediateStagedParser{t: t, repo: h.svc.repo, oldID: h.original.ID}
	h.svc.task = parser
	_, err := h.svc.StageDataSourceKnowledge(h.ctx, ds,
		&types.FetchedItem{ExternalID: "doc-1", FileName: "doc.md", Content: []byte("new")}, metadata, nil, ds.Type,
		func(context.Context) error { return nil })
	require.NoError(t, err)
	require.NotEmpty(t, parser.created)
	require.NotEqual(t, failed.ID, parser.created)
	row, err := h.svc.repo.GetKnowledgeByID(h.ctx, 7, parser.created)
	require.NoError(t, err)
	require.False(t, row.IsStaged)
}

func TestNASEmptyIndexIsDocumentFailureAndKeepsOldPublication(t *testing.T) {
	h, ds, candidate, metadata := readyNASCandidate(t)
	require.NoError(t, h.svc.repo.UpdateKnowledgeColumn(h.ctx, candidate.ID, "storage_size", int64(0)))
	_, err := h.svc.StageDataSourceKnowledge(h.ctx, ds,
		&types.FetchedItem{ExternalID: "doc-1", FileName: "doc.md", Content: []byte("new")}, metadata, nil, ds.Type,
		func(context.Context) error { t.Fatal("empty content must not publish"); return nil })
	var rejected *datasource.DocumentParseFailure
	require.ErrorAs(t, err, &rejected)
	_, err = h.svc.repo.GetKnowledgeByID(h.ctx, 7, h.original.ID)
	require.NoError(t, err)
	require.Empty(t, h.store.deleted)
}
