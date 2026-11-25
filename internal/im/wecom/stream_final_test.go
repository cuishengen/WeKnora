package wecom

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/Tencent/WeKnora/internal/im"
	"github.com/stretchr/testify/require"
)

func TestEndStreamWithContentDiscardsCachedDraftAndSendsOneFinalFrame(t *testing.T) {
	frames := make(chan streamReplyBody, 2)
	client := mediaTestClient(t, func(frame wsFrame) (interface{}, int) {
		var body streamReplyBody
		require.NoError(t, json.Unmarshal(frame.Body, &body))
		frames <- body
		return nil, 0
	})
	adapter := NewWSAdapter(client)
	msg := &im.IncomingMessage{Extra: map[string]string{"req_id": "stream-request"}}
	id, err := adapter.StartStream(context.Background(), msg)
	require.NoError(t, err)
	// A stale body simulates a successful partial or failed prior replacement.
	client.streamBufsMu.Lock()
	client.streamBufs[id].WriteString("STALE_PROTECTED_DRAFT")
	client.streamBufsMu.Unlock()
	require.NoError(t, adapter.EndStreamWithContent(context.Background(), msg, id, "员工授权已失效"))
	select {
	case body := <-frames:
		require.True(t, body.Stream.Finish)
		require.Equal(t, id, body.Stream.ID)
		require.Equal(t, "员工授权已失效", body.Stream.Content)
	case <-time.After(time.Second):
		t.Fatal("missing final WebSocket frame")
	}
	client.streamBufsMu.Lock()
	_, retained := client.streamBufs[id]
	client.streamBufsMu.Unlock()
	require.False(t, retained)
}
