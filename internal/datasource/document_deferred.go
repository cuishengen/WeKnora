package datasource

// DocumentParseDeferred preserves an original whose visible content needs an
// intentionally disabled parser capability. It is not a successful ingestion.
type DocumentParseDeferred struct {
	Reason string
}

func (e *DocumentParseDeferred) Error() string { return e.Reason }
