package compact

import (
	"bytes"
	"context"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Urenzu/trace-portal/internal/query"
	"github.com/Urenzu/trace-portal/internal/store"
	"github.com/Urenzu/trace-portal/internal/trace"
)

// yesterday is the most recent day compaction will touch; today is still being
// appended to and is deliberately skipped.
func yesterday() time.Time {
	return truncateDay(time.Now().UTC()).AddDate(0, 0, -1)
}

func newCompactor(t *testing.T, events ...trace.Event) (*Compactor, *store.Store) {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(dir)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	for _, ev := range events {
		if err := st.Append(context.Background(), ev); err != nil {
			t.Fatalf("append: %v", err)
		}
	}

	c, err := New(st, dir)
	if err != nil {
		t.Fatalf("new compactor: %v", err)
	}
	return c, st
}

func dayEvents(day time.Time, session, turn, model string, u trace.Usage, tools ...string) []trace.Event {
	req := trace.Event{
		Type: trace.EventRequest, Timestamp: day.Add(3 * time.Hour),
		SessionID: session, TurnID: turn, Model: model, Stream: true,
		MessageCount: 6, SystemBlocks: 2,
		ToolsOffered: []string{"bash", "read_file"},
		RequestBlob:  "995d8f73806444b693d0e939cb5b2be06f3c8b54a085a06020d5e6c1c5dac6bb",
	}
	resp := trace.Event{
		Type: trace.EventResponse, Timestamp: day.Add(3*time.Hour + 2*time.Second),
		SessionID: session, TurnID: turn, Model: model,
		StatusCode: 200, StopReason: "tool_use", DurationMS: 2000, TTFBMS: 250,
		Usage:        &u,
		ResponseBlob: "fdd19ee3503d72856fcac9be456cd8ac9a8bc1a7d5a5e27063bcfaea28e9b71d",
	}
	for _, name := range tools {
		resp.ToolCalls = append(resp.ToolCalls, trace.ToolCall{ID: "tu_" + name, Name: name})
	}
	return []trace.Event{req, resp}
}

func TestCompactRoundTrip(t *testing.T) {
	day := yesterday()
	usage := trace.Usage{
		InputTokens: 1200, OutputTokens: 400,
		CacheCreationInputTokens: 2000, CacheReadInputTokens: 20000,
		CacheCreation: &trace.CacheCreation{Ephemeral5mInputTokens: 1500, Ephemeral1hInputTokens: 500},
	}
	c, _ := newCompactor(t, dayEvents(day, "s1", "t1", "claude-opus-5", usage, "bash", "grep")...)

	wrote, err := c.CompactDay(day, false)
	if err != nil {
		t.Fatalf("compact: %v", err)
	}
	if !wrote {
		t.Fatal("expected a partition to be written")
	}
	if !c.IsCompacted(day) {
		t.Fatal("day not reported as compacted")
	}

	turns, ok, err := c.ReadTurns(day)
	if err != nil || !ok {
		t.Fatalf("read turns: ok=%v err=%v", ok, err)
	}
	if len(turns) != 1 {
		t.Fatalf("got %d turns, want 1", len(turns))
	}

	got := turns[0]
	if got.TurnID != "t1" || got.SessionID != "s1" || got.Model != "claude-opus-5" {
		t.Errorf("identity fields: %+v", got)
	}
	if !got.Stream || got.StopReason != "tool_use" || got.StatusCode != 200 {
		t.Errorf("response fields: %+v", got)
	}
	if got.DurationMS != 2000 || got.TTFBMS != 250 {
		t.Errorf("timings: dur=%d ttfb=%d", got.DurationMS, got.TTFBMS)
	}
	if got.MessageCount != 6 || got.SystemBlocks != 2 {
		t.Errorf("context composition: %+v", got)
	}

	// The cache TTL split must survive the round trip — it is what makes the
	// cost correct.
	w5, w1h := got.Usage.CacheWrites()
	if w5 != 1500 || w1h != 500 {
		t.Errorf("cache writes = (%d, %d), want (1500, 500)", w5, w1h)
	}
	if got.Usage.InputTokens != 1200 || got.Usage.CacheReadInputTokens != 20000 {
		t.Errorf("usage = %+v", got.Usage)
	}

	if len(got.ToolCalls) != 2 || got.ToolCalls[0].Name != "bash" || got.ToolCalls[1].Name != "grep" {
		t.Errorf("tool calls = %+v", got.ToolCalls)
	}
	if len(got.ToolsOffered) != 2 {
		t.Errorf("tools offered = %v", got.ToolsOffered)
	}
	if got.RequestBlob == "" || got.ResponseBlob == "" {
		t.Errorf("blob refs lost: %q %q", got.RequestBlob, got.ResponseBlob)
	}

	if got.CostUSD <= 0 || !got.Priced {
		t.Errorf("cost = %f priced=%v", got.CostUSD, got.Priced)
	}
}

