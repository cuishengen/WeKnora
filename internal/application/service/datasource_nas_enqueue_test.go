package service

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/Tencent/WeKnora/internal/types"
	"github.com/alicebob/miniredis/v2"
	"github.com/hibiken/asynq"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

type nasManualTaskQueue struct{ calls atomic.Int64 }

func (q *nasManualTaskQueue) Enqueue(*asynq.Task, ...asynq.Option) (*asynq.TaskInfo, error) {
	q.calls.Add(1)
	return &asynq.TaskInfo{ID: "nas-sync"}, nil
}

func TestNASConcurrentManualSyncReusesRunningLogAndAllowsLaterRun(t *testing.T) {
	f := newSQLiteDataSourceDeleteFixture(t)
	sqlDB, err := f.db.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(1)
	f.ds.Type = types.ConnectorTypeDokuWikiFilesystem
	require.NoError(t, f.dsRepo.Update(context.Background(), f.ds))
	require.NoError(t, f.db.Where("data_source_id = ?", f.ds.ID).Delete(&types.SyncLog{}).Error)
	mini := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mini.Addr()})
	t.Cleanup(func() { require.NoError(t, client.Close()) })
	queue := &nasManualTaskQueue{}
	s := &DataSourceService{dsRepo: f.dsRepo, syncLogRepo: f.syncLogRepo, taskEnqueuer: queue, redisClient: client}
	type response struct {
		log *types.SyncLog
		err error
	}
	results := make(chan response, 8)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			log, err := s.ManualSync(context.Background(), f.ds.ID)
			results <- response{log, err}
		}()
	}
	close(start)
	wg.Wait()
	close(results)
	var id string
	for got := range results {
		require.NoError(t, got.err)
		require.NotNil(t, got.log)
		if id == "" {
			id = got.log.ID
		}
		require.Equal(t, id, got.log.ID)
	}
	require.EqualValues(t, 1, queue.calls.Load())
	rows, err := f.syncLogRepo.FindByDataSource(context.Background(), f.ds.ID, 10, 0)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	rows[0].Status = types.SyncLogStatusSuccess
	require.NoError(t, f.syncLogRepo.UpdateResult(context.Background(), rows[0]))
	next, err := s.ManualSync(context.Background(), f.ds.ID)
	require.NoError(t, err)
	require.NotEqual(t, id, next.ID)
	require.EqualValues(t, 2, queue.calls.Load(), "completed tasks must not block later manual runs")
}
