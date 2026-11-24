package chatpipeline

import (
	"context"
	"strings"
	"testing"

	"github.com/Tencent/WeKnora/internal/types"
	"github.com/stretchr/testify/require"
)

func TestSpreadsheetLocationsReachActualModelContext(t *testing.T) {
	content := "A: 9,B: ZX410A,C: 25"
	result := &types.SearchResult{
		ID: "sheet-chunk", KnowledgeID: "sheet-doc", KnowledgeBaseID: "stock-kb",
		KnowledgeTitle: "Count workbook.xlsx", Content: content,
		SourceLocators: types.SourceLocators{{
			Type: types.SourceLocatorSheet, Sheet: "Month-end count", RowStart: 12, RowEnd: 15,
			SourceID: "private-parser-block", SourceHash: "private-source-revision", Quote: content,
		}},
	}
	manage := &types.ChatManage{
		PipelineRequest: types.PipelineRequest{
			Query:         "quantity for ZX410A",
			SummaryConfig: types.SummaryConfig{Prompt: "system", ContextTemplate: "{{contexts}}\n{{query}}"},
		},
		PipelineState: types.PipelineState{UserContent: "quantity for ZX410A", MergeResult: []*types.SearchResult{result}},
	}

	plugin := &PluginIntoChatMessage{}
	require.Nil(t, plugin.OnEvent(context.Background(), types.INTO_CHAT_MESSAGE, manage, func() *PluginError { return nil }))
	require.Contains(t, manage.RenderedContexts, `Worksheet "Month-end count": rows 12-15.`)
	messages, refs := prepareMessagesWithModelContext(context.Background(), manage)
	require.Len(t, messages, 2)
	modelContent := messages[1].Content
	require.Contains(t, modelContent, `Worksheet "Month-end count": rows 12-15.`)
	require.Contains(t, modelContent, "original worksheet rows, not item numbers")
	require.Contains(t, modelContent, "use headers from the same worksheet")
	require.Contains(t, modelContent, content)
	for _, durable := range []string{"sheet-chunk", "sheet-doc", "stock-kb", "private-parser-block", "private-source-revision"} {
		require.NotContains(t, modelContent, durable)
	}
	require.Equal(t, content, result.Content, "adding model context must not rewrite the stored passage")
	require.Equal(t, 12, result.SourceLocators[0].RowStart)
	require.Equal(t, `<kb doc="Count workbook.xlsx" chunk_id="sheet-chunk" kb_id="stock-kb" />`,
		refs.DecodeOutputText(`<ref id="chunk://1"/>`))
}

func TestSpreadsheetPassageKeepsMultipleSheetsDistinctWithoutInventingRowMapping(t *testing.T) {
	result := &types.SearchResult{
		Content: "A: row from first sheet\nA: row from second sheet",
		SourceLocators: types.SourceLocators{
			{Type: types.SourceLocatorSheet, Sheet: "Current", RowStart: 8, RowEnd: 9},
			{Type: types.SourceLocatorSheet, Sheet: "Current", RowStart: 8, RowEnd: 9},
			{Type: types.SourceLocatorSheet, Sheet: "Archive\n\"2025\"", RowStart: 1, RowEnd: 3, Partial: true},
			{Type: types.SourceLocatorSheet, Sheet: "Invalid", RowStart: 0, RowEnd: 3},
			{Type: types.SourceLocatorSheet, Sheet: "Invalid", RowStart: 6, RowEnd: 3},
			{Type: types.SourceLocatorSheet, Sheet: " ", RowStart: 1, RowEnd: 1},
			{Type: types.SourceLocatorPDF, Page: 2},
		},
	}

	passage := getEnrichedPassageForChat(context.Background(), result)
	require.Equal(t, 1, strings.Count(passage, `Worksheet "Current": rows 8-9.`))
	require.Contains(t, passage, `Worksheet "Archive\n\"2025\"": rows 1-3.`)
	require.Contains(t, passage, "Keep their data and headers separate")
	require.Contains(t, passage, "do not assign each text row to a worksheet")
	require.Contains(t, passage, "these locations do not cover all text")
	require.NotContains(t, passage, "Invalid")
	require.True(t, strings.HasSuffix(passage, result.Content))
}

func TestNonSpreadsheetPassageIsUnchanged(t *testing.T) {
	for _, locators := range []types.SourceLocators{
		nil,
		{{Type: types.SourceLocatorPDF, Page: 2}},
		{{Type: types.SourceLocatorSheet, Sheet: "Missing row bounds"}},
	} {
		result := &types.SearchResult{Content: "Unchanged passage", SourceLocators: locators}
		require.Equal(t, result.Content, getEnrichedPassageForChat(context.Background(), result))
	}
	empty := &types.SearchResult{SourceLocators: types.SourceLocators{{
		Type: types.SourceLocatorSheet, Sheet: "Empty", RowStart: 1, RowEnd: 1,
	}}}
	require.Empty(t, getEnrichedPassageForChat(context.Background(), empty))
}

