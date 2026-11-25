package im

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/Tencent/WeKnora/internal/companyidentity"
)

const (
	companyStreamInterval  = time.Second
	companyStreamHeartbeat = 10 * time.Second
	companyStreamTimeout   = 5 * time.Minute
	companyStreamDenied    = "员工授权已失效或暂时无法核验，请联系管理员。"
	companyStreamTimedOut  = "处理超时，回答尚未完成，请缩小问题后重试。"
	companyStreamMaxBytes  = 20 << 10
	companyStreamTruncated = "\n\n内容较长，已截断；可缩小范围继续提问。"
)

// Company progress is a fixed vocabulary. Tool arguments/results and model
// thoughts never become a progress label, including for unknown tools.
func companyStreamToolStage(name string) string {
	switch name {
	case "search_knowledge", "knowledge_search", "wiki_search", "list_documents":
		return "正在检索资料…"
	case "read_document", "read_file", "wiki_read_page", "wiki_read_issue", "query_knowledge_graph":
		return "正在核对原文…"
	case "query_understand":
		return "正在理解问题…"
	default:
		return "正在处理…"
	}
}

func companyStreamDisplay(stage, answer string, elapsed time.Duration) string {
	if strings.TrimSpace(answer) != "" {
		stage = "正在生成回答…"
	}
	if elapsed >= companyStreamHeartbeat {
		stage += fmt.Sprintf("（已用时 %d 秒）", int(elapsed/companyStreamHeartbeat)*10)
	}
	if answer != "" {
		return stage + "\n\n" + answer
	}
	return stage
}

// Hold back incomplete Markdown destinations as well as storage URLs. In
// particular, a split source-download URL must be rewritten only once its
// complete knowledge ID is available, before a client can open the link.
func companyStreamHoldback(answer string) string {
	cut := holdbackCutoff(answer)
	if idx := strings.LastIndex(answer, "]("); idx >= 0 && len(answer)-idx <= 2048 &&
		!strings.ContainsAny(answer[idx+2:], ") \t\r\n") {
		start := strings.LastIndex(answer[:idx], "[")
		if start < 0 {
			start = idx
		}
		cut = min(cut, start)
	}
	if idx := strings.LastIndex(answer, "/platform/"); idx >= 0 {
		tail := answer[idx:]
		const route = "/platform/source-download/"
		if !strings.ContainsAny(tail, " \t\r\n)<>\"'") &&
			(strings.HasPrefix(route, tail) || strings.HasPrefix(tail, route) && !originalDownloadRE.MatchString(tail)) {
			start := strings.LastIndexAny(answer[:idx], " \t\r\n(") + 1
			if !strings.HasPrefix(answer[start:idx], "https://") && !strings.HasPrefix(answer[start:idx], "http://") {
				start = idx
			}
			cut = min(cut, start)
		}
	}
	return answer[:cut]
}

// WeCom rejects bodies above 20 KiB. Bound every company frame, including its
// status/suffix, without cutting a UTF-8 rune or a Markdown destination in two.
func companyStreamLimit(content string) string {
	if len(content) <= companyStreamMaxBytes {
		return content
	}
	cut := companyStreamMaxBytes - len(companyStreamTruncated)
	for cut > 0 && !utf8.RuneStart(content[cut]) {
		cut--
	}
	visible := companyStreamHoldback(content[:cut])
	start := strings.LastIndexAny(visible, " \t\r\n()") + 1
	if strings.HasPrefix(visible[start:], "https://") || strings.HasPrefix(visible[start:], "http://") {
		visible = visible[:start]
	}
	return strings.TrimSpace(visible) + companyStreamTruncated
}

func companyStreamFailure(err error) string {
	switch {
	case errors.Is(err, companyidentity.ErrDenied):
		return companyStreamDenied
	case errors.Is(err, context.DeadlineExceeded):
		return companyStreamTimedOut
	case errors.Is(err, context.Canceled):
		return imCancelledFallback
	default:
		return imErrorFallback
	}
}

// EndStream retransmits the adapter's cached body. Recheck this boundary too;
// on denial replace that cache before ending, so a failed final write cannot
// resend an older authorized answer after the employee has been revoked.
func (s *Service) finishCompanyIMStream(ctx context.Context, msg *IncomingMessage, streamer StreamSender, adapter Adapter, streamID, content string) (string, bool, error) {
	content = companyStreamLimit(content)
	outCtx, cancel := context.WithTimeout(imOutboundContext(ctx), 15*time.Second)
	defer cancel()
	authorizedCtx, authErr := s.authorizeIMAnswer(outCtx, msg)
	if authErr != nil {
		content = companyStreamDenied
	} else {
		outCtx = authorizedCtx
	}
	finalErr := streamer.FinalizeStream(outCtx, msg, streamID, content)

	endCtx, endAuthErr := s.authorizeIMAnswer(outCtx, msg)
	if endAuthErr != nil {
		content = companyStreamDenied
	} else {
		outCtx = endCtx
	}
	var endErr error
	if finisher, ok := streamer.(StreamFinalContentSender); ok {
		// This capability sends exactly content and discards the old cache;
		// a failed preceding replace cannot strand or resend the old draft.
		endErr = finisher.EndStreamWithContent(outCtx, msg, streamID, content)
		if endErr == nil {
			finalErr = nil
		}
	} else {
		if endAuthErr != nil {
			// Unknown adapters must prove their generic replacement succeeded
			// before EndStream is allowed to retransmit their cached body.
			finalErr = streamer.FinalizeStream(outCtx, msg, streamID, content)
		}
		if finalErr == nil {
			endErr = streamer.EndStream(outCtx, msg, streamID)
		}
	}
	var fallbackErr error
	delivered := finalErr == nil && endErr == nil
	if !delivered {
		fallbackCtx, fallbackAuthErr := s.authorizeIMAnswer(outCtx, msg)
		if fallbackAuthErr != nil {
			content = companyStreamDenied
		} else {
			outCtx = fallbackCtx
		}
		fallbackErr = adapter.SendReply(outCtx, msg, &ReplyMessage{Content: content, IsFinal: true})
		delivered = fallbackErr == nil
		authErr = errors.Join(authErr, fallbackAuthErr)
	}
	authErr = errors.Join(authErr, endAuthErr)
	if authErr != nil {
		return content, false, authErr
	}
	if delivered {
		return content, true, nil
	}
	return content, false, errors.Join(finalErr, endErr, fallbackErr)
}
