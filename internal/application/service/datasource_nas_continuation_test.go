package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/Tencent/WeKnora/internal/datasource"
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
	require.Equal(t, nasSyncBatchItems, next.CompletedBatchesResult.Created)
	withoutCounts := next
	withoutCounts.CompletedBatchesResult = nil
	require.Equal(t, payload, withoutCounts, "handoff must preserve member, source and original run")
	payload = next
	require.NoError(t, run())
	require.Equal(t, nasSyncBatchItems+2, sink.calls)
	require.Len(t, queue.tasks, 1)
	require.Equal(t, types.SyncLogStatusSuccess, log.Status)
	require.Equal(t, nasSyncBatchItems+2, log.ItemsTotal)
	require.Equal(t, nasSyncBatchItems+2, log.ItemsCreated)
	final, err := log.ParseResult()
	require.NoError(t, err)
	require.Equal(t, nasSyncBatchItems+2, final.Created)
	require.NotNil(t, h.ds.LastSyncAt)
	require.Equal(t, []int{0, nasSyncBatchItems}, h.connector.starts)
}

type nasContinuationFailureStream struct {
	*ingestAckConnector
	sink *nasStagingSink
}

func (c *nasContinuationFailureStream) FetchStream(ctx context.Context, cfg *types.DataSourceConfig,
	cursor *types.SyncCursor, h datasource.StreamHandler,
) (*types.SyncCursor, error) {
	items, next, _ := c.FetchIncremental(ctx, cfg, cursor)
	offset := ingestAckOffset(cursor)
	for i, item := range items {
		c.sink.err = nil
		if item.ExternalID == "0" {
			c.sink.err = &datasource.DocumentParseFailure{KnowledgeID: "rejected", Reason: "invalid content"}
		}
		err := h.Emit(ctx, item)
		var rejected *datasource.DocumentParseFailure
		if err != nil && !errors.As(err, &rejected) {
			return nil, err
		}
		if err := h.Checkpoint(ctx, ingestAckCursor(offset+i+1)); err != nil {
			return nil, err
		}
	}
	return next, nil
}

func TestNASBatchRetainsEarlierContentFailureAsPartialAfterSuccessfulFinalBatch(t *testing.T) {
	h := newIngestAckHarness(t, true)
	h.ds.Type = types.ConnectorTypeDokuWikiFilesystem
	h.connector.items = make([]types.FetchedItem, nasSyncBatchItems+2)
	for i := range h.connector.items {
		h.connector.items[i] = types.FetchedItem{ExternalID: fmt.Sprint(i), Title: fmt.Sprintf("doc%d.txt", i),
			FileName: "doc.txt", Content: []byte("body")}
	}
	sink, queue := &nasStagingSink{}, &nasContinuationQueue{}
	h.svc.knowledgeService, h.svc.taskEnqueuer = sink, queue
	stream := &nasContinuationFailureStream{h.connector, sink}
	payload := types.DataSourceSyncPayload{DataSourceID: h.ds.ID, TenantID: h.ds.TenantID, SyncLogID: h.syncLogID}
	log := h.syncLogRepo.logs[h.syncLogID]
	cfg, err := h.ds.ParseConfig()
	require.NoError(t, err)
	require.NoError(t, h.svc.processSyncStreaming(context.Background(), stream, h.ds, log, cfg, payload, false))
	require.Equal(t, 1, log.ItemsFailed)
	require.Equal(t, nasSyncBatchItems, log.ItemsTotal)
	require.Len(t, queue.tasks, 1)
	require.NoError(t, json.Unmarshal(queue.tasks[0].Payload(), &payload))
	require.Equal(t, 1, payload.CompletedBatchesResult.Failed)
	require.NoError(t, h.svc.processSyncStreaming(context.Background(), stream, h.ds, log, cfg, payload, false))
	require.Equal(t, types.SyncLogStatusPartial, log.Status)
	require.Equal(t, nasSyncBatchItems+2, log.ItemsTotal)
	require.Equal(t, nasSyncBatchItems+1, log.ItemsCreated)
	require.Equal(t, 1, log.ItemsFailed)
	result, err := log.ParseResult()
	require.NoError(t, err)
	require.Len(t, result.Errors, 1)
	require.Equal(t, "doc0.txt", result.Errors[0].Title)
	require.Equal(t, "Ingest failed; see server logs", result.Errors[0].Message)
	require.Contains(t, log.ErrorMessage, "1 document(s) failed")
}

func TestNASCompletedBatchTotalsDoNotMutateBaseOrExceedErrorSampleCap(t *testing.T) {
	base := &types.SyncResult{Total: 150, Created: 50, Failed: 100,
		Errors: make([]types.SyncItemError, maxSyncResultErrors), NextCursor: ingestAckCursor(150)}
	batch := &types.SyncResult{Total: 2, Created: 1, Failed: 1,
		Errors: []types.SyncItemError{{Title: "last failure"}}}
	combined := mergedSyncResults(base, batch)
	require.Equal(t, 152, combined.Total)
	require.Equal(t, 101, combined.Failed)
	require.Len(t, combined.Errors, maxSyncResultErrors)
	require.Nil(t, combined.NextCursor, "the durable source cursor is not duplicated in the task payload")
	require.Equal(t, 150, base.Total)
	require.Equal(t, 2, batch.Total)
	combined.Errors[0].Title = "changed"
	require.Empty(t, base.Errors[0].Title)
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
