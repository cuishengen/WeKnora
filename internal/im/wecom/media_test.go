package wecom

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/md5"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Tencent/WeKnora/internal/im"
	ws "github.com/gorilla/websocket"
	"github.com/stretchr/testify/require"
)

func TestSendFileUploadsExactOriginalAndWaitsForSendAcknowledgement(t *testing.T) {
	var archive bytes.Buffer
	zw := zip.NewWriter(&archive)
	entry, err := zw.Create("sdk/readme.txt")
	require.NoError(t, err)
	_, err = entry.Write([]byte("original SDK attachment"))
	require.NoError(t, err)
	require.NoError(t, zw.Close())
	for _, fixture := range []struct {
		name string
		data []byte
	}{
		{"datasheet.pdf", append([]byte("%PDF-1.4\n"), bytes.Repeat([]byte("original data\n"), 50000)...)},
		{"sdk.zip", archive.Bytes()},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			var assembled bytes.Buffer
			var totalSize, nextChunk int
			var declaredMD5 string
			delivered := make(chan struct{}, 1)
			client := mediaTestClient(t, func(frame wsFrame) (interface{}, int) {
				var body map[string]interface{}
				require.NoError(t, json.Unmarshal(frame.Body, &body))
				switch frame.Cmd {
				case "aibot_upload_media_init":
					require.Equal(t, fixture.name, body["filename"])
					totalSize = int(body["total_size"].(float64))
					declaredMD5 = body["md5"].(string)
					return map[string]string{"upload_id": "upload"}, 0
				case "aibot_upload_media_chunk":
					require.Equal(t, "upload", body["upload_id"])
					require.Equal(t, nextChunk, int(body["chunk_index"].(float64)))
					chunk, err := base64.StdEncoding.DecodeString(body["base64_data"].(string))
					require.NoError(t, err)
					require.LessOrEqual(t, len(chunk), mediaChunkBytes)
					assembled.Write(chunk)
					nextChunk++
					return nil, 0
				case "aibot_upload_media_finish":
					require.Equal(t, totalSize, assembled.Len())
					require.Equal(t, fixture.data, assembled.Bytes())
					checksum := md5.Sum(assembled.Bytes())
					require.Equal(t, hex.EncodeToString(checksum[:]), declaredMD5)
					return map[string]string{"media_id": "original-file"}, 0
				case "aibot_send_msg":
					require.Equal(t, "verified-user", body["chatid"])
					require.Equal(t, float64(1), body["chat_type"])
					require.Equal(t, "file", body["msgtype"])
					require.Equal(t, "original-file", body["file"].(map[string]interface{})["media_id"])
					delivered <- struct{}{}
					return nil, 0
				default:
					t.Errorf("unexpected command %s", frame.Cmd)
					return nil, 400
				}
			})
			require.NoError(t, client.SendFile(context.Background(), &im.IncomingMessage{UserID: "verified-user", ChatType: im.ChatTypeDirect}, fixture.name, fixture.data))
			select {
			case <-delivered:
			default:
				t.Fatal("returned before platform delivery acknowledgement")
			}
		})
	}
}

func TestSendFileFailsOnPlatformRejectionAndInvalidDestinations(t *testing.T) {
	client := mediaTestClient(t, func(wsFrame) (interface{}, int) { return nil, 40013 })
	err := client.SendFile(context.Background(), &im.IncomingMessage{UserID: "member"}, "doc.pdf", []byte("%PDF-1.4"))
	require.ErrorContains(t, err, "40013")
	require.Error(t, client.SendFile(context.Background(), &im.IncomingMessage{UserID: "member", ChatType: im.ChatTypeGroup}, "doc.pdf", []byte("%PDF-1.4")))
	require.Error(t, client.SendFile(context.Background(), &im.IncomingMessage{UserID: "member"}, "large.zip", make([]byte, maxOutboundFileBytes+1)))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = client.requestAck(ctx, "aibot_send_msg", nil)
	require.ErrorIs(t, err, context.Canceled)
}

func mediaTestClient(t *testing.T, handle func(wsFrame) (interface{}, int)) *LongConnClient {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := (&ws.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		for {
			var frame wsFrame
			if conn.ReadJSON(&frame) != nil {
				return
			}
			body, code := handle(frame)
			encoded, _ := json.Marshal(body)
			if conn.WriteJSON(wsFrame{Headers: frame.Headers, Body: encoded, ErrCode: code}) != nil {
				return
			}
		}
	}))
	t.Cleanup(server.Close)
	conn, _, err := ws.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http"), nil)
	require.NoError(t, err)
	client := &LongConnClient{conn: conn}
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		for {
			var frame wsFrame
			if conn.ReadJSON(&frame) != nil {
				return
			}
			client.receiveAck(frame)
		}
	}()
	t.Cleanup(func() {
		client.Stop()
		select {
		case <-finished:
		case <-time.After(time.Second):
			t.Error("receipt reader did not stop")
		}
	})
	return client
}