// The compacted and raw paths must produce identical turns, otherwise the UI
// would show different numbers depending on whether a day happened to be
// compacted yet.
func TestCompactedMatchesRaw(t *testing.T) {
	day := yesterday()
	usage := trace.Usage{
		InputTokens: 900, OutputTokens: 300,
		CacheCreationInputTokens: 1000, CacheReadInputTokens: 15000,
		CacheCreation: &trace.CacheCreation{Ephemeral5mInputTokens: 1000},
	}
	events := dayEvents(day, "s1", "t1", "claude-sonnet-5", usage, "bash")
	c, st := newCompactor(t, events...)

	raw, err := st.Events(context.Background(), day)
	if err != nil {
		t.Fatal(err)
	}
	rawTurns := query.BuildTurns(raw)

	if _, err := c.CompactDay(day, false); err != nil {
		t.Fatalf("compact: %v", err)
	}
	compacted, _, err := c.ReadTurns(day)
	if err != nil {
		t.Fatal(err)
	}

	if len(rawTurns) != len(compacted) {
		t.Fatalf("turn counts differ: raw=%d compacted=%d", len(rawTurns), len(compacted))
	}
	r, cm := rawTurns[0], compacted[0]
	if math.Abs(r.CostUSD-cm.CostUSD) > 1e-12 {
		t.Errorf("cost differs: raw=%.12f compacted=%.12f", r.CostUSD, cm.CostUSD)
	}
	if r.Usage.InputTokens != cm.Usage.InputTokens ||
		r.Usage.CacheReadInputTokens != cm.Usage.CacheReadInputTokens {
		t.Errorf("usage differs: raw=%+v compacted=%+v", r.Usage, cm.Usage)
	}
	if !r.StartedAt.Equal(cm.StartedAt) {
		t.Errorf("timestamp differs: raw=%v compacted=%v", r.StartedAt, cm.StartedAt)
	}
	if r.StopReason != cm.StopReason || r.Model != cm.Model {
		t.Errorf("fields differ: raw=%+v compacted=%+v", r, cm)
	}
}

func TestRollup(t *testing.T) {
	day := yesterday()
	var events []trace.Event
	events = append(events, dayEvents(day, "s1", "t1", "claude-opus-5",
		trace.Usage{InputTokens: 100, OutputTokens: 50, CacheReadInputTokens: 900}, "bash")...)
	events = append(events, dayEvents(day, "s1", "t2", "claude-opus-5",
		trace.Usage{InputTokens: 200, OutputTokens: 60, CacheReadInputTokens: 800}, "bash", "grep")...)
	events = append(events, dayEvents(day, "s2", "t3", "claude-sonnet-5",
		trace.Usage{InputTokens: 300, OutputTokens: 70}, "read_file")...)

	c, _ := newCompactor(t, events...)
	if _, err := c.CompactDay(day, false); err != nil {
		t.Fatalf("compact: %v", err)
	}

	row, ok, err := c.ReadDay(day)
	if err != nil || !ok {
		t.Fatalf("read day: ok=%v err=%v", ok, err)
	}
	if row.Turns != 3 || row.Sessions != 2 {
		t.Errorf("day row = %+v, want 3 turns / 2 sessions", row)
	}
	if row.InputTokens != 600 || row.OutputTokens != 180 || row.CacheRead != 1700 {
		t.Errorf("day tokens = %+v", row)
	}
	if row.CostUSD <= 0 || row.SavingsUSD <= 0 {
		t.Errorf("cost=%f savings=%f", row.CostUSD, row.SavingsUSD)
	}

	models, err := c.ReadModels(day)
	if err != nil {
		t.Fatal(err)
	}
	byModel := map[string]int64{}
	for _, m := range models {
		byModel[m.Model] = m.Turns
	}
	if byModel["claude-opus-5"] != 2 || byModel["claude-sonnet-5"] != 1 {
		t.Errorf("by model = %v", byModel)
	}

	tools, err := c.ReadTools(day)
	if err != nil {
		t.Fatal(err)
	}
	byTool := map[string]int64{}
	for _, tl := range tools {
		byTool[tl.Tool] = tl.Calls
	}
	if byTool["bash"] != 2 || byTool["grep"] != 1 || byTool["read_file"] != 1 {
		t.Errorf("by tool = %v", byTool)
	}
}

