package repository

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Tencent/WeKnora/internal/types"
	"github.com/stretchr/testify/require"
)

func TestNASInitialPublicationFencesDuplicateSourceAfterLeaseLoss(t *testing.T) {
	db, repo, old, first := replacementFixture(t)
	ctx := context.Background()
	require.NoError(t, repo.DeleteKnowledge(ctx, old.TenantID, old.ID))
	// AutoMigrate supplied the new fields; exercise the actual shipped unique
	// indexes rather than a test-only recreation of their semantics.
	migration, err := os.ReadFile("../../../migrations/sqlite/000052_knowledge_staging.up.sql")
	require.NoError(t, err)
	indexes := strings.SplitN(string(migration), "CREATE UNIQUE INDEX", 2)
	require.Len(t, indexes, 2)
	require.NoError(t, db.Exec("CREATE UNIQUE INDEX"+indexes[1]).Error)
	now := time.Now()
	first.Channel, first.IsStaged = types.ConnectorTypeDokuWikiFilesystem, true
	first.StorageSize = 10
	first.ParseStatus, first.EnableStatus, first.ProcessedAt = types.ParseStatusCompleted, "enabled", &now
	second := *first
	second.ID = "concurrent-initial"
	require.NoError(t, repo.CreateKnowledge(ctx, first))
	require.NoError(t, repo.CreateKnowledge(ctx, &second))
	staging := repo.(interface {
		PublishStagedKnowledge(context.Context, *types.Knowledge, *types.Knowledge) error
	})
	require.NoError(t, staging.PublishStagedKnowledge(ctx, nil, first))
	require.Error(t, staging.PublishStagedKnowledge(ctx, nil, &second))
	row, err := repo.GetKnowledgeByID(ctx, second.TenantID, second.ID)
	require.NoError(t, err)
	require.True(t, row.IsStaged)
	// Same external ID in a different datasource is intentionally independent.
	third := second
	third.ID = "different-source"
	third.Metadata = types.JSON(`{"datasource_id":"ds-2","external_id":"doc-1"}`)
	require.NoError(t, repo.CreateKnowledge(ctx, &third))
	require.NoError(t, staging.PublishStagedKnowledge(ctx, nil, &third))
}

func TestStagedKnowledgeIsHiddenUntilIndexedPublication(t *testing.T) {
	db, repo, old, candidate := replacementFixture(t)
	ctx := context.Background()
	candidate.IsStaged = true
	candidate.StorageSize = 10
	candidate.ReplacesKnowledgeID = &old.ID
	require.NoError(t, repo.CreateKnowledge(ctx, candidate))
	staging := repo.(interface {
		PublishStagedKnowledge(context.Context, *types.Knowledge, *types.Knowledge) error
	})
	// A candidate can be read by the trusted parse worker, but never by the
	// public batch/list/search-target path, even if its exact ID is supplied.
	workerRow, err := repo.GetKnowledgeByID(ctx, old.TenantID, candidate.ID)
	require.NoError(t, err)
	require.True(t, workerRow.IsStaged)
	visible, err := repo.GetKnowledgeBatch(ctx, old.TenantID, []string{old.ID, candidate.ID})
	require.NoError(t, err)
	require.Len(t, visible, 1)
	require.Equal(t, old.ID, visible[0].ID)
	listed, err := repo.ListKnowledgeByKnowledgeBaseID(ctx, old.TenantID, old.KnowledgeBaseID)
	require.NoError(t, err)
	require.Len(t, listed, 1)
	head, err := repo.FindByDataSourceExternalID(ctx, old.TenantID, old.KnowledgeBaseID, "ds-1", "doc-1")
	require.NoError(t, err)
	require.Equal(t, old.ID, head.ID)
	require.Error(t, staging.PublishStagedKnowledge(ctx, old, candidate))
	_, err = repo.GetKnowledgeByID(ctx, old.TenantID, old.ID)
	require.NoError(t, err, "pending/failed replacements must preserve the published predecessor")

	now := time.Now()
	candidate.ParseStatus, candidate.EnableStatus, candidate.ProcessedAt = types.ParseStatusCompleted, "enabled", &now
	require.NoError(t, repo.UpdateKnowledge(ctx, candidate))
	require.NoError(t, staging.PublishStagedKnowledge(ctx, old, candidate))
	_, err = repo.GetKnowledgeByID(ctx, old.TenantID, old.ID)
	require.ErrorIs(t, err, ErrKnowledgeNotFound)
	visible, err = repo.GetKnowledgeBatch(ctx, old.TenantID, []string{old.ID, candidate.ID})
	require.NoError(t, err)
	require.Len(t, visible, 1)
	require.Equal(t, candidate.ID, visible[0].ID)
	require.False(t, visible[0].IsStaged)

	// An enrichment worker that loaded the staged row before commit cannot
	// restore its stale publication flag via the ordinary full-row Save.
	require.NoError(t, repo.UpdateKnowledge(ctx, candidate))
	var current types.Knowledge
	require.NoError(t, db.First(&current, "id = ?", candidate.ID).Error)
	require.False(t, current.IsStaged)
	require.NoError(t, staging.PublishStagedKnowledge(ctx, old, candidate), "commit acknowledgement retry is idempotent")
}

