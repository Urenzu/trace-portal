package source

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Urenzu/trace-portal/internal/query"
	"github.com/Urenzu/trace-portal/internal/trace"
)

// The shapes below are copied from a real ~/.claude transcript, including the
// parts that make a naive parser wrong: one record per content block all
// sharing a message id and all repeating the usage, a tool result arriving in a
// later user record with no message id of its own, and a result whose content
// is an array of blocks rather than a string.
const contentTranscript = `
{"type":"assistant","timestamp":"2026-08-29T02:46:40.000Z","sessionId":"s1","uuid":"u1","cwd":"/tmp/proj","version":"2.1.246","message":{"id":"msg_1","model":"claude-opus-5","content":[{"type":"thinking","thinking":"I should list the directory first.","signature":"CAISuQMKpgEIERgCKkBikC0iMli95"}],"usage":{"input_tokens":1000,"output_tokens":50}}}
{"type":"assistant","timestamp":"2026-08-29T02:46:41.000Z","sessionId":"s1","uuid":"u2","cwd":"/tmp/proj","version":"2.1.246","message":{"id":"msg_1","model":"claude-opus-5","content":[{"type":"text","text":"Listing the project now."}],"usage":{"input_tokens":1000,"output_tokens":50}}}
{"type":"assistant","timestamp":"2026-08-29T02:46:42.000Z","sessionId":"s1","uuid":"u3","cwd":"/tmp/proj","version":"2.1.246","message":{"id":"msg_1","model":"claude-opus-5","content":[{"type":"tool_use","id":"toolu_1","name":"Bash","input":{"command":"ls -la","description":"List project directory"}}],"usage":{"input_tokens":1000,"output_tokens":50}}}
{"type":"user","timestamp":"2026-08-29T02:46:47.000Z","sessionId":"s1","uuid":"u4","sourceToolAssistantUUID":"u3","message":{"role":"user","content":[{"tool_use_id":"toolu_1","type":"tool_result","content":"total 12\nREADME.md","is_error":false}]},"toolUseResult":{"stdout":"total 12\nREADME.md"}}
{"type":"user","timestamp":"2026-08-29T02:46:48.000Z","sessionId":"s1","uuid":"u5","message":{"role":"user","content":[{"tool_use_id":"toolu_2","type":"tool_result","content":[{"type":"text","text":"file not found"}],"is_error":true}]}}
{"type":"user","timestamp":"2026-08-29T02:46:49.000Z","sessionId":"s1","uuid":"u6","message":{"role":"user","content":"an ordinary typed prompt"}}
`

func parseTranscript(t *testing.T, body string, configure func(*ClaudeCode)) []trace.Event {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "session.jsonl")
	if err := os.WriteFile(path, []byte(strings.TrimLeft(body, "\n")), 0o600); err != nil {
		t.Fatalf("write transcript: %v", err)
	}
	cc := NewClaudeCode(dir)
	configure(cc)

	var events []trace.Event
	if _, err := cc.Parse(path, 0, func(ev trace.Event) error {
		events = append(events, ev)
		return nil
	}); err != nil {
		t.Fatalf("parse: %v", err)
	}
	return events
}

func decodeContent(t *testing.T, ev trace.Event) trace.Content {
	t.Helper()
	var c trace.Content
	if err := json.Unmarshal(ev.Content, &c); err != nil {
		t.Fatalf("decode content: %v", err)
	}
	return c
}

func TestCaptureIsOffUnlessAskedFor(t *testing.T) {
	events := parseTranscript(t, contentTranscript, func(*ClaudeCode) {})
	for _, ev := range events {
		if ev.Type == trace.EventContent || len(ev.Content) > 0 {
			t.Fatalf("captured content without being asked: %+v", ev)
		}
	}
	if len(events) != 3 {
		t.Fatalf("got %d measurement events, want 3", len(events))
	}
}