// Today's JSONL is still being appended to, so compacting it would freeze a
// partial day into a partition that later reads as complete.
func TestCompactSkipsToday(t *testing.T) {
	today := truncateDay(time.Now().UTC())
	c, _ := newCompactor(t, dayEvents(today, "s1", "t1", "claude-opus-5",
		trace.Usage{InputTokens: 10})...)

	wrote, err := c.CompactDay(today, false)
	if err != nil {
		t.Fatalf("compact: %v", err)
	}
	if wrote {
		t.Error("compacted today, which is still being written")
	}
	if c.IsCompacted(today) {
		t.Error("today reported as compacted")
	}
}

func TestCompactIsIdempotent(t *testing.T) {
	day := yesterday()
	c, _ := newCompactor(t, dayEvents(day, "s1", "t1", "claude-opus-5",
		trace.Usage{InputTokens: 10})...)

	first, err := c.CompactDay(day, false)
	if err != nil || !first {
		t.Fatalf("first compact: wrote=%v err=%v", first, err)
	}
	second, err := c.CompactDay(day, false)
	if err != nil {
		t.Fatalf("second compact: %v", err)
	}
	if second {
		t.Error("recompacted an already-compacted day")
	}
}

// The read path must transparently mix compacted days with the uncompacted
// current day.
func TestTurnsRangeMixesSources(t *testing.T) {
	day := yesterday()
	today := truncateDay(time.Now().UTC())

	var events []trace.Event
	events = append(events, dayEvents(day, "s1", "t1", "claude-opus-5",
		trace.Usage{InputTokens: 100})...)
	events = append(events, dayEvents(today, "s2", "t2", "claude-opus-5",
		trace.Usage{InputTokens: 200})...)

	c, _ := newCompactor(t, events...)
	if _, err := c.CompactDay(day, false); err != nil {
		t.Fatalf("compact: %v", err)
	}
	if !c.IsCompacted(day) || c.IsCompacted(today) {
		t.Fatalf("setup: yesterday compacted=%v today compacted=%v",
			c.IsCompacted(day), c.IsCompacted(today))
	}

	turns, err := c.TurnsRange(day, today.Add(23*time.Hour))
	if err != nil {
		t.Fatalf("turns range: %v", err)
	}
	if len(turns) != 2 {
		t.Fatalf("got %d turns, want 2 (one compacted, one raw)", len(turns))
	}
	if turns[0].TurnID != "t1" || turns[1].TurnID != "t2" {
		t.Errorf("turns out of order: %s, %s", turns[0].TurnID, turns[1].TurnID)
	}
}

