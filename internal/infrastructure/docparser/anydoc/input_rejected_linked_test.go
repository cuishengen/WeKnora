//go:build anydoc && cgo

package anydoc

import (
	"fmt"
	"testing"

	upstream "github.com/firecrawl/anydoc/go"
	"github.com/stretchr/testify/require"
)

func TestInputRejectedSeparatesDocumentAndOperationalFailures(t *testing.T) {
	for _, kind := range []string{"unsupported", "malformed", "encrypted", "resource_limit", "missing_part"} {
		err := fmt.Errorf("convert: %w", &upstream.ConvertError{Kind: kind, Detail: "input rejected"})
		require.True(t, InputRejected(err), kind)
	}
	for _, kind := range []string{"io", "pdf_no_model", "unknown_format", "needs_ocr"} {
		require.False(t, InputRejected(&upstream.ConvertError{Kind: kind}), kind)
	}
	require.False(t, InputRejected(ErrUnavailable))
}
