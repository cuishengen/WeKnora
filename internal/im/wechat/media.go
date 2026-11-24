package wechat

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/md5"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/Tencent/WeKnora/internal/im"
)

const maxOutboundFileBytes = 20 << 20

var _ im.FileSender = (*Adapter)(nil)

func (a *Adapter) MaxFileSize() int64 { return maxOutboundFileBytes }

// SendFile follows Tencent's published iLink CDN protocol. File data is
// encrypted before upload; the API and CDN never share authentication headers.
func (a *Adapter) SendFile(ctx context.Context, incoming *im.IncomingMessage, filename string, content []byte) error {
	if incoming == nil || incoming.UserID == "" || incoming.ChatType == im.ChatTypeGroup || incoming.Extra["context_token"] == "" {
		return fmt.Errorf("file delivery requires a current direct conversation")
	}
	if len(content) == 0 || len(content) > maxOutboundFileBytes || strings.TrimSpace(filename) == "" {
		return fmt.Errorf("invalid attachment")
	}
	key, fileKey := make([]byte, 16), make([]byte, 16)
	if _, err := rand.Read(key); err != nil {
		return err
	}
	if _, err := rand.Read(fileKey); err != nil {
		return err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return err
	}
	padding := aes.BlockSize - len(content)%aes.BlockSize
	padded := append(append([]byte(nil), content...), bytes.Repeat([]byte{byte(padding)}, padding)...)
	encrypted := make([]byte, len(padded))
	for offset := 0; offset < len(padded); offset += aes.BlockSize {
		block.Encrypt(encrypted[offset:offset+aes.BlockSize], padded[offset:offset+aes.BlockSize])
	}
	checksum := md5.Sum(content)
	var upload struct {
		UploadParam   string `json:"upload_param"`
		UploadFullURL string `json:"upload_full_url"`
	}
	err = a.ilinkPost(ctx, "/ilink/bot/getuploadurl", map[string]interface{}{
		"filekey": hex.EncodeToString(fileKey), "media_type": 3, "to_user_id": incoming.UserID,
		"rawsize": len(content), "rawfilemd5": hex.EncodeToString(checksum[:]), "filesize": len(encrypted),
		"no_need_thumb": true, "aeskey": hex.EncodeToString(key), "base_info": newBaseInfo(),
	}, &upload)
	if err != nil {
		return err
	}
	uploadURL := upload.UploadFullURL
	if uploadURL == "" && upload.UploadParam != "" {
		uploadURL = cdnBaseURL + "/upload?encrypted_query_param=" + url.QueryEscape(upload.UploadParam) + "&filekey=" + url.QueryEscape(hex.EncodeToString(fileKey))
	}
	u, err := url.Parse(uploadURL)
	if err != nil || u.Scheme != "https" || u.User != nil || u.Fragment != "" || u.Port() != "" && u.Port() != "443" ||
		!(u.Hostname() == "novac2c.cdn.weixin.qq.com" || strings.HasSuffix(u.Hostname(), ".cdn.weixin.qq.com")) {
		return fmt.Errorf("untrusted iLink media upload endpoint")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, uploadURL, bytes.NewReader(encrypted))
	if err != nil {
		return fmt.Errorf("invalid media upload request")
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	// Keep redirects from forwarding an encrypted company original elsewhere.
	client := *ilinkHTTPClient
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	response, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("media upload failed")
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
	response.Body.Close()
	downloadParam := response.Header.Get("x-encrypted-param")
	if response.StatusCode != http.StatusOK || downloadParam == "" {
		return fmt.Errorf("media upload was not acknowledged")
	}
	return a.ilinkPost(ctx, "/ilink/bot/sendmessage", map[string]interface{}{
		"msg": map[string]interface{}{
			"from_user_id": "", "to_user_id": incoming.UserID, "client_id": fmt.Sprintf("weknora_file_%d", time.Now().UnixNano()),
			"message_type": 2, "message_state": 2, "context_token": incoming.Extra["context_token"],
			"item_list": []map[string]interface{}{{"type": 4, "file_item": map[string]interface{}{
				"file_name": filename, "len": strconv.Itoa(len(content)), "media": map[string]interface{}{
					"encrypt_query_param": downloadParam, "aes_key": base64.StdEncoding.EncodeToString([]byte(hex.EncodeToString(key))), "encrypt_type": 1,
				},
			}}},
		}, "base_info": newBaseInfo(),
	}, nil)
}
