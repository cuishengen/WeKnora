package dokuwiki

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/Tencent/WeKnora/internal/datasource"
	"github.com/Tencent/WeKnora/internal/types"
)

type runBatchHandler struct {
	recordingHandler
	limit int
	stop  error
}

func (h *runBatchHandler) Checkpoint(ctx context.Context, cursor *types.SyncCursor) error {
	if err := h.recordingHandler.Checkpoint(ctx, cursor); err != nil {
		return err
	}
	if h.limit > 0 && len(h.items) >= h.limit {
		return h.stop
	}
	return nil
}

func TestContentFailureAttemptedOncePerRunAcrossBatches(t *testing.T) {
	c, cfg, root := fixture(t)
	// The fixture has two supported documents; add 199 successful documents
	// and a rejected PDF before them in processing order: 202 candidates total.
	for i := 0; i < 199; i++ {
		put(t, root, fmt.Sprintf("pages/release/batch/%03d.txt", i),
			[]byte(fmt.Sprintf("Specification for batch item %d.", i)))
	}
	const failedPath = "media/release/chips/rejected_datasheet.pdf"
	put(t, root, failedPath, []byte("first rejected PDF version"))
	ctx := datasource.WithSyncRunID(context.Background(), "run-one")
	stop := errors.New("successful batch boundary")
	var cursor *types.SyncCursor
	rejected, successful, batches := 0, 0, 0
	reject := func(item types.FetchedItem) error {
		if item.Metadata["relative_path"] == failedPath {
			rejected++
			return &datasource.DocumentParseFailure{KnowledgeID: "candidate", Reason: "invalid content"}
		}
		return nil
	}
	for {
		batches++
		if batches > 3 {
			t.Fatal("source did not converge after three batches")
		}
		h := &runBatchHandler{recordingHandler: recordingHandler{emitError: reject}, limit: 100, stop: stop}
		next, err := c.FetchStream(ctx, cfg, cursor, h)
		successful += len(h.items)
		if err == nil {
			cursor = next
			break
		}
		if !errors.Is(err, stop) || len(h.checkpoints) == 0 {
			t.Fatalf("unexpected batch failure: %v", err)
		}
		cursor = h.checkpoints[len(h.checkpoints)-1]
	}
	if batches != 3 || successful != 201 || rejected != 1 {
		t.Fatalf("batch counts: batches=%d successful=%d rejected=%d", batches, successful, rejected)
	}
	if cursor.ConnectorCursor["scan_complete"] != true ||
		len(cursor.ConnectorCursor["files"].(map[string]interface{})) != 201 ||
		len(cursor.ConnectorCursor["failed"].(map[string]interface{})) != 1 {
		t.Fatal("final cursor lost progress or acknowledged rejected content")
	}
	if _, exists := cursor.ConnectorCursor["files"].(map[string]interface{})[failedPath]; exists {
		t.Fatal("rejected PDF was acknowledged")
	}
	// A new manual/scheduled sync retries only the one rejected document.
	nextRun := datasource.WithSyncRunID(context.Background(), "run-two")
	h := &recordingHandler{emitError: reject}
	cursor, err := c.FetchStream(nextRun, cfg, cursor, h)
	if err != nil || rejected != 2 || len(h.items) != 0 {
		t.Fatalf("new run did not retry only the failure: rejected=%d items=%d err=%v", rejected, len(h.items), err)
	}
	// Repeating that run does not attempt the same bytes again.
	h = &recordingHandler{emitError: reject}
	cursor, err = c.FetchStream(nextRun, cfg, cursor, h)
	if err != nil || rejected != 2 || len(h.items) != 0 {
		t.Fatal("same run reattempted rejected bytes")
	}
	put(t, root, failedPath, []byte("changed rejected PDF version"))
	cursor, err = c.FetchStream(nextRun, cfg, cursor, &recordingHandler{emitError: reject})
	if err != nil || rejected != 3 {
		t.Fatal("changed bytes were not retried in the same run")
	}
	// Recovery acknowledges only after successful ingestion and clears both
	// the failure reason and its per-run attempt record.
	h = &recordingHandler{}
	cursor, err = c.FetchStream(datasource.WithSyncRunID(context.Background(), "run-three"), cfg, cursor, h)
	if err != nil || len(h.items) != 1 || h.items[0].Metadata["relative_path"] != failedPath ||
		len(cursor.ConnectorCursor["failed"].(map[string]interface{})) != 0 ||
		cursor.ConnectorCursor["failed_attempts"] != nil {
		t.Fatalf("successful recovery retained failure state: %v", err)
	}
}

