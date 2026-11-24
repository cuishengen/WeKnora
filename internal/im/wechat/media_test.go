package wechat

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"testing"

	"github.com/Tencent/WeKnora/internal/im"
	"github.com/stretchr/testify/require"
)

type mediaRoundTripper func(*http.Request) (*http.Response, error)

func (f mediaRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestWeChatFileDeliveryEncryptsExactOriginalAndChecksAcknowledgement(t *testing.T) {
	originalClient := ilinkHTTPClient
	t.Cleanup(func() { ilinkHTTPClient = originalClient })
	data := []byte("PK\x03\x04original SDK archive bytes")
	var key []byte
	var sent bool
	ilinkHTTPClient = &http.Client{Transport: mediaRoundTripper(func(req *http.Request) (*http.Response, error) {
		response := &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(bytes.NewBufferString(`{"ret":0}`))}
		switch req.URL.Path {
		case "/ilink/bot/getuploadurl":
			require.Equal(t, "Bearer bot-secret", req.Header.Get("Authorization"))
			var payload struct {
				AESKey    string `json:"aeskey"`
				MediaType int    `json:"media_type"`
				To        string `json:"to_user_id"`
				RawSize   int    `json:"rawsize"`
			}
			require.NoError(t, json.NewDecoder(req.Body).Decode(&payload))
			require.Equal(t, 3, payload.MediaType)
			require.Equal(t, "verified-sender", payload.To)
			require.Equal(t, len(data), payload.RawSize)
			var err error
			key, err = parseAESKey(payload.AESKey)
			require.NoError(t, err)
			response.Body = io.NopCloser(bytes.NewBufferString(`{"upload_full_url":"https://novac2c.cdn.weixin.qq.com/c2c/upload"}`))
		case "/c2c/upload":
			require.Empty(t, req.Header.Get("Authorization"), "CDN must not receive the bot token")
			ciphertext, err := io.ReadAll(req.Body)
			require.NoError(t, err)
			require.NotEqual(t, data, ciphertext)
			plaintext, err := decryptAES128ECB(ciphertext, key)
			require.NoError(t, err)
			require.Equal(t, data, plaintext)
			response.Header.Set("x-encrypted-param", "download-reference")
		case "/ilink/bot/sendmessage":
			var payload struct {
				Msg struct {
					To    string `json:"to_user_id"`
					Token string `json:"context_token"`
					Items []struct {
						Type int      `json:"type"`
						File fileItem `json:"file_item"`
					} `json:"item_list"`
				} `json:"msg"`
			}
			require.NoError(t, json.NewDecoder(req.Body).Decode(&payload))
			require.Equal(t, "verified-sender", payload.Msg.To)
			require.Equal(t, "current-context", payload.Msg.Token)
			require.Len(t, payload.Msg.Items, 1)
			require.Equal(t, 4, payload.Msg.Items[0].Type)
			require.Equal(t, "sdk.zip", payload.Msg.Items[0].File.FileName)
			require.Equal(t, fmt.Sprint(len(data)), payload.Msg.Items[0].File.Len)
			require.Equal(t, "download-reference", payload.Msg.Items[0].File.Media.EncryptQueryParam)
			decodedKey, err := parseAESKey(payload.Msg.Items[0].File.Media.AESKey)
			require.NoError(t, err)
			require.Equal(t, key, decodedKey)
			sent = true
		default:
			t.Errorf("unexpected endpoint %s", req.URL.Path)
		}
		return response, nil
	})}
	adapter := NewAdapter("bot-secret", "bot")
	require.NoError(t, adapter.SendFile(context.Background(), &im.IncomingMessage{UserID: "verified-sender", Extra: map[string]string{"context_token": "current-context"}}, "sdk.zip", data))
	require.True(t, sent)
	ilinkHTTPClient = &http.Client{Transport: mediaRoundTripper(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Body: io.NopCloser(bytes.NewBufferString(`{"ret":-14}`))}, nil
	})}
	require.Error(t, adapter.SendReply(context.Background(), &im.IncomingMessage{UserID: "verified-sender"}, &im.ReplyMessage{Content: "reply"}), "HTTP 200 with a failed platform status is not delivery")
}

func TestWeChatIncomingIdentityMustBelongToThisBotAndBeDirect(t *testing.T) {
	client := NewLongPollClient("token", "bot", nil)
	msg := &weixinMessage{FromUserID: "member", ToUserID: "other-bot", MessageType: 1, ItemList: []messageItem{{Type: 1, TextItem: &textItem{Text: "hello"}}}}
	require.Nil(t, client.parseMessage(msg))
	msg.ToUserID = "bot"
	msg.GroupID = "group"
	require.Nil(t, client.parseMessage(msg))
	msg.GroupID = ""
	require.NotNil(t, client.parseMessage(msg))
}