func TestAggregateRangeUsesRollups(t *testing.T) {
	day := yesterday()
	var events []trace.Event
	events = append(events, dayEvents(day, "s1", "t1", "claude-opus-5",
		trace.Usage{InputTokens: 100, OutputTokens: 50, CacheReadInputTokens: 900}, "bash")...)
	events = append(events, dayEvents(day, "s2", "t2", "claude-sonnet-5",
		trace.Usage{InputTokens: 200, OutputTokens: 60}, "grep")...)

	c, _ := newCompactor(t, events...)

	// The window must cover the whole UTC day, or the rollup cannot answer for
	// it and this test would silently compare the raw path against itself.
	from := day
	to := day.Add(24 * time.Hour)
	rawAgg, err := c.AggregateRange(from, to)
	if err != nil {
		t.Fatalf("aggregate raw: %v", err)
	}

	if _, err := c.CompactDay(day, false); err != nil {
		t.Fatalf("compact: %v", err)
	}
	rollupAgg, err := c.AggregateRange(from, to)
	if err != nil {
		t.Fatalf("aggregate rollup: %v", err)
	}

	// The rollup path must agree with the raw path on every total.
	if rawAgg.Turns != rollupAgg.Turns {
		t.Errorf("turns: raw=%d rollup=%d", rawAgg.Turns, rollupAgg.Turns)
	}
	if rawAgg.Usage.InputTokens != rollupAgg.Usage.InputTokens ||
		rawAgg.Usage.CacheReadInputTokens != rollupAgg.Usage.CacheReadInputTokens {
		t.Errorf("usage: raw=%+v rollup=%+v", rawAgg.Usage, rollupAgg.Usage)
	}
	if math.Abs(rawAgg.CostUSD-rollupAgg.CostUSD) > 1e-12 {
		t.Errorf("cost: raw=%.12f rollup=%.12f", rawAgg.CostUSD, rollupAgg.CostUSD)
	}
	if math.Abs(rawAgg.SavingsUSD-rollupAgg.SavingsUSD) > 1e-12 {
		t.Errorf("savings: raw=%.12f rollup=%.12f", rawAgg.SavingsUSD, rollupAgg.SavingsUSD)
	}
	if rawAgg.ByModel["claude-opus-5"] != rollupAgg.ByModel["claude-opus-5"] {
		t.Errorf("by model: raw=%v rollup=%v", rawAgg.ByModel, rollupAgg.ByModel)
	}
	if rawAgg.ToolCalls["bash"] != rollupAgg.ToolCalls["bash"] {
		t.Errorf("tool calls: raw=%v rollup=%v", rawAgg.ToolCalls, rollupAgg.ToolCalls)
	}
	if rawAgg.Sessions != rollupAgg.Sessions {
		t.Errorf("sessions: raw=%d rollup=%d", rawAgg.Sessions, rollupAgg.Sessions)
	}

	// Guard against this test passing while both paths read raw JSONL:
	// SessionsExact is only cleared when a rollup actually answered a day.
	if !rawAgg.SessionsExact {
		t.Error("pre-compaction aggregate claims to have used a rollup")
	}
	if rollupAgg.SessionsExact {
		t.Fatal("post-compaction aggregate did not use the rollup path")
	}
}

func TestCompactAll(t *testing.T) {
	d1 := yesterday().AddDate(0, 0, -2)
	d2 := yesterday()
	var events []trace.Event
	events = append(events, dayEvents(d1, "s1", "t1", "claude-opus-5", trace.Usage{InputTokens: 10})...)
	events = append(events, dayEvents(d2, "s2", "t2", "claude-opus-5", trace.Usage{InputTokens: 20})...)

	c, _ := newCompactor(t, events...)
	n, err := c.CompactAll()
	if err != nil {
		t.Fatalf("compact all: %v", err)
	}
	if n != 2 {
		t.Errorf("compacted %d days, want 2", n)
	}

	again, err := c.CompactAll()
	if err != nil {
		t.Fatalf("second compact all: %v", err)
	}
	if again != 0 {
		t.Errorf("recompacted %d days, want 0", again)
	}
}

// The consolidated index must answer identically to the per-day partitions it
// was built from, and must be what a wide query actually reads.
func TestConsolidatedIndex(t *testing.T) {
	const days = 4
	end := truncateDay(time.Now().UTC()).AddDate(0, 0, -1)
	start := end.AddDate(0, 0, -days+1)

	var events []trace.Event
	for d := 0; d < days; d++ {
		day := start.AddDate(0, 0, d)
		events = append(events, dayEvents(day, "s"+day.Format("02"), "t"+day.Format("02"),
			"claude-opus-5", trace.Usage{InputTokens: 100, OutputTokens: 20, CacheReadInputTokens: 900}, "bash")...)
	}

	c, _ := newCompactor(t, events...)
	n, err := c.CompactAll()
	if err != nil {
		t.Fatalf("compact all: %v", err)
	}
	if n != days {
		t.Fatalf("compacted %d days, want %d", n, days)
	}

	idx, err := c.loadIndex()
	if err != nil {
		t.Fatalf("load index: %v", err)
	}
	if idx == nil {
		t.Fatal("no consolidated index was written")
	}
	if len(idx.days) != days {
		t.Errorf("index covers %d days, want %d", len(idx.days), days)
	}

	// Every indexed day must match its partition exactly.
	for d := 0; d < days; d++ {
		day := start.AddDate(0, 0, d)
		partition, ok, err := c.ReadDay(day)
		if err != nil || !ok {
			t.Fatalf("read partition day: ok=%v err=%v", ok, err)
		}
		indexed := idx.days[day.Format("2006-01-02")]
		if indexed != partition {
			t.Errorf("%s: index=%+v partition=%+v", day.Format("2006-01-02"), indexed, partition)
		}
	}

	agg, err := c.AggregateRange(start, end.Add(24*time.Hour))
	if err != nil {
		t.Fatalf("aggregate: %v", err)
	}
	if agg.Turns != days {
		t.Errorf("turns = %d, want %d", agg.Turns, days)
	}
	if agg.ToolCalls["bash"] != days {
		t.Errorf("tool calls = %v", agg.ToolCalls)
	}
	if agg.SessionsExact {
		t.Error("wide query did not use the rollup path")
	}
}