func TestStagedPublicationRollsBackBothSidesOnFailure(t *testing.T) {
	db, repo, old, candidate := replacementFixture(t)
	ctx := context.Background()
	now := time.Now()
	candidate.IsStaged, candidate.ReplacesKnowledgeID = true, &old.ID
	candidate.StorageSize = 10
	candidate.ParseStatus, candidate.EnableStatus, candidate.ProcessedAt = types.ParseStatusCompleted, "enabled", &now
	require.NoError(t, repo.CreateKnowledge(ctx, candidate))
	require.NoError(t, db.Exec(`CREATE TRIGGER fail_publication BEFORE UPDATE OF is_staged ON knowledges
WHEN NEW.id = 'new' AND NEW.is_staged = 0 BEGIN SELECT RAISE(FAIL, 'forced publication failure'); END`).Error)
	staging := repo.(interface {
		PublishStagedKnowledge(context.Context, *types.Knowledge, *types.Knowledge) error
	})
	require.ErrorContains(t, staging.PublishStagedKnowledge(ctx, old, candidate), "forced publication failure")
	published, err := repo.GetKnowledgeByID(ctx, old.TenantID, old.ID)
	require.NoError(t, err)
	require.False(t, published.DeletedAt.Valid)
	stored, err := repo.GetKnowledgeByID(ctx, old.TenantID, candidate.ID)
	require.NoError(t, err)
	require.True(t, stored.IsStaged)
}

func TestStagedPublicationRejectsChangedPredecessorAndForeignSource(t *testing.T) {
	for _, mutation := range []string{"predecessor", "source"} {
		t.Run(mutation, func(t *testing.T) {
			_, repo, old, candidate := replacementFixture(t)
			ctx := context.Background()
			now := time.Now()
			candidate.IsStaged, candidate.ReplacesKnowledgeID = true, &old.ID
			candidate.StorageSize = 10
			candidate.ParseStatus, candidate.EnableStatus, candidate.ProcessedAt = types.ParseStatusCompleted, "enabled", &now
			if mutation == "source" {
				candidate.Metadata = types.JSON(`{"datasource_id":"foreign","external_id":"doc-1"}`)
			}
			require.NoError(t, repo.CreateKnowledge(ctx, candidate))
			if mutation == "predecessor" {
				require.NoError(t, repo.UpdateKnowledgeColumn(ctx, old.ID, "title", "concurrent edit"))
			}
			staging := repo.(interface {
				PublishStagedKnowledge(context.Context, *types.Knowledge, *types.Knowledge) error
			})
			require.Error(t, staging.PublishStagedKnowledge(ctx, old, candidate))
			_, err := repo.GetKnowledgeByID(ctx, old.TenantID, old.ID)
			require.NoError(t, err)
		})
	}
}
