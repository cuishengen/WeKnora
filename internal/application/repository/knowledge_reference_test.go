package repository

import (
	"context"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/stretchr/testify/require"
	"testing"
)

func TestSourceReferenceRoundTripIsSeparateFromUploadMetadata(t *testing.T) {
	_, repo, _, row := replacementFixture(t)
	row.SourceReference = &types.SourceFileReference{Alias: "vendor-wiki", RelativePath: "media/sdk.zip", SHA256: "original-sha", Size: 90000000}
	require.NoError(t, repo.CreateKnowledge(context.Background(), row))
	saved, err := repo.GetKnowledgeByID(context.Background(), row.TenantID, row.ID)
	require.NoError(t, err)
	require.Equal(t, row.SourceReference, saved.SourceReference)
	row.ID = "untrusted-upload"
	row.SourceReference = nil
	row.Metadata = types.JSON(`{"source_reference":{"alias":"vendor-wiki","relative_path":"media/private.zip"}}`)
	require.NoError(t, repo.CreateKnowledge(context.Background(), row))
	saved, err = repo.GetKnowledgeByID(context.Background(), row.TenantID, row.ID)
	require.NoError(t, err)
	require.Nil(t, saved.SourceReference)
}