// The narrow session projection must agree with the full turn read on every
// field a session summary is built from.
func TestSessionProjectionMatchesFullRead(t *testing.T) {
	day := yesterday()
	var events []trace.Event
	events = append(events, dayEvents(day, "s1", "t1", "claude-opus-5",
		trace.Usage{InputTokens: 100, OutputTokens: 50, CacheReadInputTokens: 900,
			CacheCreationInputTokens: 300,
			CacheCreation:            &trace.CacheCreation{Ephemeral5mInputTokens: 200, Ephemeral1hInputTokens: 100}}, "bash", "grep")...)
	events = append(events, dayEvents(day, "s2", "t2", "claude-sonnet-5",
		trace.Usage{InputTokens: 200, OutputTokens: 60}, "read_file")...)

	c, _ := newCompactor(t, events...)
	if _, err := c.CompactDay(day, false); err != nil {
		t.Fatalf("compact: %v", err)
	}

	from, to := day, day.Add(24*time.Hour)

	full, err := c.TurnsRange(from, to)
	if err != nil {
		t.Fatal(err)
	}
	fromFull := query.SessionsFromTurns(full)

	projected, err := c.SessionsRange(from, to)
	if err != nil {
		t.Fatal(err)
	}

	if len(fromFull) != len(projected) {
		t.Fatalf("session counts differ: full=%d projected=%d", len(fromFull), len(projected))
	}
	byID := map[string]query.Session{}
	for _, s := range projected {
		byID[s.ID] = s
	}
	for _, want := range fromFull {
		got, ok := byID[want.ID]
		if !ok {
			t.Errorf("session %s missing from projection", want.ID)
			continue
		}
		if math.Abs(got.CostUSD-want.CostUSD) > 1e-12 {
			t.Errorf("%s cost: full=%.12f projected=%.12f", want.ID, want.CostUSD, got.CostUSD)
		}
		if got.Usage != want.Usage && got.Usage.InputTokens != want.Usage.InputTokens {
			t.Errorf("%s usage: full=%+v projected=%+v", want.ID, want.Usage, got.Usage)
		}
		if math.Abs(got.CacheHitRate-want.CacheHitRate) > 1e-12 {
			t.Errorf("%s cache hit: full=%f projected=%f", want.ID, want.CacheHitRate, got.CacheHitRate)
		}
		if got.Turns != want.Turns || got.ToolCalls != want.ToolCalls {
			t.Errorf("%s counts: full=%+v projected=%+v", want.ID, want, got)
		}
		if got.Model != want.Model {
			t.Errorf("%s model: full=%s projected=%s", want.ID, want.Model, got.Model)
		}
	}
}

// buildDays seeds n consecutive completed days, each with sessionsPerDay
// sessions of turnsPerSession turns, and compacts them.
func buildDays(t *testing.T, n, sessionsPerDay, turnsPerSession int) (*Compactor, time.Time, time.Time) {
	t.Helper()
	end := truncateDay(time.Now().UTC()).AddDate(0, 0, -1)
	start := end.AddDate(0, 0, -n+1)

	var events []trace.Event
	for d := 0; d < n; d++ {
		day := start.AddDate(0, 0, d)
		for s := 0; s < sessionsPerDay; s++ {
			session := fmt.Sprintf("d%02d-s%02d", d, s)
			for k := 0; k < turnsPerSession; k++ {
				ts := day.Add(time.Duration(s*turnsPerSession+k) * time.Minute)
				turn := fmt.Sprintf("%s-t%02d", session, k)
				events = append(events,
					trace.Event{Type: trace.EventRequest, Timestamp: ts,
						SessionID: session, TurnID: turn, Model: "claude-opus-5"},
					trace.Event{Type: trace.EventResponse, Timestamp: ts.Add(time.Second),
						SessionID: session, TurnID: turn, Model: "claude-opus-5",
						StatusCode: 200, DurationMS: 1000,
						Usage: &trace.Usage{InputTokens: 100, OutputTokens: 20}},
				)
			}
		}
	}

	c, _ := newCompactor(t, events...)
	if _, err := c.CompactAll(); err != nil {
		t.Fatalf("compact: %v", err)
	}
	return c, start, end.Add(24 * time.Hour)
}