func TestCaptureRecordsWhatWasSaidAndDone(t *testing.T) {
	events := parseTranscript(t, contentTranscript, func(cc *ClaudeCode) {
		cc.CaptureContent(true, 0)
	})

	byKind := map[trace.ContentKind]trace.Content{}
	for _, ev := range events {
		if ev.Type != trace.EventContent {
			continue
		}
		c := decodeContent(t, ev)
		if _, dup := byKind[c.Kind]; dup && c.Kind != trace.ContentToolResult {
			t.Fatalf("two %s blocks captured, want one", c.Kind)
		}
		if c.Kind != trace.ContentToolResult {
			byKind[c.Kind] = c
		}
	}

	if got := byKind[trace.ContentText].Text; got != "Listing the project now." {
		t.Fatalf("assistant text captured as %q", got)
	}
	thinking := byKind[trace.ContentThinking].Text
	if thinking != "I should list the directory first." {
		t.Fatalf("thinking captured as %q", thinking)
	}
	// The signature is a kilobyte of base64 per block that nothing verifies.
	if strings.Contains(thinking, "CAISuQMK") {
		t.Fatal("kept the thinking signature")
	}

	call := byKind[trace.ContentToolUse]
	if call.Name != "Bash" || call.Tool != "toolu_1" {
		t.Fatalf("tool call captured as %+v", call)
	}
	if !strings.Contains(string(call.Input), `"ls -la"`) {
		t.Fatalf("tool arguments captured as %s", call.Input)
	}
}

func TestToolResultsAreCapturedAndPairedWithTheirCall(t *testing.T) {
	events := parseTranscript(t, contentTranscript, func(cc *ClaudeCode) {
		cc.CaptureContent(true, 0)
	})

	var results []trace.Event
	for _, ev := range events {
		if ev.ContentKind == trace.ContentToolResult {
			results = append(results, ev)
		}
	}
	if len(results) != 2 {
		t.Fatalf("captured %d tool results, want 2", len(results))
	}

	first := decodeContent(t, results[0])
	if !strings.Contains(first.Text, "README.md") {
		t.Fatalf("result text captured as %q", first.Text)
	}
	// The second result's content is an array of blocks, not a string. Typing
	// it as a string is what makes a naive parser drop half the results.
	second := decodeContent(t, results[1])
	if second.Text != "file not found" || !second.IsError {
		t.Fatalf("block-shaped result captured as %+v", second)
	}

	// A tool result carries no message id, because the API never issued one for
	// it. It reaches its turn by the tool_use id instead.
	for i := range events {
		if events[i].Type == trace.EventContent {
			events[i].ContentBlob = "ref-" + events[i].ToolUseID + events[i].TurnID
		}
	}
	turns := query.BuildTurns(events)
	if len(turns) != 1 {
		t.Fatalf("built %d turns, want 1 — a tool result must not become a turn", len(turns))
	}
	found := false
	for _, ref := range turns[0].ContentBlobs {
		if ref == "ref-toolu_1u4" {
			found = true
		}
		if ref == "ref-toolu_2u5" {
			t.Fatal("attached a result whose call is not in this turn")
		}
	}
	if !found {
		t.Fatalf("the tool result never reached its turn: %v", turns[0].ContentBlobs)
	}
	if len(turns[0].ToolCalls) != 1 || turns[0].ToolCalls[0].ID != "toolu_1" {
		t.Fatalf("tool calls came out as %+v", turns[0].ToolCalls)
	}
}

func TestCaptureTruncatesAndSaysSo(t *testing.T) {
	big := strings.Repeat("x", 4096)
	body := `{"type":"assistant","timestamp":"2026-08-29T02:46:41.000Z","sessionId":"s1","uuid":"u1","version":"2.1.246","message":{"id":"msg_1","model":"claude-opus-5","content":[{"type":"text","text":"` + big + `"}],"usage":{"input_tokens":10,"output_tokens":10}}}` + "\n"
	events := parseTranscript(t, body, func(cc *ClaudeCode) { cc.CaptureContent(true, 100) })

	for _, ev := range events {
		if ev.Type != trace.EventContent {
			continue
		}
		c := decodeContent(t, ev)
		if len(c.Text) != 100 {
			t.Fatalf("kept %d bytes, want the 100-byte limit", len(c.Text))
		}
		if !c.Truncated || c.Bytes != 4096 {
			// Truncated output read as complete is a confident wrong answer
			// about what the agent actually saw.
			t.Fatalf("truncation not recorded: %+v", c)
		}
		return
	}
	t.Fatal("no content event emitted")
}
