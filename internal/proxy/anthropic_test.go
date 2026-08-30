package proxy

import (
	"testing"

	"github.com/Urenzu/trace-portal/internal/trace"
)

func TestSummarizeRequest(t *testing.T) {
	body := []byte(`{
	  "model":"claude-opus-5","stream":true,"max_tokens":4096,
	  "system":[{"type":"text","text":"a"},{"type":"text","text":"b"}],
	  "messages":[{"role":"user","content":"hi"},{"role":"assistant","content":"yo"}],
	  "tools":[{"name":"bash"},{"name":"read"}]
	}`)

	var ev trace.Event
	summarizeRequest(body, &ev)

	if ev.Model != "claude-opus-5" || !ev.Stream || ev.MaxTokens != 4096 {
		t.Errorf("scalars: %+v", ev)
	}
	if ev.MessageCount != 2 {
		t.Errorf("MessageCount = %d, want 2", ev.MessageCount)
	}
	if ev.SystemBlocks != 2 {
		t.Errorf("SystemBlocks = %d, want 2", ev.SystemBlocks)
	}
	if got := ev.ToolsOffered; len(got) != 2 || got[0] != "bash" || got[1] != "read" {
		t.Errorf("ToolsOffered = %v", got)
	}
}

func TestSummarizeRequestStringSystem(t *testing.T) {
	var ev trace.Event
	summarizeRequest([]byte(`{"system":"you are helpful","messages":[]}`), &ev)
	if ev.SystemBlocks != 1 {
		t.Errorf("SystemBlocks = %d, want 1", ev.SystemBlocks)
	}
}

// A body we cannot parse must not corrupt the event; the proxied call still
// has to succeed, so parsing is strictly best effort.
func TestSummarizeRequestGarbage(t *testing.T) {
	ev := trace.Event{Model: "preset"}
	summarizeRequest([]byte("not json"), &ev)
	if ev.Model != "preset" {
		t.Errorf("garbage body mutated event: %+v", ev)
	}
}

func TestSessionKeyGroupsGrowingConversation(t *testing.T) {
	turn1 := []byte(`{"system":"S","messages":[{"role":"user","content":"first"}]}`)
	turn2 := []byte(`{"system":"S","messages":[{"role":"user","content":"first"},{"role":"assistant","content":"a"},{"role":"user","content":"second"}]}`)
	other := []byte(`{"system":"S","messages":[{"role":"user","content":"different"}]}`)

	if a, b := sessionKey(turn1), sessionKey(turn2); a != b {
		t.Errorf("turns of one conversation got different sessions: %s vs %s", a, b)
	}
	if a, b := sessionKey(turn1), sessionKey(other); a == b {
		t.Errorf("distinct conversations collided on %s", a)
	}
}

func TestSummarizeResponse(t *testing.T) {
	body := []byte(`{
	  "model":"claude-opus-5","stop_reason":"tool_use",
	  "content":[{"type":"text","text":"thinking"},{"type":"tool_use","id":"tu_1","name":"bash"}],
	  "usage":{"input_tokens":10,"output_tokens":5,"cache_read_input_tokens":900}
	}`)

	var ev trace.Event
	summarizeResponse(body, &ev)

	if ev.StopReason != "tool_use" {
		t.Errorf("StopReason = %q", ev.StopReason)
	}
	if ev.Usage == nil || ev.Usage.CacheReadInputTokens != 900 || ev.Usage.Total() != 915 {
		t.Errorf("Usage = %+v", ev.Usage)
	}
	if len(ev.ToolCalls) != 1 || ev.ToolCalls[0].Name != "bash" {
		t.Errorf("ToolCalls = %+v", ev.ToolCalls)
	}
}

func TestSummarizeSSE(t *testing.T) {
	stream := "event: message_start\n" +
		`data: {"type":"message_start","message":{"model":"claude-opus-5","usage":{"input_tokens":12,"output_tokens":1,"cache_creation_input_tokens":300,"cache_read_input_tokens":1200}}}` + "\n\n" +
		"event: content_block_start\n" +
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"tu_9","name":"read_file"}}` + "\n\n" +
		"event: content_block_delta\n" +
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"ignored"}}` + "\n\n" +
		"event: message_delta\n" +
		`data: {"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":57}}` + "\n\n" +
		"event: message_stop\n" +
		`data: {"type":"message_stop"}` + "\n\n"

	var ev trace.Event
	summarizeSSE([]byte(stream), &ev)

	if ev.Model != "claude-opus-5" {
		t.Errorf("Model = %q", ev.Model)
	}
	if ev.StopReason != "tool_use" {
		t.Errorf("StopReason = %q", ev.StopReason)
	}
	if ev.Usage == nil {
		t.Fatal("Usage not captured")
	}
	// message_delta restates output tokens; the final value must win, while
	// the input-side counters from message_start survive.
	if ev.Usage.OutputTokens != 57 {
		t.Errorf("OutputTokens = %d, want 57", ev.Usage.OutputTokens)
	}
	if ev.Usage.InputTokens != 12 || ev.Usage.CacheReadInputTokens != 1200 || ev.Usage.CacheCreationInputTokens != 300 {
		t.Errorf("input counters lost: %+v", ev.Usage)
	}
	if len(ev.ToolCalls) != 1 || ev.ToolCalls[0].Name != "read_file" {
		t.Errorf("ToolCalls = %+v", ev.ToolCalls)
	}
}

// A stream cut off mid-flight must still yield whatever was observed.
func TestSummarizeSSETruncated(t *testing.T) {
	stream := `data: {"type":"message_start","message":{"model":"m","usage":{"input_tokens":7}}}` + "\n\n" +
		`data: {"type":"content_bl`

	var ev trace.Event
	summarizeSSE([]byte(stream), &ev)

	if ev.Usage == nil || ev.Usage.InputTokens != 7 {
		t.Errorf("Usage = %+v", ev.Usage)
	}
}
