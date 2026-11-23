package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Tencent/WeKnora/internal/datasource"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/stretchr/testify/require"
)

type nasSettingsConnector struct {
	datasource.Connector
	calls  int
	config *types.DataSourceConfig
	err    error
}

func (*nasSettingsConnector) Type() string { return types.ConnectorTypeDokuWikiFilesystem }
func (c *nasSettingsConnector) Validate(_ context.Context, config *types.DataSourceConfig) error {
	c.calls++
	c.config = config
	return c.err
}

func nasSettingsFixture(t *testing.T) (*DataSourceService, *sqliteDataSourceDeleteFixture, *nasSettingsConnector) {
	t.Helper()
	f := newSQLiteDataSourceDeleteFixture(t)
	f.ds.Type = types.ConnectorTypeDokuWikiFilesystem
	f.ds.SyncDeletions = false
	f.ds.Config = types.JSON(`{"type":"dokuwiki_filesystem","settings":{"source_alias":"vendor-wiki"},"resource_ids":["namespace:release"]}`)
	require.NoError(t, f.dsRepo.Update(context.Background(), f.ds))
	connector := &nasSettingsConnector{}
	registry := datasource.NewConnectorRegistry()
	require.NoError(t, registry.Register(connector))
	return &DataSourceService{dsRepo: f.dsRepo, scheduler: f.scheduler, connectorRegistry: registry}, f, connector
}

func TestNASPatchScheduleWithoutConfigPreservesSelectionAndWorkerState(t *testing.T) {
	svc, f, connector := nasSettingsFixture(t)
	ctx := context.Background()
	before, err := f.dsRepo.FindByID(ctx, f.ds.ID)
	require.NoError(t, err)
	worker := *before
	now := time.Now().UTC()
	worker.LastSyncAt = &now
	worker.LastSyncCursor = types.JSON(`{"connector_cursor":{"files":{"pages/release/manual.txt":"published-sha"}}}`)
	worker.LastSyncResult = types.JSON(`{"created":3}`)
	worker.ErrorMessage = "worker state must survive settings writes"
	require.NoError(t, f.dsRepo.UpdateSyncState(ctx, &worker))
	connector.err = errors.New("unchanged source must not be revalidated")
	after := *before
	after.Config = nil // The native schedule-only handler intentionally omits it.
	after.SyncSchedule = "CRON_TZ=Asia/Shanghai 0 0 1 * * *"
	saved, err := svc.PatchDataSourceSyncSettings(ctx, before, &after, []string{"sync_schedule", "sync_deletions"})
	require.NoError(t, err)
	require.Zero(t, connector.calls)
	require.Equal(t, before.Config, saved.Config)
	require.Equal(t, after.SyncSchedule, saved.SyncSchedule)
	require.False(t, saved.SyncDeletions)
	require.Equal(t, worker.LastSyncCursor, saved.LastSyncCursor)
	require.Equal(t, worker.LastSyncResult, saved.LastSyncResult)
	require.Equal(t, worker.ErrorMessage, saved.ErrorMessage)
	require.NotNil(t, saved.LastSyncAt)
	require.WithinDuration(t, now, *saved.LastSyncAt, time.Millisecond)
}

func TestNASPatchConfigurationChangesStillValidate(t *testing.T) {
	for _, valid := range []bool{true, false} {
		name := "valid"
		if !valid {
			name = "rejected"
		}
		t.Run(name, func(t *testing.T) {
			svc, f, connector := nasSettingsFixture(t)
			ctx := context.Background()
			before, err := f.dsRepo.FindByID(ctx, f.ds.ID)
			require.NoError(t, err)
			after := *before
			after.Config = types.JSON(`{"type":"dokuwiki_filesystem","settings":{"source_alias":"vendor-wiki"},"resource_ids":["namespace:release:audio"]}`)
			if !valid {
				connector.err = datasource.ErrInvalidConfig
			}
			_, err = svc.PatchDataSourceSyncSettings(ctx, before, &after, []string{"config"})
			require.Equal(t, 1, connector.calls)
			require.NotNil(t, connector.config)
			require.Equal(t, []string{"namespace:release:audio"}, connector.config.ResourceIDs)
			stored, readErr := f.dsRepo.FindByID(ctx, before.ID)
			require.NoError(t, readErr)
			if valid {
				require.NoError(t, err)
				cfg, parseErr := stored.ParseConfig()
				require.NoError(t, parseErr)
				require.Equal(t, []string{"namespace:release:audio"}, cfg.ResourceIDs)
			} else {
				require.ErrorIs(t, err, datasource.ErrInvalidConfig)
				require.Equal(t, before.Config, stored.Config)
			}
		})
	}
}