func TestSourceProvenanceDistinguishesSameNamedDocumentsInModelContext(t *testing.T) {
	results := []*types.SearchResult{
		{
			ID: "first-copy-chunk", KnowledgeID: "first-copy-doc", KnowledgeBaseID: "manuals-kb",
			KnowledgeTitle: "chip_datasheet.pdf", Content: "Version 0.1 lists capacity 12.",
			Metadata: map[string]string{
				"source_alias": "supplier-manuals", "relative_path": "media/earlier-family/chip_datasheet.pdf",
				"source_id":         "media:earlier-family:chip_datasheet.pdf",
				"source_url":        "https://manuals.example.test/fetch.php?media=earlier-family:chip_datasheet.pdf",
				"source_updated_at": "2026-10-01T16:00:00+08:00",
			},
		},
		{
			ID: "second-copy-chunk", KnowledgeID: "second-copy-doc", KnowledgeBaseID: "manuals-kb",
			KnowledgeTitle: "chip_datasheet.pdf", Content: "Version 0.2 lists capacity 16.",
			Metadata: map[string]string{
				"relative_path": "media/later-family/chip_datasheet.pdf", "source_id": "media:later-family:chip_datasheet.pdf",
			},
		},
	}
	manage := &types.ChatManage{
		PipelineRequest: types.PipelineRequest{
			Query:         "compare the capacity",
			SummaryConfig: types.SummaryConfig{Prompt: "system", ContextTemplate: "{{contexts}}\n{{query}}"},
		},
		PipelineState: types.PipelineState{MergeResult: results},
	}
	plugin := &PluginIntoChatMessage{}
	require.Nil(t, plugin.OnEvent(context.Background(), types.INTO_CHAT_MESSAGE, manage, func() *PluginError { return nil }))
	messages, refs := prepareMessagesWithModelContext(context.Background(), manage)
	modelContent := messages[len(messages)-1].Content
	for _, relative := range []string{"media/earlier-family/chip_datasheet.pdf", "media/later-family/chip_datasheet.pdf"} {
		require.Contains(t, modelContent, relative)
		require.Contains(t, manage.RenderedContexts, relative)
	}
	require.Contains(t, modelContent, "media:earlier-family:chip_datasheet.pdf")
	require.Contains(t, modelContent, "media:later-family:chip_datasheet.pdf")
	require.Contains(t, modelContent, "not document publication date): 2026-10-01T08:00:00Z")
	require.Contains(t, modelContent, "they do not establish version priority")
	require.Contains(t, modelContent, results[0].Content)
	require.Contains(t, modelContent, results[1].Content)
	require.Equal(t, `<kb doc="chip_datasheet.pdf" chunk_id="first-copy-chunk" kb_id="manuals-kb" />`,
		refs.DecodeOutputText(`<ref id="chunk://1"/>`))
	require.Equal(t, `<kb doc="chip_datasheet.pdf" chunk_id="second-copy-chunk" kb_id="manuals-kb" />`,
		refs.DecodeOutputText(`<ref id="chunk://2"/>`))
}

func TestSourceProvenanceOmitsSecretsAndUnverifiedTimes(t *testing.T) {
	metadata := map[string]string{
		"relative_path":     "media/spec.pdf",
		"source_url":        "https://private-user:private-password@manuals.example.test/fetch.php?media=spec.pdf&token=private-query-token#private-fragment",
		"source_updated_at": "not-a-verified-time",
		"api_key":           "private-api-key", "credentials": "private-credentials",
		"datasource_id": "private-datasource-id", "external_id": "private-external-id",
		"sha256": "private-source-hash", "storage_path": "private-storage-path",
	}
	context := sourceProvenanceContext(metadata)
	require.Contains(t, context, `"https://manuals.example.test/fetch.php"`)
	for _, secret := range []string{
		"private-user", "private-password", "private-query-token", "private-fragment", "private-api-key",
		"private-credentials", "private-datasource-id", "private-external-id", "private-source-hash", "private-storage-path",
		"not-a-verified-time", "source_modified_at",
	} {
		require.NotContains(t, context, secret)
	}
	require.Contains(t, metadata["source_url"], "private-query-token", "sanitizing the model view must not mutate source metadata")
	require.Empty(t, sourceProvenanceContext(map[string]string{"source_url": "javascript:alert('private')"}))
	require.Empty(t, sourceProvenanceContext(nil))
}