// Paging through the whole window must yield exactly the unpaged list: same
// sessions, same order, no duplicates, no gaps.
func TestSessionsPageMatchesUnpaged(t *testing.T) {
	c, from, to := buildDays(t, 5, 4, 3)

	want, err := c.SessionsRange(from, to)
	if err != nil {
		t.Fatal(err)
	}
	if len(want) != 20 {
		t.Fatalf("setup: got %d sessions, want 20", len(want))
	}

	var got []query.Session
	seen := map[string]bool{}
	cursor := ""
	for pages := 0; ; pages++ {
		if pages > 50 {
			t.Fatal("pagination did not terminate")
		}
		page, err := c.SessionsPage(from, to, 3, cursor, Filter{})
		if err != nil {
			t.Fatal(err)
		}
		for _, s := range page.Sessions {
			if seen[s.ID] {
				t.Errorf("session %s returned on more than one page", s.ID)
			}
			seen[s.ID] = true
			got = append(got, s)
		}
		if page.NextCursor == "" {
			break
		}
		cursor = page.NextCursor
	}

	if len(got) != len(want) {
		t.Fatalf("paged returned %d sessions, unpaged %d", len(got), len(want))
	}
	for i := range want {
		if got[i].ID != want[i].ID {
			t.Errorf("position %d: paged=%s unpaged=%s", i, got[i].ID, want[i].ID)
		}
		if got[i].Turns != want[i].Turns {
			t.Errorf("%s turns: paged=%d unpaged=%d", got[i].ID, got[i].Turns, want[i].Turns)
		}
	}
}

// The first page must not read the whole window — that is the point.
func TestSessionsPageStopsEarly(t *testing.T) {
	c, from, to := buildDays(t, 30, 4, 2)

	page, err := c.SessionsPage(from, to, 3, "", Filter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Sessions) != 3 {
		t.Fatalf("got %d sessions, want 3", len(page.Sessions))
	}
	if page.NextCursor == "" {
		t.Error("expected more pages to be available")
	}
	// Four sessions land per day, so one or two days should satisfy a page of
	// three; the exact number depends on when sessions become complete.
	if page.DaysScanned > 3 {
		t.Errorf("scanned %d days to fill a 3-session page from a 30-day window", page.DaysScanned)
	}
	t.Logf("filled a 3-session page from a 30-day window after %d days", page.DaysScanned)
}

// A session spanning midnight must appear once, with all its turns, rather than
// being split or emitted early at the scan frontier.
func TestSessionsPageHandlesSessionSpanningMidnight(t *testing.T) {
	end := truncateDay(time.Now().UTC()).AddDate(0, 0, -1)
	start := end.AddDate(0, 0, -1)

	var events []trace.Event
	add := func(ts time.Time, session, turn string) {
		events = append(events,
			trace.Event{Type: trace.EventRequest, Timestamp: ts,
				SessionID: session, TurnID: turn, Model: "claude-opus-5"},
			trace.Event{Type: trace.EventResponse, Timestamp: ts.Add(time.Second),
				SessionID: session, TurnID: turn, Model: "claude-opus-5",
				StatusCode: 200, DurationMS: 1000,
				Usage: &trace.Usage{InputTokens: 100}},
		)
	}
	// One session with turns on both sides of midnight.
	add(start.Add(23*time.Hour+30*time.Minute), "spanning", "t1")
	add(end.Add(30*time.Minute), "spanning", "t2")
	// An unrelated session entirely on the later day.
	add(end.Add(5*time.Hour), "later", "t3")

	c, _ := newCompactor(t, events...)
	if _, err := c.CompactAll(); err != nil {
		t.Fatalf("compact: %v", err)
	}

	page, err := c.SessionsPage(start, end.Add(24*time.Hour), 50, "", Filter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Sessions) != 2 {
		t.Fatalf("got %d sessions, want 2: %+v", len(page.Sessions), page.Sessions)
	}
	for _, s := range page.Sessions {
		if s.ID == "spanning" && s.Turns != 2 {
			t.Errorf("session spanning midnight has %d turns, want 2", s.Turns)
		}
	}
}

