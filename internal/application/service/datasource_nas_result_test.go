package service

import (
	"fmt"
	"testing"

	"github.com/Tencent/WeKnora/internal/types"
	"github.com/stretchr/testify/require"
)

func TestNASResumedResultReportsUnresolvedDocumentsInsteadOfAttempts(t *testing.T) {
	ds := &types.DataSource{Type: types.ConnectorTypeDokuWikiFilesystem}
	for _, attempts := range []int{0, 1, 20} {
		result := &types.SyncResult{Created: 201, Failed: attempts, Total: 201 + attempts}
		cursor := &types.SyncCursor{ConnectorCursor: map[string]interface{}{
			"failed": map[string]string{"media/board.pdf": "rejected"},
		}}
		reconcileNASFailures(ds, cursor, result)
		require.Equal(t, 1, result.Failed)
		require.Equal(t, 202, result.Total)
		require.Len(t, result.Errors, 1)
		require.Equal(t, "board.pdf", result.Errors[0].Title)
	}
	failed := map[string]string{}
	for i := 0; i < 150; i++ {
		failed[fmt.Sprintf("media/%d.pdf", i)] = "rejected"
	}
	result := &types.SyncResult{}
	reconcileNASFailures(ds, &types.SyncCursor{ConnectorCursor: map[string]interface{}{"failed": failed}}, result)
	require.Equal(t, 150, result.Failed)
	require.Len(t, result.Errors, maxSyncResultErrors)
	reconcileNASFailures(ds, &types.SyncCursor{ConnectorCursor: map[string]interface{}{"failed": map[string]string{}}}, result)
	require.Zero(t, result.Failed)
	require.Empty(t, result.Errors)
}

func TestNASFailureReconciliationLeavesUnrelatedAndLegacyCursorsAlone(t *testing.T) {
	for _, kind := range []string{types.ConnectorTypeDokuWikiFilesystem, "feishu"} {
		result := &types.SyncResult{Total: 3, Created: 2, Failed: 1}
		reconcileNASFailures(&types.DataSource{Type: kind}, &types.SyncCursor{}, result)
		require.Equal(t, 1, result.Failed)
		require.Equal(t, 3, result.Total)
	}
}
