package wecom

import (
	"context"
	"crypto/md5"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/Tencent/WeKnora/internal/im"
)

const (
	maxOutboundFileBytes = 20 << 20
	mediaChunkBytes      = 512 << 10
	mediaAckTimeout      = 15 * time.Second
)

// SendFile implements the official long-connection temporary-media protocol.
// Upload and delivery are acknowledged separately; a failed acknowledgement is
// never retried blindly because the platform may already have delivered it.
func (c *LongConnClient) SendFile(ctx context.Context, incoming *im.IncomingMessage, filename string, content []byte) error {
	if incoming == nil || incoming.ChatType == im.ChatTypeGroup || incoming.UserID == "" {
		return fmt.Errorf("original files require a direct conversation")
	}
	if len(content) < 5 || len(content) > maxOutboundFileBytes {
		return fmt.Errorf("file must contain 5 bytes to 20 MiB")
	}
	if strings.TrimSpace(filename) == "" || len(filename) > 256 {
		return fmt.Errorf("invalid attachment filename")
	}
	checksum := md5.Sum(content)
	response, err := c.requestAck(ctx, "aibot_upload_media_init", map[string]interface{}{
		"type": "file", "filename": filename, "total_size": len(content),
		"total_chunks": (len(content) + mediaChunkBytes - 1) / mediaChunkBytes,
		"md5":          hex.EncodeToString(checksum[:]),
	})
	if err != nil {
		return fmt.Errorf("initialize attachment upload: %w", err)
	}
	var initialized struct {
		UploadID string `json:"upload_id"`
	}
	if err := json.Unmarshal(response.Body, &initialized); err != nil || initialized.UploadID == "" {
		return fmt.Errorf("upload acknowledgement omitted upload_id")
	}
	for offset, index := 0, 0; offset < len(content); offset, index = offset+mediaChunkBytes, index+1 {
		end := min(offset+mediaChunkBytes, len(content))
		if _, err := c.requestAck(ctx, "aibot_upload_media_chunk", map[string]interface{}{
			"upload_id": initialized.UploadID, "chunk_index": index,
			"base64_data": base64.StdEncoding.EncodeToString(content[offset:end]),
		}); err != nil {
			return fmt.Errorf("upload attachment chunk %d: %w", index, err)
		}
	}
	response, err = c.requestAck(ctx, "aibot_upload_media_finish", map[string]interface{}{"upload_id": initialized.UploadID})
	if err != nil {
		return fmt.Errorf("finish attachment upload: %w", err)
	}
	var uploaded struct {
		MediaID string `json:"media_id"`
	}
	if err := json.Unmarshal(response.Body, &uploaded); err != nil || uploaded.MediaID == "" {
		return fmt.Errorf("upload acknowledgement omitted media_id")
	}
	_, err = c.requestAck(ctx, "aibot_send_msg", map[string]interface{}{
		"chatid": incoming.UserID, "chat_type": 1,
		"msgtype": "file", "file": map[string]string{"media_id": uploaded.MediaID},
	})
	if err != nil {
		return fmt.Errorf("send attachment: %w", err)
	}
	return nil
}

func (c *LongConnClient) requestAck(ctx context.Context, command string, body interface{}) (wsFrame, error) {
	if err := ctx.Err(); err != nil {
		return wsFrame{}, err
	}
	requestID := fmt.Sprintf("media_%d_%d", time.Now().UnixNano(), c.reqSeq.Add(1))
	response := make(chan wsFrame, 1)
	c.ackMu.Lock()
	if c.acks == nil {
		c.acks = make(map[string]chan wsFrame)
	}
	c.acks[requestID] = response
	c.ackMu.Unlock()
	defer func() {
		c.ackMu.Lock()
		delete(c.acks, requestID)
		c.ackMu.Unlock()
	}()
	payload, err := json.Marshal(body)
	if err != nil {
		return wsFrame{}, err
	}
	if err := c.writeJSON(wsFrame{Cmd: command, Headers: map[string]string{"req_id": requestID}, Body: payload}); err != nil {
		return wsFrame{}, err
	}
	timer := time.NewTimer(mediaAckTimeout)
	defer timer.Stop()
	select {
	case frame := <-response:
		if frame.ErrCode != 0 {
			// Do not log the raw platform response: it may echo recipient data.
			return frame, fmt.Errorf("platform rejected %s (code %d)", command, frame.ErrCode)
		}
		return frame, nil
	case <-ctx.Done():
		return wsFrame{}, ctx.Err()
	case <-timer.C:
		return wsFrame{}, fmt.Errorf("platform acknowledgement timed out")
	}
}

func (c *LongConnClient) receiveAck(frame wsFrame) {
	c.ackMu.Lock()
	response := c.acks[frame.Headers["req_id"]]
	c.ackMu.Unlock()
	if response != nil {
		select {
		case response <- frame:
		default:
		}
	}
}