func TestFailureRunKeepsOldPublicationAndRetryWithoutRunID(t *testing.T) {
	c, cfg, root := fixture(t)
	_, baseline, err := c.FetchIncremental(context.Background(), cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	const failedPath = "pages/release/chips/spec.txt"
	oldHash := baseline.ConnectorCursor["files"].(map[string]interface{})[failedPath]
	put(t, root, failedPath, []byte("Changed specification with a rejected body."))
	rejected := 0
	reject := func(item types.FetchedItem) error {
		rejected++
		return &datasource.DocumentParseFailure{Reason: "rejected update"}
	}
	ctx := datasource.WithSyncRunID(context.Background(), "same-run")
	cursor, err := c.FetchStream(ctx, cfg, baseline, &recordingHandler{emitError: reject})
	if err != nil || rejected != 1 || cursor.ConnectorCursor["files"].(map[string]interface{})[failedPath] != oldHash {
		t.Fatal("rejected update changed the published hash")
	}
	// Legacy callers have no persistent run identity and must not inherit a
	// previous run's suppression just because failed_attempts is present.
	cursor, err = c.FetchStream(context.Background(), cfg, cursor, &recordingHandler{emitError: reject})
	if err != nil || rejected != 2 || cursor.ConnectorCursor["failed_attempts"] != nil {
		t.Fatal("legacy caller did not retry content failure")
	}
	_, err = c.FetchStream(context.Background(), cfg, cursor, &recordingHandler{emitError: reject})
	if err != nil || rejected != 3 {
		t.Fatal("legacy caller stopped retrying on a later scan")
	}
}

func TestDeferredContentPreservesPublicationAndWaitsForCapability(t *testing.T) {
	c, cfg, root := fixture(t)
	_, baseline, err := c.FetchIncremental(context.Background(), cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	const deferredPath = "media/release/chips/manual.pdf"
	oldHash := baseline.ConnectorCursor["files"].(map[string]interface{})[deferredPath]
	put(t, root, deferredPath, []byte("changed image-only PDF"))
	ctx := datasource.WithSyncRunID(context.Background(), "pending-run")
	failed := &recordingHandler{emitError: func(item types.FetchedItem) error {
		return &datasource.DocumentParseFailure{Reason: "old empty-content rejection"}
	}}
	cursor, err := c.FetchStream(ctx, cfg, baseline, failed)
	if err != nil {
		t.Fatal(err)
	}
	deferCount := 0
	deferItem := func(item types.FetchedItem) error {
		if item.Metadata["relative_path"] != deferredPath {
			return nil
		}
		deferCount++
		return &datasource.DocumentParseDeferred{Reason: "image recognition disabled"}
	}
	ctx = datasource.WithSyncRunID(context.Background(), "fixed-parser-run")
	cursor, err = c.FetchStream(ctx, cfg, cursor, &recordingHandler{emitError: deferItem})
	if err != nil || deferCount != 1 || cursor.ConnectorCursor["files"].(map[string]interface{})[deferredPath] != oldHash ||
		len(cursor.ConnectorCursor["failed"].(map[string]interface{})) != 0 ||
		cursor.ConnectorCursor["failed_attempts"] != nil {
		t.Fatalf("deferred update did not preserve publication and clear old failure: %v", err)
	}
	entry := cursor.ConnectorCursor["deferred"].(map[string]interface{})[deferredPath].(map[string]interface{})
	if entry["sha256"] != digest([]byte("changed image-only PDF")) || entry["reason"] != "image recognition disabled" {
		t.Fatal("deferred item lost its content identity or reason")
	}
	// A new manual sync, with the same disabled policy, does no ingestion.
	h := &recordingHandler{emitError: deferItem}
	cursor, err = c.FetchStream(datasource.WithSyncRunID(context.Background(), "next-run"), cfg, cursor, h)
	if err != nil || deferCount != 1 || len(h.items) != 0 {
		t.Fatal("unchanged deferred file created another candidate")
	}
	// An explicit full scan permits a fresh attempt but cannot acknowledge a
	// deferred response as parsed content.
	h = &recordingHandler{emitError: deferItem}
	cursor, err = c.FetchFullStream(ctx, cfg, cursor, h)
	if err != nil || deferCount < 2 || cursor.ConnectorCursor["files"].(map[string]interface{})[deferredPath] != oldHash {
		t.Fatal("full scan skipped the deferred item or acknowledged it")
	}
	// Full mode tries other files too; restore their successful handling and
	// test that changed bytes become eligible even while vision remains off.
	put(t, root, deferredPath, []byte("new image-only PDF version"))
	h = &recordingHandler{emitError: deferItem}
	before := deferCount
	cursor, err = c.FetchStream(ctx, cfg, cursor, h)
	if err != nil || deferCount != before+1 {
		t.Fatal("changed deferred version was not retried")
	}
	// Enabling vision makes the unchanged pending bytes eligible. Only a
	// successful Emit may replace the old acknowledged hash.
	h = &recordingHandler{}
	cursor, err = c.FetchStream(datasource.WithVisionEnabled(ctx, true), cfg, cursor, h)
	if err != nil || len(h.items) != 1 || h.items[0].Metadata["relative_path"] != deferredPath ||
		cursor.ConnectorCursor["deferred"] != nil ||
		cursor.ConnectorCursor["files"].(map[string]interface{})[deferredPath] != digest([]byte("new image-only PDF version")) {
		t.Fatalf("enabled vision did not permit successful recovery: %v", err)
	}
}
