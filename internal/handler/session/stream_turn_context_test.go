package session

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
)

// cancelAfterFirstReadStream mimics Redis: a canceled context fails the read.
// The first successful read cancels the caller's context, the way agent mode
// releases the sandbox turn lease before the completion event is published.
type cancelAfterFirstReadStream struct {
	interfaces.StreamManager
	events []interfaces.StreamEvent
	cancel context.CancelFunc
	reads  int
}

func (s *cancelAfterFirstReadStream) GetEvents(
	ctx context.Context, _, _ string, fromOffset int,
) ([]interfaces.StreamEvent, int, error) {
	if err := ctx.Err(); err != nil {
		return nil, fromOffset, fmt.Errorf("failed to get events from Redis: %w", err)
	}
	s.reads++
	if s.reads == 1 {
		s.cancel()
		return s.events[:1], 1, nil
	}
	if fromOffset >= len(s.events) {
		return nil, len(s.events), nil
	}
	return s.events[fromOffset:], len(s.events), nil
}

func TestHandleAgentEventsForSSE_DrainsCompletionAfterTurnContextCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	h := &Handler{streamManager: &cancelAfterFirstReadStream{
		cancel: cancel,
		events: []interfaces.StreamEvent{
			{ID: "answer-1", Type: types.ResponseTypeAnswer, Content: "你好", Done: true},
			{ID: "complete-1", Type: types.ResponseTypeComplete, Done: true},
		},
	}}
	c, recorder := newTestGinContext(t, "")

	done := make(chan struct{})
	go func() {
		h.handleAgentEventsForSSE(
			ctx, c, "sess1", "msg1", "req-1", nil, false, publicStreamRewriter())
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("SSE poll kept using the cancelled turn context and never delivered completion")
	}

	body := recorder.Body.String()
	require.Contains(t, body, "你好")
	assert.Contains(t, body, `"response_type":"complete"`)
}

func TestHandleAgentEventsForSSE_TerminalErrorClosesWithoutCompleteOrTitle(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h := &Handler{streamManager: &cancelAfterFirstReadStream{
		cancel: cancel,
		events: []interfaces.StreamEvent{{ID: "error", Type: types.ResponseTypeError,
			Content: "configured rerank model is unavailable", Done: true}},
	}}
	c, recorder := newTestGinContext(t, "")
	done := make(chan struct{})
	go func() {
		h.handleAgentEventsForSSE(ctx, c, "error-session", "error-message", "request", nil, true, nil)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("terminal error left the SSE connection waiting for complete/title")
	}
	require.Contains(t, recorder.Body.String(), `"response_type":"error"`)
	require.Contains(t, recorder.Body.String(), "configured rerank model is unavailable")
}

func TestHandleAgentEventsForSSE_OrdinaryDoneDoesNotCloseStream(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h := &Handler{streamManager: &cancelAfterFirstReadStream{
		cancel: cancel,
		events: []interfaces.StreamEvent{
			{ID: "tool", Type: types.ResponseTypeToolResult, Content: "retrieval finished", Done: true},
			{ID: "progress", Type: types.ResponseTypeProgress, Content: "reading source", Done: true},
			{ID: "answer", Type: types.ResponseTypeAnswer, Content: "answer after tool", Done: true},
			{ID: "complete", Type: types.ResponseTypeComplete, Done: true},
		},
	}}
	c, recorder := newTestGinContext(t, "")
	done := make(chan struct{})
	go func() {
		h.handleAgentEventsForSSE(ctx, c, "normal-session", "normal-message", "request", nil, false, nil)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("normal stream did not finish")
	}
	require.Contains(t, recorder.Body.String(), "answer after tool")
	require.Contains(t, recorder.Body.String(), "reading source")
}
