package service

import (
	"testing"

	"github.com/Tencent/WeKnora/internal/types"
	"github.com/stretchr/testify/require"
)

func TestNASPDFRecognitionDefersOnlyConfirmedImageOnlyPDFWithoutVision(t *testing.T) {
	knowledge := &types.Knowledge{IsStaged: true, Channel: types.ConnectorTypeDokuWikiFilesystem}
	payload := types.DocumentProcessPayload{FileType: "pdf"}
	result := &types.ReadResult{Metadata: map[string]string{"text_content_chars": "0", "scanned_page_count": "1", "image_source_type": "scanned_pdf"},
		ImageRefs: []types.ImageRef{{}}}
	require.True(t, needsDeferredPDFRecognition(knowledge, payload, result))
	for _, change := range []func(*types.Knowledge, *types.DocumentProcessPayload, *types.ReadResult){
		func(k *types.Knowledge, _ *types.DocumentProcessPayload, _ *types.ReadResult) { k.IsStaged = false },
		func(k *types.Knowledge, _ *types.DocumentProcessPayload, _ *types.ReadResult) { k.Channel = "web" },
		func(_ *types.Knowledge, p *types.DocumentProcessPayload, _ *types.ReadResult) {
			p.EnableMultimodel = true
		},
		func(_ *types.Knowledge, p *types.DocumentProcessPayload, _ *types.ReadResult) { p.FileType = "docx" },
		func(_ *types.Knowledge, _ *types.DocumentProcessPayload, r *types.ReadResult) { r.Metadata = nil },
		func(_ *types.Knowledge, _ *types.DocumentProcessPayload, r *types.ReadResult) {
			r.Metadata["text_content_chars"] = "12"
		},
		func(_ *types.Knowledge, _ *types.DocumentProcessPayload, r *types.ReadResult) {
			r.Metadata["scanned_page_count"] = "0"
		},
		func(_ *types.Knowledge, _ *types.DocumentProcessPayload, r *types.ReadResult) {
			r.Metadata["scanned_page_count"] = "invalid"
		},
		func(_ *types.Knowledge, _ *types.DocumentProcessPayload, r *types.ReadResult) {
			r.Metadata["scanned_page_count"] = "-1"
		},
		func(_ *types.Knowledge, _ *types.DocumentProcessPayload, r *types.ReadResult) { r.ImageRefs = nil },
		func(_ *types.Knowledge, _ *types.DocumentProcessPayload, r *types.ReadResult) {
			r.Error = "parse failed"
		},
		func(_ *types.Knowledge, _ *types.DocumentProcessPayload, r *types.ReadResult) {
			r.Metadata["image_source_type"] = "unknown"
		},
	} {
		k, p, r := *knowledge, payload, *result
		r.Metadata = map[string]string{"text_content_chars": "0", "scanned_page_count": "1", "image_source_type": "scanned_pdf"}
		change(&k, &p, &r)
		require.False(t, needsDeferredPDFRecognition(&k, p, &r))
	}
}
