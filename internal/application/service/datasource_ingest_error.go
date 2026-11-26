package service

import (
	"errors"

	"github.com/Tencent/WeKnora/internal/datasource"
)

func isDocumentRecognitionDeferred(err error) bool {
	var deferred *datasource.DocumentParseDeferred
	return errors.As(err, &deferred)
}

// dataSourceIngestError preserves the cause for retries without exposing storage
// diagnostics in the user-facing sync log. Details are logged at the ingest site.
type dataSourceIngestError struct {
	cause error
}

func (e *dataSourceIngestError) Error() string {
	return "Ingest failed; see server logs"
}

func (e *dataSourceIngestError) Unwrap() error {
	return e.cause
}
