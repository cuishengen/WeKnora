package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/Tencent/WeKnora/internal/types"
	"github.com/hibiken/asynq"
	"github.com/stretchr/testify/require"
)

type nasContinuationQueue struct {
	tasks []*asynq.Task
	opts  [][]asynq.Option
	err   error
}

func (q *nasContinuationQueue) Enqueue(task *asynq.Task, opts ...asynq.Option) (*asynq.TaskInfo, error) {
	q.tasks = append(q.tasks, task)
	q.opts = append(q.opts, opts)
	return &asynq.TaskInfo{ID: "continued"}, q.err
}

func TestNASBatchHandsOffAndFinishesFromSavedCheckpoint(t *testing.T) {
	h := newIngestAckHarness(t, true)
	h.ds.Type = types.ConnectorTypeDokuWikiFilesystem
	h.ds.SyncMode = types.SyncModeIncremental
	h.connector.items = make([]types.FetchedItem, nasSyncBatchItems+2)
	for i := range h.connector.items {
		h.connector.items[i] = types.FetchedItem{ExternalID: fmt.Sprint(i), FileName: "doc.txt", Content: []byte("body")}
	}
	sink := &nasStagingSink{}
	queue := &nasContinuationQueue{}
	h.svc.knowledgeService, h.svc.taskEnqueuer = sink, queue
	payload := types.DataSourceSyncPayload{DataSourceID: h.ds.ID, TenantID: h.ds.TenantID, SyncLogID: h.syncLogID, Trigger: "manual"}
	log := h.syncLogRepo.logs[h.syncLogID]
	cfg, err := h.ds.ParseConfig()
	require.NoError(t, err)
	run := func() error {
		return h.svc.processSyncStreaming(context.Background(), &ingestAckStream{h.connector}, h.ds, log, cfg, payload, false)
	}
	require.NoError(t, run())
	require.Equal(t, nasSyncBatchItems, sink.calls)
	require.Equal(t, types.SyncLogStatusRunning, log.Status)
	require.Nil(t, log.FinishedAt)
	require.Nil(t, h.ds.LastSyncAt)
	cursor, err := h.ds.ParseSyncCursor()
	require.NoError(t, err)
	require.Equal(t, nasSyncBatchItems, ingestAckOffset(cursor))
	require.Len(t, queue.tasks, 1)
	var next types.DataSourceSyncPayload
	require.NoError(t, json.Unmarshal(queue.tasks[0].Payload(), &next))
	require.Equal(t, payload, next, "handoff must preserve member, source and original run")
	require.NoError(t, run())
	require.Equal(t, nasSyncBatchItems+2, sink.calls)
	require.Len(t, queue.tasks, 1)
	require.Equal(t, types.SyncLogStatusSuccess, log.Status)
	require.NotNil(t, h.ds.LastSyncAt)
	require.Equal(t, []int{0, nasSyncBatchItems}, h.connector.starts)
}

func TestNASBatchDeadlineWaitsForProgressAndPreservesOperationalErrors(t *testing.T) {
	h := &streamSyncHandler{svc: &DataSourceService{}, ds: &types.DataSource{Type: types.ConnectorTypeDokuWikiFilesystem},
		result: &types.SyncResult{Total: 200, Failed: 200}, yieldAt: time.Now().Add(-time.Second)}
	// Unacknowledged rejected files alone must not cause a continuation loop
	// over the same prefix; a following document must still be attempted.
	err := h.Emit(context.Background(), types.FetchedItem{})
	require.NoError(t, err)
	h.result.Created = 1
	require.ErrorIs(t, h.Emit(context.Background(), types.FetchedItem{}), errNASBatchComplete)
	require.Nil(t, h.ingestErr)
	h.ingestErr = errors.New("database unavailable")
	require.ErrorIs(t, h.Emit(context.Background(), types.FetchedItem{}), h.ingestErr)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	h.ingestErr = nil
	require.ErrorIs(t, h.Emit(ctx, types.FetchedItem{}), context.Canceled)
}

func TestNASContinuationIsIdempotentButEnqueueFailureStopsRun(t *testing.T) {
	queue := &nasContinuationQueue{}
	svc := &DataSourceService{taskEnqueuer: queue}
	ds := &types.DataSource{ID: "source", Type: types.ConnectorTypeDokuWikiFilesystem, LastSyncCursor: types.JSON(`{"files":1}`)}
	payload := types.DataSourceSyncPayload{SyncLogID: "run"}
	require.NoError(t, svc.enqueueNASContinuation(context.Background(), ds, payload))
	queue.err = asynq.ErrTaskIDConflict
	require.NoError(t, svc.enqueueNASContinuation(context.Background(), ds, payload))
	require.Equal(t, fmt.Sprint(queue.opts[0]), fmt.Sprint(queue.opts[1]))
	queue.err = errors.New("Redis offline")
	require.ErrorContains(t, svc.enqueueNASContinuation(context.Background(), ds, payload), "Redis offline")
	payload.ForceFull = true
	require.ErrorContains(t, svc.enqueueNASContinuation(context.Background(), ds, payload), "only incremental")
}