func TestSessionsPageRejectsBadCursor(t *testing.T) {
	c, from, to := buildDays(t, 2, 1, 1)
	if _, err := c.SessionsPage(from, to, 10, "!!!not-base64!!!", Filter{}); err == nil {
		t.Error("expected an error for a malformed cursor")
	}
}

// Looking up one session must read only the days it touches, not the window.
func TestSessionDetailStopsEarly(t *testing.T) {
	c, from, to := buildDays(t, 30, 4, 3)

	// The newest day's sessions are named d29-*.
	detail, ok, err := c.SessionDetail(from, to, "d29-s03")
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("session not found")
	}
	if detail.Turns != 3 || len(detail.TurnList) != 3 {
		t.Errorf("turns = %d / %d, want 3", detail.Turns, len(detail.TurnList))
	}
	if detail.ID != "d29-s03" {
		t.Errorf("id = %s", detail.ID)
	}

	if _, ok, err := c.SessionDetail(from, to, "no-such-session"); err != nil {
		t.Fatal(err)
	} else if ok {
		t.Error("unknown session reported as found")
	}
}

// A session detail must include turns from both sides of midnight.
func TestSessionDetailSpanningMidnight(t *testing.T) {
	end := truncateDay(time.Now().UTC()).AddDate(0, 0, -1)
	start := end.AddDate(0, 0, -1)

	var events []trace.Event
	add := func(ts time.Time, turn string) {
		events = append(events,
			trace.Event{Type: trace.EventRequest, Timestamp: ts,
				SessionID: "spanning", TurnID: turn, Model: "claude-opus-5"},
			trace.Event{Type: trace.EventResponse, Timestamp: ts.Add(time.Second),
				SessionID: "spanning", TurnID: turn, Model: "claude-opus-5",
				StatusCode: 200, DurationMS: 1000, Usage: &trace.Usage{InputTokens: 100}},
		)
	}
	add(start.Add(23*time.Hour+30*time.Minute), "t1")
	add(end.Add(30*time.Minute), "t2")

	c, _ := newCompactor(t, events...)
	if _, err := c.CompactAll(); err != nil {
		t.Fatal(err)
	}

	detail, ok, err := c.SessionDetail(start, end.Add(24*time.Hour), "spanning")
	if err != nil || !ok {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
	if len(detail.TurnList) != 2 {
		t.Errorf("got %d turns, want 2 across midnight", len(detail.TurnList))
	}
}

// Per-project attribution has to survive compaction, and the absolute path
// must never appear in the archive.
func TestCompactedTurnKeepsProjectWithoutThePath(t *testing.T) {
	day := yesterday()
	events := dayEvents(day, "s1", "t1", "claude-opus-5", trace.Usage{InputTokens: 100})
	for i := range events {
		events[i].Project = trace.Project(`C:\Users\levir\dev\projects\apex-analysis`)
		events[i].ProjectID = trace.ProjectID(`C:\Users\levir\dev\projects\apex-analysis`)
		events[i].GitBranch = "main"
		events[i].Source = "claude-code"
		events[i].MessageID = "msg_x"
	}

	c, _ := newCompactor(t, events...)
	if _, err := c.CompactDay(day, false); err != nil {
		t.Fatal(err)
	}
	turns, ok, err := c.ReadTurns(day)
	if err != nil || !ok {
		t.Fatalf("read: ok=%v err=%v", ok, err)
	}
	if len(turns) != 1 {
		t.Fatalf("got %d turns", len(turns))
	}

	got := turns[0]
	if got.Project != "apex-analysis" {
		t.Errorf("Project = %q", got.Project)
	}
	if got.ProjectID == "" || got.GitBranch != "main" || got.Source != "claude-code" {
		t.Errorf("attribution lost: %+v", got)
	}
	if got.MessageID != "msg_x" {
		t.Errorf("MessageID = %q — dedup across sources depends on it", got.MessageID)
	}

	// Nothing in the partition may contain the original path.
	var leaked []string
	filepath.Walk(c.PartitionDir(day), func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		raw, readErr := os.ReadFile(path)
		if readErr == nil && (bytes.Contains(raw, []byte("levir")) || bytes.Contains(raw, []byte("Users"))) {
			leaked = append(leaked, filepath.Base(path))
		}
		return nil
	})
	if len(leaked) > 0 {
		t.Errorf("absolute path leaked into the archive: %v", leaked)
	}
}

