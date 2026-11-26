package service

import (
	"strconv"
	"strings"

	"github.com/Tencent/WeKnora/internal/types"
)

const stagedVisionDeferredPrefix = "Document recognition deferred: "
const visionDisabledReason = "Visible PDF content requires image recognition, which is disabled for this knowledge base"

// Only the builtin PDF parser's explicit safe-text measurement may classify a
// staged original as image-only. Unknown/older parsers keep their normal path.
func needsDeferredPDFRecognition(knowledge *types.Knowledge, payload types.DocumentProcessPayload, result *types.ReadResult) bool {
	if result == nil {
		return false
	}
	scannedPages, err := strconv.Atoi(result.Metadata["scanned_page_count"])
	return err == nil && scannedPages > 0 && knowledge != nil && knowledge.IsStaged && knowledge.Channel == types.ConnectorTypeDokuWikiFilesystem &&
		strings.EqualFold(payload.FileType, "pdf") && !payload.EnableMultimodel && result != nil &&
		result.Error == "" && result.Metadata["text_content_chars"] == "0" &&
		result.Metadata["image_source_type"] == "scanned_pdf" &&
		len(result.ImageRefs) > 0
}
