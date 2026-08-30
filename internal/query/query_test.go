package query

import (
	"math"
	"testing"
	"time"

	"github.com/Urenzu/trace-portal/internal/trace"
)

var base = time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)

func reqEvent(session, turn string, offset time.Duration) trace.Event {
	return trace.Event{
		Type: trace.EventRequest, Timestamp: base.Add(offset),
		SessionID: session, TurnID: turn,
		Model: "claude-opus-5", MessageCount: 2, SystemBlocks: 1,
		ToolsOffered: []string{"bash"},
	}
}

func respEvent(session, turn string, offset time.Duration, u trace.Usage, tools ...string) trace.Event {
	ev := trace.Event{
		Type: trace.EventResponse, Timestamp: base.Add(offset),
		SessionID: session, TurnID: turn,
		Model: "claude-opus-5", StatusCode: 200, StopReason: "end_turn",
		DurationMS: 1500, TTFBMS: 300, Usage: &u,
	}
	for _, name := range tools {
		ev.ToolCalls = append(ev.ToolCalls, trace.ToolCall{ID: "tu_" + name, Name: name})
	}
	return ev
}

func TestBuildTurnsPairsEvents(t *testing.T) {
	events := []trace.Event{
		reqEvent("s1", "t1", 0),
		respEvent("s1", "t1", time.Second, trace.Usage{InputTokens: 100, OutputTokens: 50}, "bash"),
	}

	turns := BuildTurns(events)
	if len(turns) != 1 {
		t.Fatalf("got %d turns, want 1", len(turns))
	}
	turn := turns[0]
	if turn.Pending {
		t.Error("completed turn still marked pending")
	}
	if turn.MessageCount != 2 || turn.SystemBlocks != 1 {
		t.Errorf("request fields lost: %+v", turn)
	}
	if turn.StopReason != "end_turn" || turn.DurationMS != 1500 {
		t.Errorf("response fields lost: %+v", turn)
	}
	if len(turn.ToolCalls) != 1 || turn.ToolCalls[0].Name != "bash" {
		t.Errorf("ToolCalls = %+v", turn.ToolCalls)
	}
	if !turn.Priced || turn.CostUSD <= 0 {
		t.Errorf("turn not priced: cost=%f priced=%v", turn.CostUSD, turn.Priced)
	}
}

// A request with no response is in flight (or was lost to a crash) and must be
// reported as pending rather than dropped.
func TestBuildTurnsMarksPending(t *testing.T) {
	turns := BuildTurns([]trace.Event{reqEvent("s1", "t1", 0)})
	if len(turns) != 1 || !turns[0].Pending {
		t.Fatalf("turns = %+v", turns)
	}
}

// Events for one turn can be interleaved with others in the log; pairing is by
// TurnID, not adjacency.
func TestBuildTurnsPairsOutOfOrder(t *testing.T) {
	events := []trace.Event{
		reqEvent("s1", "t1", 0),
		reqEvent("s1", "t2", time.Second),
		respEvent("s1", "t2", 2*time.Second, trace.Usage{InputTokens: 10}),
		respEvent("s1", "t1", 3*time.Second, trace.Usage{InputTokens: 20}),
	}

	turns := BuildTurns(events)
	if len(turns) != 2 {
		t.Fatalf("got %d turns, want 2", len(turns))
	}
	for _, turn := range turns {
		if turn.Pending {
			t.Errorf("turn %s left pending", turn.TurnID)
		}
	}
	// Turns come back in start order regardless of event order.
	if turns[0].TurnID != "t1" || turns[1].TurnID != "t2" {
		t.Errorf("order = %s, %s", turns[0].TurnID, turns[1].TurnID)
	}
}

