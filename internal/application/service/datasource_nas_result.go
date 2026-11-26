package service

import (
	"encoding/json"
	"path"
	"sort"

	"github.com/Tencent/WeKnora/internal/types"
)

// A resumed NAS run can skip a rejection already attempted by an earlier task
// in the same run. The durable cursor, rather than repeated attempts, defines
// the unresolved failure count after a confirmed checkpoint or complete scan.
func reconcileNASFailures(ds *types.DataSource, cursor *types.SyncCursor, result *types.SyncResult) {
	if ds.Type != types.ConnectorTypeDokuWikiFilesystem || cursor == nil {
		return
	}
	encoded, err := cursor.ToJSON()
	if err != nil {
		return
	}
	var state struct {
		Connector struct {
			Failed map[string]string `json:"failed"`
		} `json:"connector_cursor"`
	}
	if json.Unmarshal(encoded, &state) != nil || state.Connector.Failed == nil {
		return
	}
	result.Failed = len(state.Connector.Failed)
	result.Total = result.Created + result.Updated + result.Deleted + result.Skipped + result.Failed
	result.Errors = nil
	paths := make([]string, 0, len(state.Connector.Failed))
	for filename := range state.Connector.Failed {
		paths = append(paths, filename)
	}
	sort.Strings(paths)
	for _, filename := range paths {
		recordSyncError(result, types.SyncItemError{Title: path.Base(filename), Code: "ingest_failed",
			Message: "Document parsing failed; original and previous publication preserved"})
		if len(result.Errors) == maxSyncResultErrors {
			break
		}
	}
}
