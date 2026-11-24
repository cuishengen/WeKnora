package chatpipeline

import (
	"context"
	"fmt"
	"html"
	"net/url"
	"strings"
	"time"

	"github.com/Tencent/WeKnora/internal/reranking"
	"github.com/Tencent/WeKnora/internal/searchutil"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
	"github.com/Tencent/WeKnora/internal/utils"
)

// PluginIntoChatMessage handles the transformation of search results into chat messages
type PluginIntoChatMessage struct {
	messageService interfaces.MessageService
	// kbService reads retrieved images for a vision chat model. Nil leaves
	// the model with their captions.
	kbService interfaces.KnowledgeBaseService
}

// NewPluginIntoChatMessage creates and registers a new PluginIntoChatMessage instance
func NewPluginIntoChatMessage(
	eventManager *EventManager, messageService interfaces.MessageService, kbService interfaces.KnowledgeBaseService,
) *PluginIntoChatMessage {
	res := &PluginIntoChatMessage{messageService: messageService, kbService: kbService}
	eventManager.Register(res)
	return res
}

// ActivationEvents returns the event types this plugin handles
func (p *PluginIntoChatMessage) ActivationEvents() []types.EventType {
	return []types.EventType{types.INTO_CHAT_MESSAGE}
}

// OnEvent processes the INTO_CHAT_MESSAGE event to format chat message content
func (p *PluginIntoChatMessage) OnEvent(ctx context.Context,
	eventType types.EventType, chatManage *types.ChatManage, next func() *PluginError,
) *PluginError {
	pipelineInfo(ctx, "IntoChatMessage", "input", map[string]interface{}{
		"session_id":       chatManage.SessionID,
		"merge_result_cnt": len(chatManage.MergeResult),
		"template_len":     len(chatManage.SummaryConfig.ContextTemplate),
	})

	// Separate FAQ and document results when FAQ priority is enabled
	var faqResults, docResults []*types.SearchResult
	var hasHighConfidenceFAQ bool
	// exactFAQ is the position in faqResults of the FAQ that cleared the
	// direct-answer threshold; only that entry is marked as an exact match.
	exactFAQ := -1

	if chatManage.FAQPriorityEnabled {
		for _, result := range chatManage.MergeResult {
			if result.ChunkType == string(types.ChunkTypeFAQ) {
				faqResults = append(faqResults, result)
				// Check if this FAQ has high confidence (above direct answer
				// threshold). Compare the score before boosts: the FAQ boost
				// exists to rank FAQs above documents, and letting it (or the
				// wiki/memory boosts) lift an FAQ over the threshold marked
				// middling matches as exact.
				if reranking.PreBoostScore(result) >= chatManage.FAQDirectAnswerThreshold && !hasHighConfidenceFAQ {
					hasHighConfidenceFAQ = true
					exactFAQ = len(faqResults) - 1
					pipelineInfo(ctx, "IntoChatMessage", "high_confidence_faq", map[string]interface{}{
						"chunk_id":  result.ID,
						"score":     fmt.Sprintf("%.4f", reranking.PreBoostScore(result)),
						"threshold": chatManage.FAQDirectAnswerThreshold,
					})
				}
			} else {
				docResults = append(docResults, result)
			}
		}
		pipelineInfo(ctx, "IntoChatMessage", "faq_separation", map[string]interface{}{
			"faq_count":           len(faqResults),
			"doc_count":           len(docResults),
			"has_high_confidence": hasHighConfidenceFAQ,
		})
	}

	// 验证用户查询的安全性
	safeQuery, isValid := utils.ValidateInput(chatManage.Query)
	if !isValid {
		pipelineWarn(ctx, "IntoChatMessage", "invalid_query", map[string]interface{}{
			"session_id": chatManage.SessionID,
		})
		return ErrTemplateExecute.WithError(fmt.Errorf("user query contains invalid content"))
	}

	// Intent-based no-search path: no retrieval results, but still render
	// through the context template so runtime metadata (current_time, etc.) is injected.
	if !chatManage.NeedsRetrieval() {
		userContent := safeQuery
		if rewrite := strings.TrimSpace(chatManage.RewriteQuery); rewrite != "" {
			if safeRewrite, ok := utils.ValidateInput(rewrite); ok {
				userContent = safeRewrite
			} else {
				pipelineWarn(ctx, "IntoChatMessage", "invalid_rewrite_query_fallback", map[string]interface{}{
					"session_id": chatManage.SessionID,
				})
			}
		}
		if chatManage.ImageDescription != "" && !chatManage.ChatModelSupportsVision {
			userContent += "\n\n[用户上传图片内容]\n" + chatManage.ImageDescription
		}
		if chatManage.QuotedContext != "" {
			userContent += "\n\n" + chatManage.QuotedContext
		}
		// Inject attachment content (documents, audio transcripts, etc.)
		if len(chatManage.Attachments) > 0 {
			userContent += chatManage.Attachments.BuildPrompt()
		}

		if tpl := chatManage.SummaryConfig.ContextTemplate; tpl != "" {
			chatManage.UserContent = types.RenderPromptPlaceholders(tpl, types.PlaceholderValues{
				"query":    userContent,
				"contexts": "",
				"language": chatManage.Language,
			})
		} else {
			chatManage.UserContent = userContent
		}

		pipelineInfo(ctx, "IntoChatMessage", "no_search_with_template", map[string]interface{}{
			"session_id":       chatManage.SessionID,
			"user_content_len": len(chatManage.UserContent),
			"has_template":     chatManage.SummaryConfig.ContextTemplate != "",
		})
		return next()
	}

	var contextsBuilder strings.Builder
	// Keep the result order for image selection; final model handles are
	// assigned later, in prepareMessagesWithModelContext.
	var contextOrder []*types.SearchResult

	// Collect unique document metadata (title + description), once per knowledge
	allResults := chatManage.MergeResult
	if chatManage.FAQPriorityEnabled && len(faqResults) > 0 {
		allResults = append(faqResults, docResults...)
	}
	docHeader := buildDocumentHeader(allResults)
	if docHeader != "" {
		contextsBuilder.WriteString(docHeader)
		contextsBuilder.WriteString("\n")
	}

	// Build contexts string based on FAQ priority strategy
	if chatManage.FAQPriorityEnabled && len(faqResults) > 0 {
		contextsBuilder.WriteString("<source type=\"faq\" priority=\"high\">\n")
		for i, result := range faqResults {
			passage := getEnrichedPassageForChat(ctx, result)
			contextOrder = append(contextOrder, result)
			if i == exactFAQ {
				contextsBuilder.WriteString(fmt.Sprintf("<context id=\"FAQ-%d\" match=\"exact\">%s</context>\n", i+1, passage))
			} else {
				contextsBuilder.WriteString(fmt.Sprintf("<context id=\"FAQ-%d\">%s</context>\n", i+1, passage))
			}
		}
		contextsBuilder.WriteString("</source>\n")

		if len(docResults) > 0 {
			contextsBuilder.WriteString("<source type=\"document\" priority=\"supplementary\">\n")
			for i, result := range docResults {
				passage := getEnrichedPassageForChat(ctx, result)
				contextOrder = append(contextOrder, result)
				contextsBuilder.WriteString(fmt.Sprintf("<context id=\"DOC-%d\">%s</context>\n", i+1, passage))
			}
			contextsBuilder.WriteString("</source>")
		}
	} else {
		for i, result := range chatManage.MergeResult {
			passage := getEnrichedPassageForChat(ctx, result)
			contextOrder = append(contextOrder, result)
			if i > 0 {
				contextsBuilder.WriteString("\n")
			}
			contextsBuilder.WriteString(fmt.Sprintf("<context id=\"%d\">%s</context>", i+1, passage))
		}
	}

	chatManage.RenderedContexts = contextsBuilder.String()

	// Replace placeholders in context template
	userContent := types.RenderPromptPlaceholders(chatManage.SummaryConfig.ContextTemplate, types.PlaceholderValues{
		"query":    safeQuery,
		"contexts": chatManage.RenderedContexts,
		"language": chatManage.Language,
	})

	// Append image description as text fallback only when the chat model cannot
	// process images directly. Vision-capable models see images via MultiContent.
	if chatManage.ImageDescription != "" && !chatManage.ChatModelSupportsVision {
		userContent += "\n\n[用户上传图片内容]\n" + chatManage.ImageDescription
	}
	p.attachContextImages(ctx, chatManage, contextOrder)
	if chatManage.QuotedContext != "" {
		userContent += "\n\n" + chatManage.QuotedContext
	}
	// Inject attachment content (documents, audio transcripts, etc.)
	if len(chatManage.Attachments) > 0 {
		userContent += chatManage.Attachments.BuildPrompt()
	}

	// Set formatted content back to chat management
	chatManage.UserContent = userContent
	pipelineInfo(ctx, "IntoChatMessage", "output", map[string]interface{}{
		"session_id":                 chatManage.SessionID,
		"user_content_len":           len(chatManage.UserContent),
		"faq_priority":               chatManage.FAQPriorityEnabled,
		"intent":                     chatManage.Intent,
		"image_description":          chatManage.ImageDescription,
		"chat_model_supports_vision": chatManage.ChatModelSupportsVision,
	})

	p.persistRenderedContent(ctx, chatManage)
	return next()
}

