package slack

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
)

// webAPI is a minimal Slack Web API client (auth.test, chat.postMessage,
// chat.update, chat.delete, the three-step external file upload). The base
// URL is configurable (tests point it at a local fake); the bot token rides
// the Authorization header and NEVER appears in an error or a log line —
// Slack's own error codes ("invalid_auth", "channel_not_found", …) are all
// the callers see.
type webAPI struct {
	base     string
	botToken string
	hc       *http.Client
}

func newWebAPI(base, botToken string, hc *http.Client) *webAPI {
	if hc == nil {
		hc = http.DefaultClient
	}
	return &webAPI{base: base, botToken: botToken, hc: hc}
}

// apiError is a Slack-side refusal: the method's error code only (never the
// request, which embeds the token).
type apiError struct {
	method string
	code   string
}

func (e *apiError) Error() string { return "slack " + e.method + ": " + e.code }

// call posts one Web API method with a JSON body and decodes its response
// into out (nil when the method returns nothing beyond ok).
func (a *webAPI) call(ctx context.Context, method string, body map[string]any, out any) error {
	var reqBody io.Reader
	if body != nil {
		blob, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("slack %s: encode request: %w", method, err)
		}
		reqBody = bytes.NewReader(blob)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.base+"/"+method, reqBody)
	if err != nil {
		return fmt.Errorf("slack %s: build request: %w", method, err)
	}
	req.Header.Set("Authorization", "Bearer "+a.botToken)
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	resp, err := a.hc.Do(req)
	if err != nil {
		return fmt.Errorf("slack %s: %w", method, err) // *url.Error: method + URL only, no headers
	}
	defer resp.Body.Close() //nolint:errcheck
	var envelope struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
	}
	// Peer-payload read cap (see maxPeerPayloadBytes): a larger or endless
	// body is a broken or hostile endpoint, never a payload to buffer whole.
	blob, err := io.ReadAll(io.LimitReader(resp.Body, maxPeerPayloadBytes))
	if err != nil {
		return fmt.Errorf("slack %s: read response: %w", method, err)
	}
	if err := json.Unmarshal(blob, &envelope); err != nil {
		return fmt.Errorf("slack %s: decode response: HTTP %d", method, resp.StatusCode)
	}
	if !envelope.OK {
		code := envelope.Error
		if code == "" {
			code = fmt.Sprintf("http_%d", resp.StatusCode)
		}
		return &apiError{method: method, code: code}
	}
	if out != nil {
		if err := json.Unmarshal(blob, out); err != nil {
			return fmt.Errorf("slack %s: decode response: %w", method, err)
		}
	}
	return nil
}

// authTest returns the bot's own user id (used to recognise self messages).
func (a *webAPI) authTest(ctx context.Context) (botUserID string, err error) {
	var out struct {
		UserID string `json:"user_id"`
	}
	if err := a.call(ctx, "auth.test", map[string]any{}, &out); err != nil {
		return "", err
	}
	return out.UserID, nil
}

// postParams is a chat.postMessage. Blocks, when non-empty, render instead of
// text (text stays as the fallback/notification line). ThreadTS, when set,
// posts into that thread — never broadcast to the channel.
type postParams struct {
	Channel  string
	ThreadTS string
	Text     string
	Blocks   []block
}

func (a *webAPI) postMessage(ctx context.Context, p postParams) (ts string, err error) {
	body := map[string]any{
		"channel": p.Channel,
		"text":    p.Text,
	}
	if p.ThreadTS != "" {
		body["thread_ts"] = p.ThreadTS
	}
	if len(p.Blocks) > 0 {
		body["blocks"] = p.Blocks
	}
	var out struct {
		TS string `json:"ts"`
	}
	if err := a.call(ctx, "chat.postMessage", body, &out); err != nil {
		return "", err
	}
	return out.TS, nil
}

// updateParams is a chat.update. Blocks is passed as an explicit empty array
// when non-nil, which strips an actions block from the message (the
// button-press and prompt-expiry edits).
type updateParams struct {
	Channel string
	TS      string
	Text    string
	Blocks  []block // non-nil (may be empty) ⇒ send "blocks": []
}

func (a *webAPI) updateMessage(ctx context.Context, p updateParams) error {
	body := map[string]any{
		"channel": p.Channel,
		"ts":      p.TS,
		"text":    p.Text,
	}
	if p.Blocks != nil {
		body["blocks"] = p.Blocks
	}
	return a.call(ctx, "chat.update", body, nil)
}

func (a *webAPI) deleteMessage(ctx context.Context, channel, ts string) error {
	return a.call(ctx, "chat.delete", map[string]any{"channel": channel, "ts": ts}, nil)
}

// urlCause strips the URL from a transport error aimed at a pre-signed
// endpoint. The upload URL Slack hands out carries its credential in the
// query (sig=…), and a raw *url.Error embeds the full URL ("Post
// \"https://…?sig=…\""), so an unwrapped error would leak the signature into
// logs and tool output. Only the cause survives; the caller's wrapper names
// the operation.
func urlCause(err error) error {
	var uerr *url.Error
	if errors.As(err, &uerr) {
		return uerr.Err
	}
	return err
}

// uploadParams is one external file upload (files.getUploadURLExternal →
// POST the bytes → files.completeUploadExternal attached to a conversation).
type uploadParams struct {
	Filename       string
	Data           []byte
	Channel        string
	ThreadTS       string
	InitialComment string
}

func (a *webAPI) uploadFile(ctx context.Context, p uploadParams) error {
	var draft struct {
		UploadURL string `json:"upload_url"`
		FileID    string `json:"file_id"`
	}
	if err := a.call(ctx, "files.getUploadURLExternal", map[string]any{
		"filename": p.Filename,
		"length":   len(p.Data),
	}, &draft); err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, draft.UploadURL, bytes.NewReader(p.Data))
	if err != nil {
		// *url.Error of op "parse": the URL (with its signature) must not
		// reach the message either.
		return fmt.Errorf("slack file upload: build request: %w", urlCause(err))
	}
	resp, err := a.hc.Do(req)
	if err != nil {
		return fmt.Errorf("slack file upload: %w", urlCause(err))
	}
	defer resp.Body.Close() //nolint:errcheck
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("slack file upload: HTTP %d", resp.StatusCode)
	}
	// completeUploadExternal attaches the uploaded file to a conversation.
	// The file is named in the REQUIRED "files" array ([{"id": …, "title": …}]
	// — docs.slack.dev); a top-level file_id is not an alternative shape, and
	// Slack rejects the call without the array. The title is the filename.
	complete := map[string]any{
		"files":    []map[string]any{{"id": draft.FileID, "title": p.Filename}},
		"channels": p.Channel,
	}
	if p.ThreadTS != "" {
		complete["thread_ts"] = p.ThreadTS
	}
	if p.InitialComment != "" {
		complete["initial_comment"] = p.InitialComment
	}
	return a.call(ctx, "files.completeUploadExternal", complete, nil)
}
