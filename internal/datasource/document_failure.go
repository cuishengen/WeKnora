package datasource

import "fmt"

// DocumentParseFailure is a confirmed rejection of one document's content.
// Connectors may continue the scan, but must retain the last published hash
// and retry this item next time. Infrastructure/auth/configuration errors must
// never be wrapped in this type.
type DocumentParseFailure struct {
	KnowledgeID string
	Reason      string
}

func (e *DocumentParseFailure) Error() string {
	return fmt.Sprintf("document %s could not be parsed: %s", e.KnowledgeID, e.Reason)
}