// attachContextImages reads, for a vision chat model, the images of the
// contexts that rest on an image matched by its own vector: the caption a
// context carries may leave out exactly what matched. It sets ContextImages
// and durable chunk IDs; the final message renderer supplies their handles.
func (p *PluginIntoChatMessage) attachContextImages(
	ctx context.Context, chatManage *types.ChatManage, contexts []*types.SearchResult,
) {
	chatManage.ContextImages = nil
	chatManage.ContextImageChunkIDs = nil
	if !chatManage.ChatModelSupportsVision || p.kbService == nil {
		return
	}
	images, positions := searchutil.ContextImages(
		ctx, contexts, p.kbService.ReadChunkImage, searchutil.MaxContextImages)
	if len(images) == 0 {
		return
	}
	chatManage.ContextImages = images
	named := make([]string, len(positions))
	for i, pos := range positions {
		named[i] = contexts[pos].ID
	}
	chatManage.ContextImageChunkIDs = named
	pipelineInfo(ctx, "IntoChatMessage", "context_images", map[string]interface{}{
		"session_id": chatManage.SessionID,
		"count":      len(images),
		"contexts":   named,
	})
}

// persistRenderedContent asynchronously writes the RAG-augmented UserContent back
// to the user message so that subsequent conversation turns can see the full
// retrieval context in history.
func (p *PluginIntoChatMessage) persistRenderedContent(ctx context.Context, chatManage *types.ChatManage) {
	if chatManage.UserMessageID == "" || chatManage.UserContent == "" {
		pipelineInfo(ctx, "IntoChatMessage", "persist_rendered_content_skip", map[string]interface{}{
			"session_id":       chatManage.SessionID,
			"user_message_id":  chatManage.UserMessageID,
			"has_user_content": chatManage.UserContent != "",
			"reason":           "empty_id_or_content",
		})
		return
	}
	if chatManage.UserContent == chatManage.Query {
		return
	}
	pipelineInfo(ctx, "IntoChatMessage", "persist_rendered_content", map[string]interface{}{
		"session_id":           chatManage.SessionID,
		"user_message_id":      chatManage.UserMessageID,
		"rendered_content_len": len(chatManage.UserContent),
	})
	bgCtx := context.WithoutCancel(ctx)
	go func() {
		if err := p.messageService.UpdateMessageRenderedContent(
			bgCtx, chatManage.SessionID, chatManage.UserMessageID, chatManage.UserContent,
		); err != nil {
			pipelineWarn(bgCtx, "IntoChatMessage", "persist_rendered_content_error", map[string]interface{}{
				"session_id":      chatManage.SessionID,
				"user_message_id": chatManage.UserMessageID,
				"error":           err.Error(),
			})
		}
	}()
}