func TestBuildSessionsRollsUp(t *testing.T) {
	events := []trace.Event{
		reqEvent("s1", "t1", 0),
		respEvent("s1", "t1", time.Second,
			trace.Usage{InputTokens: 100, OutputTokens: 50, CacheReadInputTokens: 900}, "bash"),
		reqEvent("s1", "t2", 10*time.Second),
		respEvent("s1", "t2", 11*time.Second,
			trace.Usage{InputTokens: 200, OutputTokens: 60, CacheReadInputTokens: 800}, "read", "bash"),
		reqEvent("s2", "t3", 20*time.Second),
		respEvent("s2", "t3", 21*time.Second, trace.Usage{InputTokens: 5}),
	}

	sessions := BuildSessions(events)
	if len(sessions) != 2 {
		t.Fatalf("got %d sessions, want 2", len(sessions))
	}

	// Most recently active first.
	if sessions[0].ID != "s2" {
		t.Errorf("sessions[0] = %s, want s2", sessions[0].ID)
	}

	var s1 Session
	for _, s := range sessions {
		if s.ID == "s1" {
			s1 = s
		}
	}
	if s1.Turns != 2 {
		t.Errorf("Turns = %d, want 2", s1.Turns)
	}
	if s1.Usage.InputTokens != 300 || s1.Usage.CacheReadInputTokens != 1700 {
		t.Errorf("Usage = %+v", s1.Usage)
	}
	if s1.ToolCalls != 3 {
		t.Errorf("ToolCalls = %d, want 3", s1.ToolCalls)
	}
	if got := s1.ToolNames; len(got) != 2 || got[0] != "bash" || got[1] != "read" {
		t.Errorf("ToolNames = %v, want sorted [bash read]", got)
	}
	// 1700 cached of 2000 total input tokens.
	if math.Abs(s1.CacheHitRate-0.85) > 1e-9 {
		t.Errorf("CacheHitRate = %f, want 0.85", s1.CacheHitRate)
	}
}

// The rate must not be diluted by output tokens, which are never cacheable.
func TestCacheHitRateIgnoresOutputTokens(t *testing.T) {
	u := trace.Usage{InputTokens: 100, CacheReadInputTokens: 900, OutputTokens: 100_000}
	if got := cacheHitRate(u); math.Abs(got-0.9) > 1e-9 {
		t.Errorf("cacheHitRate = %f, want 0.9", got)
	}
}

func TestBuildSessionDetail(t *testing.T) {
	events := []trace.Event{
		reqEvent("s1", "t1", 0),
		respEvent("s1", "t1", time.Second, trace.Usage{InputTokens: 10}),
		reqEvent("s2", "t2", time.Second),
		respEvent("s2", "t2", 2*time.Second, trace.Usage{InputTokens: 20}),
	}

	detail, ok := BuildSessionDetail(events, "s1")
	if !ok {
		t.Fatal("session s1 not found")
	}
	if len(detail.TurnList) != 1 || detail.TurnList[0].TurnID != "t1" {
		t.Errorf("TurnList = %+v", detail.TurnList)
	}
	if _, ok := BuildSessionDetail(events, "nope"); ok {
		t.Error("unknown session reported as found")
	}
}

func TestSessionCountsErrors(t *testing.T) {
	events := []trace.Event{
		reqEvent("s1", "t1", 0),
		{Type: trace.EventError, Timestamp: base.Add(time.Second), SessionID: "s1", TurnID: "t1",
			Error: "dial tcp: connection refused", DurationMS: 40},
	}

	sessions := BuildSessions(events)
	if len(sessions) != 1 || sessions[0].Errors != 1 {
		t.Fatalf("sessions = %+v", sessions)
	}
	turns := BuildTurns(events)
	if turns[0].Pending {
		t.Error("errored turn still marked pending")
	}
}