// resumedSessionEvents builds one session whose turns land on the given days
// with idle days in between — the shape of a conversation picked back up the
// next morning, or the following week.
func resumedSessionEvents(id string, days []time.Time) []trace.Event {
	var events []trace.Event
	for i, day := range days {
		ts := day.Add(9 * time.Hour)
		turn := fmt.Sprintf("t%d", i)
		events = append(events,
			trace.Event{Type: trace.EventRequest, Timestamp: ts,
				SessionID: id, TurnID: turn, Model: "claude-opus-5"},
			trace.Event{Type: trace.EventResponse, Timestamp: ts.Add(time.Second),
				SessionID: id, TurnID: turn, Model: "claude-opus-5",
				StatusCode: 200, DurationMS: 1000, Usage: &trace.Usage{InputTokens: 100}},
		)
	}
	return events
}

// A session resumed after an idle day is one session, not several. Walking days
// backwards used to treat the first day without turns as the session's end,
// which listed the same conversation once per stretch of activity.
func TestSessionsPageKeepsResumedSessionWhole(t *testing.T) {
	today := truncateDay(time.Now().UTC())
	days := []time.Time{
		today.AddDate(0, 0, -6),
		today.AddDate(0, 0, -4),
		today.AddDate(0, 0, -1),
	}

	c, _ := newCompactor(t, resumedSessionEvents("resumed", days)...)
	if _, err := c.CompactAll(); err != nil {
		t.Fatalf("compact: %v", err)
	}

	page, err := c.SessionsPage(today.AddDate(0, 0, -7), today, 50, "", Filter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Sessions) != 1 {
		t.Fatalf("got %d rows for one resumed session: %+v", len(page.Sessions), page.Sessions)
	}
	if page.Sessions[0].Turns != len(days) {
		t.Errorf("turns = %d, want %d", page.Sessions[0].Turns, len(days))
	}
}

// The detail view must carry every day of a resumed session, in time order.
// Truncating at the first idle day made a long session look short and cheap.
func TestSessionDetailSpansIdleDays(t *testing.T) {
	today := truncateDay(time.Now().UTC())
	days := []time.Time{
		today.AddDate(0, 0, -6),
		today.AddDate(0, 0, -4),
		today.AddDate(0, 0, -1),
	}

	c, _ := newCompactor(t, resumedSessionEvents("resumed", days)...)
	if _, err := c.CompactAll(); err != nil {
		t.Fatalf("compact: %v", err)
	}

	detail, ok, err := c.SessionDetail(today.AddDate(0, 0, -7), today, "resumed")
	if err != nil || !ok {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
	if len(detail.TurnList) != len(days) {
		t.Fatalf("got %d turns, want %d across idle days", len(detail.TurnList), len(days))
	}
	for i, turn := range detail.TurnList {
		if i > 0 && turn.StartedAt.Before(detail.TurnList[i-1].StartedAt) {
			t.Fatalf("turn %d starts before its predecessor: %s < %s",
				i, turn.StartedAt, detail.TurnList[i-1].StartedAt)
		}
		if got := truncateDay(turn.StartedAt); !got.Equal(days[i]) {
			t.Errorf("turn %d on %s, want %s", i, got.Format("2006-01-02"), days[i].Format("2006-01-02"))
		}
	}
}

// An archive compacted before the session-day index existed must still read
// correctly: the index is derived from the turns already on disk.
func TestSessionDayIndexDerivedFromOlderPartitions(t *testing.T) {
	today := truncateDay(time.Now().UTC())
	days := []time.Time{today.AddDate(0, 0, -5), today.AddDate(0, 0, -2)}

	c, _ := newCompactor(t, resumedSessionEvents("resumed", days)...)
	if _, err := c.CompactAll(); err != nil {
		t.Fatalf("compact: %v", err)
	}

	// Take the prepared rollups away, leaving only turns.parquet, as an older
	// build would have written.
	for _, day := range days {
		if err := os.Remove(filepath.Join(c.PartitionDir(day), sessionsFile)); err != nil {
			t.Fatal(err)
		}
	}
	if err := c.RebuildIndex(); err != nil {
		t.Fatalf("rebuild index: %v", err)
	}

	detail, ok, err := c.SessionDetail(today.AddDate(0, 0, -7), today, "resumed")
	if err != nil || !ok {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
	if len(detail.TurnList) != len(days) {
		t.Errorf("got %d turns, want %d", len(detail.TurnList), len(days))
	}
}
