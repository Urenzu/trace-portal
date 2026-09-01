package api

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/Urenzu/trace-portal/internal/compact"
	"github.com/Urenzu/trace-portal/internal/store"
	"github.com/Urenzu/trace-portal/internal/trace"
)

func newTestServer(t *testing.T, events ...trace.Event) (http.Handler, *store.Store) {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(dir)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	for _, ev := range events {
		if err := st.Append(ev); err != nil {
			t.Fatalf("append: %v", err)
		}
	}
	c, err := compact.New(st, dir)
	if err != nil {
		t.Fatalf("new compactor: %v", err)
	}
	return New(st, c, nil, slog.New(slog.NewTextHandler(io.Discard, nil))).Handler(), st
}

func get(t *testing.T, h http.Handler, path string, into any) int {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	if into != nil && rec.Code == http.StatusOK {
		if err := json.Unmarshal(rec.Body.Bytes(), into); err != nil {
			t.Fatalf("decode %s: %v (body: %s)", path, err, rec.Body.String())
		}
	}
	return rec.Code
}

// Events land "now" so they fall inside the default window.
func sampleEvents(now time.Time) []trace.Event {
	return []trace.Event{
		{Type: trace.EventRequest, Timestamp: now, SessionID: "s1", TurnID: "t1",
			Model: "claude-opus-5", MessageCount: 2},
		{Type: trace.EventResponse, Timestamp: now.Add(time.Second), SessionID: "s1", TurnID: "t1",
			Model: "claude-opus-5", StatusCode: 200, StopReason: "tool_use", DurationMS: 900,
			Usage:     &trace.Usage{InputTokens: 1000, OutputTokens: 500, CacheReadInputTokens: 9000},
			ToolCalls: []trace.ToolCall{{ID: "tu_1", Name: "bash"}}},
	}
}

func TestSessionsEndpoint(t *testing.T) {
	now := time.Now().UTC().Add(-time.Minute)
	h, _ := newTestServer(t, sampleEvents(now)...)

	var body struct {
		Sessions []struct {
			ID           string  `json:"id"`
			Turns        int     `json:"turns"`
			CostUSD      float64 `json:"cost_usd"`
			CacheHitRate float64 `json:"cache_hit_rate"`
			ToolCalls    int     `json:"tool_calls"`
		} `json:"sessions"`
	}
	if code := get(t, h, "/api/sessions", &body); code != http.StatusOK {
		t.Fatalf("status = %d", code)
	}
	if len(body.Sessions) != 1 {
		t.Fatalf("got %d sessions, want 1", len(body.Sessions))
	}
	s := body.Sessions[0]
	if s.ID != "s1" || s.Turns != 1 || s.ToolCalls != 1 {
		t.Errorf("session = %+v", s)
	}
	if s.CostUSD <= 0 {
		t.Errorf("CostUSD = %f, want > 0", s.CostUSD)
	}
	if s.CacheHitRate <= 0.8 {
		t.Errorf("CacheHitRate = %f, want ~0.9", s.CacheHitRate)
	}
}

// An empty result must be [] rather than null so the frontend can treat the
// field as an array without a nil check.
func TestSessionsEmptyIsArray(t *testing.T) {
	h, _ := newTestServer(t)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/sessions", nil))

	var raw map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if string(raw["sessions"]) != "[]" {
		t.Errorf("sessions = %s, want []", raw["sessions"])
	}
}

func TestSessionDetailEndpoint(t *testing.T) {
	now := time.Now().UTC().Add(-time.Minute)
	h, _ := newTestServer(t, sampleEvents(now)...)

	var detail struct {
		ID    string `json:"id"`
		Turns []struct {
			TurnID     string `json:"turn_id"`
			StopReason string `json:"stop_reason"`
			Pending    bool   `json:"pending"`
		} `json:"turn_list"`
	}
	if code := get(t, h, "/api/sessions/s1", &detail); code != http.StatusOK {
		t.Fatalf("status = %d", code)
	}
	if detail.ID != "s1" || len(detail.Turns) != 1 {
		t.Fatalf("detail = %+v", detail)
	}
	if detail.Turns[0].StopReason != "tool_use" || detail.Turns[0].Pending {
		t.Errorf("turn = %+v", detail.Turns[0])
	}
}

func TestSessionDetailNotFound(t *testing.T) {
	h, _ := newTestServer(t)
	if code := get(t, h, "/api/sessions/nope", nil); code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", code)
	}
}

func TestStatsEndpoint(t *testing.T) {
	now := time.Now().UTC().Add(-time.Minute)
	h, _ := newTestServer(t, sampleEvents(now)...)

	var stats Stats
	if code := get(t, h, "/api/stats", &stats); code != http.StatusOK {
		t.Fatalf("status = %d", code)
	}
	if stats.Sessions != 1 || stats.Turns != 1 {
		t.Errorf("stats = %+v", stats)
	}
	if stats.ByModel["claude-opus-5"] != 1 {
		t.Errorf("ByModel = %v", stats.ByModel)
	}
	if stats.ToolCalls["bash"] != 1 {
		t.Errorf("ToolCalls = %v", stats.ToolCalls)
	}
	// 9000 cache reads at Opus 5 rates save 0.9x of $5/MTok.
	if want := 9000 * 5.0 / 1e6 * 0.9; stats.SavingsUSD < want*0.99 || stats.SavingsUSD > want*1.01 {
		t.Errorf("SavingsUSD = %f, want ~%f", stats.SavingsUSD, want)
	}
}

