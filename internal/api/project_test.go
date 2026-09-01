package api

import (
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/Urenzu/trace-portal/internal/trace"
)

// projectEvents builds one session's worth of turns in a named project and
// branch, priced so the costs are ordered by the caller's `read` figure.
func projectEvents(now time.Time, session, project, projectID, branch string, turns int, read int) []trace.Event {
	var events []trace.Event
	for i := 0; i < turns; i++ {
		ts := now.Add(time.Duration(i) * time.Second)
		turn := fmt.Sprintf("%s-t%d", session, i)
		events = append(events,
			trace.Event{Type: trace.EventRequest, Timestamp: ts,
				SessionID: session, TurnID: turn, Model: "claude-opus-5",
				Project: project, ProjectID: projectID, GitBranch: branch},
			trace.Event{Type: trace.EventResponse, Timestamp: ts.Add(time.Millisecond),
				SessionID: session, TurnID: turn, Model: "claude-opus-5",
				Project: project, ProjectID: projectID, GitBranch: branch,
				StatusCode: 200,
				Usage: &trace.Usage{
					InputTokens: 100, OutputTokens: 50, CacheReadInputTokens: read,
				},
				ToolCalls: []trace.ToolCall{{ID: "tu_" + turn, Name: "Bash"}}},
		)
	}
	return events
}

// The project page has to agree with the dashboard row it was reached from, and
// has to cut the same work by branch — which is the question a row cannot
// answer.
func TestProjectEndpoint(t *testing.T) {
	now := time.Now().UTC().Add(-time.Hour)

	var events []trace.Event
	events = append(events, projectEvents(now, "s-main", "alpha", "pid-alpha", "main", 3, 90_000)...)
	events = append(events, projectEvents(now.Add(time.Minute), "s-feat", "alpha", "pid-alpha", "feature/x", 2, 10)...)
	// A second project must not leak into the first one's figures.
	events = append(events, projectEvents(now.Add(2*time.Minute), "s-beta", "beta", "pid-beta", "main", 4, 500)...)

	h, _ := newTestServer(t, events...)

	var detail struct {
		Project  string `json:"project"`
		Found    bool   `json:"found"`
		Turns    int    `json:"turns"`
		Sessions int    `json:"sessions"`
		ByBranch []struct {
			Branch   string  `json:"branch"`
			Turns    int     `json:"turns"`
			Sessions int     `json:"sessions"`
			CostUSD  float64 `json:"cost_usd"`
			HitRate  float64 `json:"cache_hit_rate"`
		} `json:"by_branch"`
		ByDay []struct {
			Day   string `json:"day"`
			Turns int    `json:"turns"`
		} `json:"by_day"`
		Tools []struct {
			Name  string `json:"name"`
			Count int    `json:"count"`
		} `json:"tools"`
		Costliest *struct {
			ID string `json:"id"`
		} `json:"costliest_session"`
	}

	if code := get(t, h, "/api/projects/pid-alpha", &detail); code != http.StatusOK {
		t.Fatalf("status = %d", code)
	}
	if !detail.Found || detail.Project != "alpha" {
		t.Fatalf("detail = %+v, want a found project named alpha", detail)
	}
	if detail.Turns != 5 || detail.Sessions != 2 {
		t.Errorf("turns/sessions = %d/%d, want 5/2", detail.Turns, detail.Sessions)
	}

	// Branches are ranked by cost, and the cache-heavy one is cheaper per turn
	// despite having more turns — which is the whole point of the column.
	if len(detail.ByBranch) != 2 {
		t.Fatalf("branches = %+v, want two", detail.ByBranch)
	}
	byName := map[string]int{}
	for _, b := range detail.ByBranch {
		byName[b.Branch] = b.Turns
	}
	if byName["main"] != 3 || byName["feature/x"] != 2 {
		t.Errorf("branch turns = %v, want main:3 feature/x:2", byName)
	}
	for _, b := range detail.ByBranch {
		if b.Branch == "main" && !(b.HitRate > 0.9) {
			t.Errorf("main hit rate = %v, want the cache-heavy branch above 0.9", b.HitRate)
		}
		if b.Branch == "feature/x" && b.HitRate > 0.5 {
			t.Errorf("feature/x hit rate = %v, want the cache-poor branch low", b.HitRate)
		}
	}

	// Tool counts are per call, not per session: five turns each called Bash.
	if len(detail.Tools) != 1 || detail.Tools[0].Name != "Bash" || detail.Tools[0].Count != 5 {
		t.Errorf("tools = %+v, want Bash counted five times", detail.Tools)
	}

	// The series has to sum to the headline, or the chart contradicts the tile
	// directly above it.
	sum := 0
	for _, d := range detail.ByDay {
		sum += d.Turns
	}
	if sum != detail.Turns {
		t.Errorf("by_day turns = %d, want %d", sum, detail.Turns)
	}

	if detail.Costliest == nil || detail.Costliest.ID != "s-main" {
		t.Errorf("costliest = %+v, want s-main", detail.Costliest)
	}
}

