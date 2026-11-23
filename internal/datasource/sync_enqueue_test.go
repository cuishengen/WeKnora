package datasource

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/Tencent/WeKnora/internal/types"
	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

type nasRunningLogs struct{ *fakeSyncLogRepo }

func (r *nasRunningLogs) FindRunning(_ context.Context, dsID string) (*types.SyncLog, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, row := range r.logs {
		if row.DataSourceID == dsID && row.Status == types.SyncLogStatusRunning {
			copy := *row
			return &copy, nil
		}
	}
	return nil, nil
}

func TestNASConcurrentScheduleTicksCoalesceExistingRun(t *testing.T) {
	mini := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mini.Addr()})
	t.Cleanup(func() { require.NoError(t, client.Close()) })
	repo := newFakeDataSourceRepo()
	ds := &types.DataSource{ID: "nas", TenantID: 7, Type: types.ConnectorTypeDokuWikiFilesystem, Status: types.DataSourceStatusActive}
	require.NoError(t, repo.Create(context.Background(), ds))
	logs := &nasRunningLogs{newFakeSyncLogRepo()}
	queue := &fakeTaskEnqueuer{}
	s := NewScheduler(repo, logs, queue, client)
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() { defer wg.Done(); s.triggerSync(ds.ID, ds.TenantID) }()
	}
	wg.Wait()
	require.EqualValues(t, 1, queue.count.Load())
	running, err := FindRunningSync(context.Background(), logs, ds.ID)
	require.NoError(t, err)
	require.NotNil(t, running)
	// A scheduled tick also coalesces a still-running manual log.
	s.triggerSync(ds.ID, ds.TenantID)
	require.EqualValues(t, 1, queue.count.Load())
}

func TestNASDailyScheduleIsOneAMShanghaiRegardlessOfServerTimezone(t *testing.T) {
	schedule := "CRON_TZ=Asia/Shanghai 0 0 1 * * *"
	require.NoError(t, ValidateSyncSchedule(schedule))
	parsed, err := syncScheduleParser.Parse(schedule)
	require.NoError(t, err)
	start := time.Date(2026, 11, 23, 16, 59, 59, 0, time.UTC)
	require.Equal(t, time.Date(2026, 11, 23, 17, 0, 0, 0, time.UTC), parsed.Next(start))
}
