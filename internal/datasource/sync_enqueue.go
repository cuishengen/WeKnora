package datasource

import (
	"context"
	"fmt"
	"time"

	"github.com/Tencent/WeKnora/internal/common/redislock"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
	"github.com/redis/go-redis/v9"
)

// WithSourceEnqueueLock coalesces manual and scheduled NAS requests before
// creating a log/task. It is separate from the long-lived execution lock.
func WithSourceEnqueueLock(ctx context.Context, client *redis.Client, ds *types.DataSource, fn func(context.Context) error) error {
	if ds.Type != types.ConnectorTypeDokuWikiFilesystem {
		return fn(ctx)
	}
	if client == nil {
		return fmt.Errorf("NAS source enqueue requires Redis")
	}
	return redislock.WithRenewableLock(ctx, client,
		fmt.Sprintf("weknora:datasource-enqueue:%d:%s", ds.TenantID, ds.ID),
		30*time.Second, 10*time.Second, fn)
}

// FindRunningSync returns the existing run so repeated controls can open the
// same progress log. The optional query keeps the public repository API stable.
func FindRunningSync(ctx context.Context, repo interfaces.SyncLogRepository, dsID string) (*types.SyncLog, error) {
	if finder, ok := repo.(interface {
		FindRunning(context.Context, string) (*types.SyncLog, error)
	}); ok {
		return finder.FindRunning(ctx, dsID)
	}
	running, err := repo.HasRunningSync(ctx, dsID)
	if err != nil || !running {
		return nil, err
	}
	rows, err := repo.FindByDataSource(ctx, dsID, 100, 0)
	if err != nil {
		return nil, err
	}
	for _, row := range rows {
		if row.Status == types.SyncLogStatusRunning {
			return row, nil
		}
	}
	return nil, fmt.Errorf("source already has a running sync")
}