// An unknown project is not an error: a link can outlive the window it was made
// in, and the page says so rather than failing.
func TestProjectEndpointUnknown(t *testing.T) {
	h, _ := newTestServer(t, sampleEvents(time.Now().UTC().Add(-time.Minute))...)

	var detail struct {
		Found bool `json:"found"`
		Turns int  `json:"turns"`
	}
	if code := get(t, h, "/api/projects/nope", &detail); code != http.StatusOK {
		t.Fatalf("status = %d", code)
	}
	if detail.Found || detail.Turns != 0 {
		t.Errorf("detail = %+v, want not found and empty", detail)
	}
}

// Ranking by cost cannot be answered by the backwards walk, so it takes its own
// path — and that path still has to page without repeating or dropping a row.
func TestSessionsRankedByCost(t *testing.T) {
	now := time.Now().UTC().Add(-time.Hour)

	var events []trace.Event
	// Cheapest first in time, so recency and cost disagree.
	events = append(events, projectEvents(now, "cheap", "alpha", "pid", "main", 1, 10)...)
	events = append(events, projectEvents(now.Add(time.Minute), "mid", "alpha", "pid", "main", 1, 5_000)...)
	events = append(events, projectEvents(now.Add(2*time.Minute), "dear", "alpha", "pid", "main", 1, 900_000)...)

	h, _ := newTestServer(t, events...)

	type page struct {
		Sessions []struct {
			ID      string  `json:"id"`
			CostUSD float64 `json:"cost_usd"`
		} `json:"sessions"`
		NextCursor string `json:"next_cursor"`
	}

	var first page
	if code := get(t, h, "/api/sessions?sort=cost&limit=2", &first); code != http.StatusOK {
		t.Fatalf("status = %d", code)
	}
	if len(first.Sessions) != 2 {
		t.Fatalf("sessions = %+v, want two", first.Sessions)
	}
	if first.Sessions[0].ID != "dear" || first.Sessions[1].ID != "mid" {
		t.Errorf("order = %s,%s, want dear,mid", first.Sessions[0].ID, first.Sessions[1].ID)
	}
	if first.NextCursor == "" {
		t.Fatal("want a cursor, one session is unpaged")
	}

	var second page
	if code := get(t, h, "/api/sessions?sort=cost&limit=2&cursor="+first.NextCursor, &second); code != http.StatusOK {
		t.Fatalf("status = %d", code)
	}
	if len(second.Sessions) != 1 || second.Sessions[0].ID != "cheap" {
		t.Errorf("page two = %+v, want the cheap session alone", second.Sessions)
	}
	if second.NextCursor != "" {
		t.Errorf("cursor = %q, want the listing exhausted", second.NextCursor)
	}
}

// An unrecognised sort is not an error either — it falls back to recency rather
// than rejecting a link someone typed by hand.
func TestSessionsUnknownSortFallsBackToRecent(t *testing.T) {
	now := time.Now().UTC().Add(-time.Hour)
	var events []trace.Event
	events = append(events, projectEvents(now, "older", "alpha", "pid", "main", 1, 900_000)...)
	events = append(events, projectEvents(now.Add(time.Minute), "newer", "alpha", "pid", "main", 1, 10)...)

	h, _ := newTestServer(t, events...)

	var body struct {
		Sessions []struct {
			ID string `json:"id"`
		} `json:"sessions"`
	}
	if code := get(t, h, "/api/sessions?sort=nonsense", &body); code != http.StatusOK {
		t.Fatalf("status = %d", code)
	}
	if len(body.Sessions) == 0 || body.Sessions[0].ID != "newer" {
		t.Errorf("sessions = %+v, want the newest first", body.Sessions)
	}
}

// The dashboard series has to reconcile with the totals beside it.
func TestStatsCarriesDailySeries(t *testing.T) {
	now := time.Now().UTC().Add(-time.Hour)
	h, _ := newTestServer(t, projectEvents(now, "s", "alpha", "pid", "main", 4, 1_000)...)

	var stats struct {
		Turns   int     `json:"turns"`
		CostUSD float64 `json:"cost_usd"`
		ByDay   []struct {
			Day     string  `json:"day"`
			Turns   int     `json:"turns"`
			CostUSD float64 `json:"cost_usd"`
		} `json:"by_day"`
	}
	if code := get(t, h, "/api/stats", &stats); code != http.StatusOK {
		t.Fatalf("status = %d", code)
	}
	if len(stats.ByDay) == 0 {
		t.Fatal("want a daily series")
	}

	turns, cost := 0, 0.0
	for _, d := range stats.ByDay {
		turns += d.Turns
		cost += d.CostUSD
	}
	if turns != stats.Turns {
		t.Errorf("series turns = %d, want %d", turns, stats.Turns)
	}
	if diff := cost - stats.CostUSD; diff > 1e-9 || diff < -1e-9 {
		t.Errorf("series cost = %v, want %v", cost, stats.CostUSD)
	}
}