// buildDocumentHeader generates a document metadata section listing each unique
// knowledge document (by KnowledgeID) with its title and description.
// Returns an empty string when no meaningful metadata is available.
func buildDocumentHeader(results []*types.SearchResult) string {
	type docMeta struct {
		title       string
		description string
		metadata    string
	}

	seen := make(map[string]struct{})
	var docs []docMeta

	for _, r := range results {
		if r.KnowledgeID == "" {
			continue
		}
		if _, ok := seen[r.KnowledgeID]; ok {
			continue
		}
		seen[r.KnowledgeID] = struct{}{}

		title := r.KnowledgeTitle
		if title == "" {
			title = r.KnowledgeFilename
		}
		if title == "" {
			continue
		}

		docs = append(docs, docMeta{
			title:       title,
			description: r.KnowledgeDescription,
			metadata:    r.KnowledgeCustomMetadata,
		})
	}

	if len(docs) == 0 {
		return ""
	}

	var b strings.Builder
	b.WriteString("<documents>\n")
	for _, d := range docs {
		b.WriteString("<document>\n")
		b.WriteString(fmt.Sprintf("<title>%s</title>\n", html.EscapeString(d.title)))
		if d.description != "" {
			b.WriteString(fmt.Sprintf("<description>%s</description>\n", html.EscapeString(d.description)))
		}
		if d.metadata != "" {
			b.WriteString(fmt.Sprintf("<metadata>%s</metadata>\n", html.EscapeString(d.metadata)))
		}
		b.WriteString("</document>\n")
	}
	b.WriteString("</documents>")
	return b.String()
}