// The same call can be observed by a tailed agent log and by the proxy. They
// must collapse into one turn, each contributing what only it knows, rather
// than double-counting the cost.
func TestTurnsDedupeAcrossSourcesByMessageID(t *testing.T) {
	usage := trace.Usage{InputTokens: 100, OutputTokens: 50, CacheReadInputTokens: 900}

	fromProxy := trace.Event{
		Type: trace.EventResponse, Timestamp: base, SessionID: "hash-abc",
		TurnID: "proxy-local-id", MessageID: "msg_shared", Source: "proxy",
		Model: "claude-opus-5", StatusCode: 200, StopReason: "end_turn",
		DurationMS: 2000, TTFBMS: 300, Usage: &usage,
		ResponseBlob: "fdd19ee3503d72856fcac9be456cd8ac9a8bc1a7d5a5e27063bcfaea28e9b71d",
	}
	fromTranscript := trace.Event{
		Type: trace.EventResponse, Timestamp: base, SessionID: "real-session-uuid",
		TurnID: "msg_shared", MessageID: "msg_shared", Source: "claude-code",
		Model: "claude-opus-5", StatusCode: 200, StopReason: "end_turn",
		Usage: &usage, Project: "work", ProjectID: "abc123def456", GitBranch: "main", Effort: "high",
	}

	turns := BuildTurns([]trace.Event{fromProxy, fromTranscript})
	if len(turns) != 1 {
		t.Fatalf("got %d turns, want 1 — the same call was counted twice", len(turns))
	}

	turn := turns[0]
	// Cost must not double.
	single, _ := BuildTurns([]trace.Event{fromTranscript})[0].CostUSD, 0
	if math.Abs(turn.CostUSD-single) > 1e-12 {
		t.Errorf("cost doubled: merged=%.10f single=%.10f", turn.CostUSD, single)
	}
	if turn.Usage.InputTokens != 100 || turn.Usage.CacheReadInputTokens != 900 {
		t.Errorf("usage summed instead of merged: %+v", turn.Usage)
	}

	// Each source contributes what only it knows.
	if turn.Project != "work" || turn.GitBranch != "main" || turn.Effort != "high" {
		t.Errorf("transcript-only fields lost: %+v", turn)
	}
	if turn.ResponseBlob == "" {
		t.Errorf("proxy-only payload reference lost")
	}
	if turn.MessageID != "msg_shared" {
		t.Errorf("MessageID = %q", turn.MessageID)
	}
}

// Turns without a message id still pair by TurnID, so proxy request/response
// events and failures are unaffected.
func TestTurnsWithoutMessageIDStillPair(t *testing.T) {
	events := []trace.Event{
		reqEvent("s1", "t1", 0),
		respEvent("s1", "t1", time.Second, trace.Usage{InputTokens: 10}),
	}
	turns := BuildTurns(events)
	if len(turns) != 1 || turns[0].Pending {
		t.Fatalf("turns = %+v", turns)
	}
}

// Agent logs repeat a message once per content block, each copy carrying the
// full usage. Counting them separately would roughly double every total, so
// they must collapse to one turn whose usage is taken, not summed.
func TestRepeatedRecordsForOneMessageCollapse(t *testing.T) {
	usage := trace.Usage{
		InputTokens: 2, OutputTokens: 277,
		CacheCreationInputTokens: 1968, CacheReadInputTokens: 30173,
	}
	record := func(uuid string) trace.Event {
		return trace.Event{
			Type: trace.EventResponse, Timestamp: base, SessionID: "s1",
			TurnID: "msg_same", MessageID: "msg_same", Source: "claude-code",
			Model: "claude-opus-5", StatusCode: 200, StopReason: "tool_use",
			Usage: &usage,
		}
	}
	events := []trace.Event{record("a"), record("b"), record("c")}

	turns := BuildTurns(events)
	if len(turns) != 1 {
		t.Fatalf("got %d turns from one message, want 1", len(turns))
	}

	turn := turns[0]
	if turn.Usage.CacheReadInputTokens != 30173 {
		t.Errorf("cache reads = %d, want 30173 — usage was summed across duplicates",
			turn.Usage.CacheReadInputTokens)
	}
	if turn.Usage.OutputTokens != 277 {
		t.Errorf("output = %d, want 277", turn.Usage.OutputTokens)
	}

	// Cost must match a single observation of the message.
	single := BuildTurns([]trace.Event{record("a")})[0].CostUSD
	if math.Abs(turn.CostUSD-single) > 1e-12 {
		t.Errorf("cost inflated by duplicates: %.10f vs %.10f", turn.CostUSD, single)
	}
}
