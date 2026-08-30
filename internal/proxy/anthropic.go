package proxy

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"

	"github.com/Urenzu/trace-portal/internal/trace"
)

// requestBody is the subset of the Messages API request we index. Everything
// else stays in the blob and is never parsed on the hot path.
type requestBody struct {
	Model     string          `json:"model"`
	Stream    bool            `json:"stream"`
	MaxTokens int             `json:"max_tokens"`
	System    json.RawMessage `json:"system"`
	Messages  []struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	} `json:"messages"`
	Tools []struct {
		Name string `json:"name"`
	} `json:"tools"`
}

// summarizeRequest fills the request-side fields of ev from a Messages API
// body. A body it cannot parse leaves ev untouched rather than failing the
// proxied call — observability must never break the thing it observes.
func summarizeRequest(body []byte, ev *trace.Event) {
	var req requestBody
	if err := json.Unmarshal(body, &req); err != nil {
		return
	}
	ev.Model = req.Model
	ev.Stream = req.Stream
	ev.MaxTokens = req.MaxTokens
	ev.MessageCount = len(req.Messages)
	ev.SystemBlocks = countSystemBlocks(req.System)
	for _, t := range req.Tools {
		ev.ToolsOffered = append(ev.ToolsOffered, t.Name)
	}
}

func countSystemBlocks(raw json.RawMessage) int {
	if len(raw) == 0 {
		return 0
	}
	var blocks []json.RawMessage
	if err := json.Unmarshal(raw, &blocks); err == nil {
		return len(blocks)
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil && s != "" {
		return 1
	}
	return 0
}

// sessionKey groups turns of the same conversation. Agent loops replay a
// growing message list against a stable system prompt and first user message,
// so hashing that prefix identifies a session without the client cooperating.
func sessionKey(body []byte) string {
	var req requestBody
	if err := json.Unmarshal(body, &req); err != nil {
		return "unknown"
	}
	h := sha256.New()
	h.Write(req.System)
	for _, m := range req.Messages {
		if m.Role == "user" {
			h.Write(m.Content)
			break
		}
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}

// responseBody is the subset of a non-streaming Messages API response we index.
type responseBody struct {
	StopReason string `json:"stop_reason"`
	Model      string `json:"model"`
	Content    []struct {
		Type string `json:"type"`
		ID   string `json:"id"`
		Name string `json:"name"`
	} `json:"content"`
	Usage *trace.Usage `json:"usage"`
}

// summarizeResponse fills the response-side fields of ev from a buffered JSON
// response body.
func summarizeResponse(body []byte, ev *trace.Event) {
	var resp responseBody
	if err := json.Unmarshal(body, &resp); err != nil {
		return
	}
	ev.StopReason = resp.StopReason
	if ev.Model == "" {
		ev.Model = resp.Model
	}
	ev.Usage = resp.Usage
	for _, c := range resp.Content {
		if c.Type == "tool_use" {
			ev.ToolCalls = append(ev.ToolCalls, trace.ToolCall{ID: c.ID, Name: c.Name})
		}
	}
}

// sseFrame is the union of the streaming events we care about. Frames we do not
// recognize (content_block_delta text, ping, …) unmarshal into zero values and
// are ignored.
type sseFrame struct {
	Type    string `json:"type"`
	Message *struct {
		Model string       `json:"model"`
		Usage *trace.Usage `json:"usage"`
	} `json:"message"`
	ContentBlock *struct {
		Type string `json:"type"`
		ID   string `json:"id"`
		Name string `json:"name"`
	} `json:"content_block"`
	Delta *struct {
		StopReason string `json:"stop_reason"`
	} `json:"delta"`
	Usage *trace.Usage `json:"usage"`
}

// summarizeSSE folds a captured SSE stream into ev. Input-token counters arrive
// once in message_start; output tokens are restated in message_delta, so the
// later value wins.
func summarizeSSE(stream []byte, ev *trace.Event) {
	sc := bufio.NewScanner(bytes.NewReader(stream))
	// Tool-use inputs are streamed as partial JSON and a single frame can be
	// large; give the scanner room well past the default 64KiB line limit.
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)

	for sc.Scan() {
		line := sc.Bytes()
		if !bytes.HasPrefix(line, []byte("data:")) {
			continue
		}
		payload := bytes.TrimSpace(line[len("data:"):])
		if len(payload) == 0 || bytes.Equal(payload, []byte("[DONE]")) {
			continue
		}

		var f sseFrame
		if err := json.Unmarshal(payload, &f); err != nil {
			continue
		}
		switch f.Type {
		case "message_start":
			if f.Message == nil {
				continue
			}
			if ev.Model == "" {
				ev.Model = f.Message.Model
			}
			if f.Message.Usage != nil {
				u := *f.Message.Usage
				ev.Usage = &u
			}
		case "content_block_start":
			if f.ContentBlock != nil && f.ContentBlock.Type == "tool_use" {
				ev.ToolCalls = append(ev.ToolCalls, trace.ToolCall{
					ID:   f.ContentBlock.ID,
					Name: f.ContentBlock.Name,
				})
			}
		case "message_delta":
			if f.Delta != nil && f.Delta.StopReason != "" {
				ev.StopReason = f.Delta.StopReason
			}
			if f.Usage != nil {
				if ev.Usage == nil {
					ev.Usage = &trace.Usage{}
				}
				ev.Usage.OutputTokens = f.Usage.OutputTokens
				// Later API versions restate input counters here too.
				if f.Usage.InputTokens > 0 {
					ev.Usage.InputTokens = f.Usage.InputTokens
				}
			}
		}
	}
}

func isSSE(contentType string) bool {
	return strings.HasPrefix(contentType, "text/event-stream")
}