// getEnrichedPassageForChat 合并Content和ImageInfo的文本内容，为聊天消息准备
func getEnrichedPassageForChat(ctx context.Context, result *types.SearchResult) string {
	if result.Content == "" && result.ImageInfo == "" {
		return ""
	}

	content := result.Content
	if result.ImageInfo != "" {
		content = enrichContentWithImageInfo(ctx, content, result.ImageInfo)
	}
	return sourceProvenanceContext(result.Metadata) + spreadsheetPassageContext(result.SourceLocators) + content
}

// sourceProvenanceContext retains the source labels needed to distinguish
// identically named documents. Only this explicit allowlist reaches the model;
// connector credentials, storage locations and internal IDs must stay private.
func sourceProvenanceContext(metadata map[string]string) string {
	var b strings.Builder
	for _, key := range []string{"source_alias", "relative_path", "source_id"} {
		if value := strings.TrimSpace(metadata[key]); value != "" {
			fmt.Fprintf(&b, "- %s: %q\n", key, value)
		}
	}
	if raw := strings.TrimSpace(metadata["source_url"]); raw != "" {
		if sourceURL, err := url.Parse(raw); err == nil && sourceURL.Host != "" &&
			(sourceURL.Scheme == "https" || sourceURL.Scheme == "http") {
			// Source URLs may contain signed query parameters or basic auth.
			// Path/identity labels above distinguish copies without those secrets.
			sourceURL.User = nil
			sourceURL.RawQuery = ""
			sourceURL.ForceQuery = false
			sourceURL.Fragment = ""
			fmt.Fprintf(&b, "- source_url (credentials/query/fragment omitted): %q\n", sourceURL.String())
		}
	}
	if modified, err := time.Parse(time.RFC3339, metadata["source_updated_at"]); err == nil {
		fmt.Fprintf(&b, "- source_modified_at (connector/file modification time, not document publication date): %s\n",
			modified.UTC().Format(time.RFC3339))
	}
	if b.Len() == 0 {
		return ""
	}
	return "[Source provenance: distinguish source copies by these labels; they do not establish version priority.]\n" + b.String() + "\n"
}

// spreadsheetPassageContext exposes positions already stored with a passage.
// Both the persisted context and the final model view use this path; adding
// source_locators to the tool-result map alone would be dropped by its renderer.
// Do not infer column labels or individual row positions from a folded range.
func spreadsheetPassageContext(locators types.SourceLocators) string {
	type sheetRange struct {
		sheet string
		start int
		end   int
	}
	seen := make(map[sheetRange]bool)
	sheets := make(map[string]bool)
	var ranges []sheetRange
	partial := false
	for _, locator := range locators {
		if locator.Type != types.SourceLocatorSheet || strings.TrimSpace(locator.Sheet) == "" ||
			locator.RowStart < 1 || locator.RowEnd < locator.RowStart {
			continue
		}
		position := sheetRange{locator.Sheet, locator.RowStart, locator.RowEnd}
		if !seen[position] {
			seen[position] = true
			ranges = append(ranges, position)
		}
		sheets[locator.Sheet] = true
		partial = partial || locator.Partial
	}
	if len(ranges) == 0 {
		return ""
	}

	var b strings.Builder
	b.WriteString("[Spreadsheet source locations: row numbers refer to original worksheet rows, not item numbers. " +
		"Column letters alone do not establish field meanings; use headers from the same worksheet before interpreting quantities.]\n")
	for _, position := range ranges {
		fmt.Fprintf(&b, "- Worksheet %q: rows %d-%d.\n", position.sheet, position.start, position.end)
	}
	if len(sheets) > 1 {
		b.WriteString("[This passage spans multiple worksheets. Keep their data and headers separate; " +
			"the locations above do not assign each text row to a worksheet.]\n")
	}
	if partial {
		b.WriteString("[Some passage text has no source mapping; these locations do not cover all text.]\n")
	}
	return b.String() + "\n"
}

// enrichContentWithImageInfo delegates to the shared searchutil implementation.
func enrichContentWithImageInfo(_ context.Context, content string, imageInfoJSON string) string {
	return searchutil.EnrichContentWithImageInfoForChat(content, imageInfoJSON)
}
