package service

import (
	"context"
	"errors"
	"testing"

	"github.com/Tencent/WeKnora/internal/datasource"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
	"github.com/stretchr/testify/require"
)

type nasStagingSink struct {
	interfaces.KnowledgeService
	err   error
	calls int
}

func (s *nasStagingSink) StageDataSourceKnowledge(context.Context, *types.DataSource, *types.FetchedItem,
	map[string]string, []string, string, func(context.Context) error,
) (bool, error) {
	s.calls++
	return false, s.err
}

func TestNASOnlyTypedContentFailureAllowsFollowingDocument(t *testing.T) {
	ds := &types.DataSource{ID: "nas", Type: types.ConnectorTypeDokuWikiFilesystem}
	item := types.FetchedItem{ExternalID: "doc", Content: []byte("body"), FileName: "doc.txt"}
	t.Run("content rejection", func(t *testing.T) {
		sink := &nasStagingSink{err: &datasource.DocumentParseFailure{KnowledgeID: "candidate", Reason: "malformed"}}
		result := &types.SyncResult{}
		h := &streamSyncHandler{svc: &DataSourceService{knowledgeService: sink}, ds: ds, result: result}
		var rejected *datasource.DocumentParseFailure
		require.ErrorAs(t, h.Emit(context.Background(), item), &rejected)
		require.Nil(t, h.ingestErr, "connector can record the failure and continue without acknowledging its hash")
		sink.err = nil
		require.NoError(t, h.Emit(context.Background(), item))
		require.Equal(t, 2, sink.calls)
		require.Equal(t, 1, result.Failed)
		require.Equal(t, 1, result.Created)
		require.Equal(t, 1, h.nonBlockingFailures)
	})
	for _, err := range []error{errors.New("quota exhausted"), errors.New("ACL changed"),
		errors.New("enqueue failed"), errors.New("database unavailable"), ErrInvalidFileType, context.Canceled} {
		t.Run(err.Error(), func(t *testing.T) {
			sink := &nasStagingSink{err: err}
			h := &streamSyncHandler{svc: &DataSourceService{knowledgeService: sink}, ds: ds, result: &types.SyncResult{}}
			require.ErrorIs(t, h.Emit(context.Background(), item), err)
			require.ErrorIs(t, h.Emit(context.Background(), item), err)
			require.Equal(t, 1, sink.calls, "operational errors must abort before the next source item")
		})
	}
}