// Events outside the requested window must not appear.
func TestWindowFiltersOldEvents(t *testing.T) {
	old := time.Now().UTC().AddDate(0, 0, -30)
	h, _ := newTestServer(t, sampleEvents(old)...)

	var body struct {
		Sessions []json.RawMessage `json:"sessions"`
	}
	get(t, h, "/api/sessions", &body)
	if len(body.Sessions) != 0 {
		t.Errorf("default window returned %d old sessions", len(body.Sessions))
	}

	// Widening the window brings them back.
	get(t, h, "/api/sessions?days=60", &body)
	if len(body.Sessions) != 1 {
		t.Errorf("widened window returned %d sessions, want 1", len(body.Sessions))
	}
}

func TestBlobEndpoint(t *testing.T) {
	h, st := newTestServer(t)
	payload := []byte(`{"messages":[{"role":"user","content":"hello"}]}`)
	ref, err := st.PutBlob(payload)
	if err != nil {
		t.Fatalf("put blob: %v", err)
	}

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/blobs/"+ref, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if rec.Body.String() != string(payload) {
		t.Errorf("body = %s", rec.Body.String())
	}
}

func TestBlobEndpointRejectsBadRef(t *testing.T) {
	h, _ := newTestServer(t)
	if code := get(t, h, "/api/blobs/not-a-hash", nil); code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", code)
	}
}

func TestHealthEndpoint(t *testing.T) {
	now := time.Now().UTC()
	h, _ := newTestServer(t, sampleEvents(now)...)

	var health struct {
		Status       string `json:"status"`
		DaysCaptured int    `json:"days_captured"`
		Days         []struct {
			Day     string  `json:"day"`
			Turns   int     `json:"turns"`
			CostUSD float64 `json:"cost_usd"`
		} `json:"days"`
	}
	if code := get(t, h, "/api/health", &health); code != http.StatusOK {
		t.Fatalf("status = %d", code)
	}
	if health.Status != "ok" || health.DaysCaptured != 1 {
		t.Errorf("health = %+v", health)
	}

	// The per-day breakdown is what lets the UI show which days hold anything
	// rather than presenting a first and last date as continuous coverage.
	if len(health.Days) != 1 {
		t.Fatalf("days = %+v, want one entry", health.Days)
	}
	if got, want := health.Days[0].Day, now.Format("2006-01-02"); got != want {
		t.Errorf("day = %q, want %q", got, want)
	}
	if health.Days[0].Turns == 0 {
		t.Errorf("day turns = 0, want the sample events counted")
	}
}

// The sessions endpoint pages: a limit caps the result and hands back a cursor
// that fetches the next page without repeating or skipping anything.
func TestSessionsEndpointPaginates(t *testing.T) {
	now := time.Now().UTC().Add(-time.Hour)

	var events []trace.Event
	for i := 0; i < 5; i++ {
		ts := now.Add(time.Duration(i) * time.Minute)
		session := fmt.Sprintf("sess-%d", i)
		events = append(events,
			trace.Event{Type: trace.EventRequest, Timestamp: ts,
				SessionID: session, TurnID: fmt.Sprintf("t%d", i), Model: "claude-opus-5"},
			trace.Event{Type: trace.EventResponse, Timestamp: ts.Add(time.Second),
				SessionID: session, TurnID: fmt.Sprintf("t%d", i), Model: "claude-opus-5",
				StatusCode: 200, DurationMS: 500,
				Usage: &trace.Usage{InputTokens: 100, OutputTokens: 20}},
		)
	}
	h, _ := newTestServer(t, events...)

	type pageResp struct {
		Sessions []struct {
			ID string `json:"id"`
		} `json:"sessions"`
		NextCursor  string `json:"next_cursor"`
		DaysScanned int    `json:"days_scanned"`
	}

	seen := map[string]bool{}
	cursor := ""
	pages := 0
	for {
		pages++
		if pages > 10 {
			t.Fatal("pagination did not terminate")
		}
		path := "/api/sessions?limit=2"
		if cursor != "" {
			path += "&cursor=" + url.QueryEscape(cursor)
		}

		var page pageResp
		if code := get(t, h, path, &page); code != http.StatusOK {
			t.Fatalf("status = %d", code)
		}
		if len(page.Sessions) > 2 {
			t.Fatalf("page returned %d sessions, limit was 2", len(page.Sessions))
		}
		for _, s := range page.Sessions {
			if seen[s.ID] {
				t.Errorf("session %s appeared on two pages", s.ID)
			}
			seen[s.ID] = true
		}
		if page.NextCursor == "" {
			break
		}
		cursor = page.NextCursor
	}

	if len(seen) != 5 {
		t.Errorf("paged through %d sessions, want 5", len(seen))
	}
	if pages < 3 {
		t.Errorf("expected at least 3 pages at limit=2, got %d", pages)
	}
}

func TestSessionsEndpointRejectsBadCursor(t *testing.T) {
	h, _ := newTestServer(t)
	if code := get(t, h, "/api/sessions?cursor=%21%21%21", nil); code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", code)
	}
}
