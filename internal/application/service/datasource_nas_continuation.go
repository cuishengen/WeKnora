package service

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/Tencent/WeKnora/internal/logger"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/hibiken/asynq"
)

const nasSyncBatchItems = 100
const nasSyncBatchDuration = 30 * time.Minute

var errNASBatchComplete = errors.New("NAS sync batch checkpointed; continue in another task")

// A large first import must not spend its finite failure retries on normal
// progress. Hand off between documents, after the connector saved their hashes;
// retain the original source, sync log and initiator, and the two-hour task cap.
func (s *DataSourceService) enqueueNASContinuation(ctx context.Context, ds *types.DataSource,
	payload types.DataSourceSyncPayload,
) error {
	if ds.Type != types.ConnectorTypeDokuWikiFilesystem || payload.ForceFull || ds.SyncMode == types.SyncModeFull {
		return fmt.Errorf("only incremental NAS sync supports task continuation")
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	hash := sha256.Sum256(ds.LastSyncCursor)
	taskID := fmt.Sprintf("dssync:continue:%s:%x", payload.SyncLogID, hash[:12])
	_, err = s.taskEnqueuer.Enqueue(asynq.NewTask(types.TypeDataSourceSync, encoded),
		asynq.Queue(types.QueueSync), asynq.TaskID(taskID), asynq.MaxRetry(5),
		asynq.Timeout(2*time.Hour), asynq.ProcessIn(2*time.Second))
	if err != nil && !errors.Is(err, asynq.ErrTaskIDConflict) {
		return fmt.Errorf("enqueue NAS continuation: %w", err)
	}
	logger.Infof(ctx, "NAS source batch checkpointed: ds=%s syncLog=%s continuation=%s",
		ds.ID, payload.SyncLogID, taskID)
	return nil
}
