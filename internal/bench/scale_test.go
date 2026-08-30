package bench

import (
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/Urenzu/trace-portal/internal/compact"
	"github.com/Urenzu/trace-portal/internal/query"
	"github.com/Urenzu/trace-portal/internal/store"
	"github.com/Urenzu/trace-portal/internal/trace"
)

// TestScaleJSONLvsParquet measures the read path before and after compaction at
// a year of heavy use. It is skipped under -short because it writes a few
// hundred megabytes.
//
//	go test ./internal/bench -run TestScaleJSONLvsParquet -v -timeout 30m
func TestScaleJSONLvsParquet(t *testing.T) {
	if testing.Short() {
		t.Skip("writes hundreds of MB; run explicitly")
	}

	const (
		days        = 365
		turnsPerDay = 1000 // a heavy agent user, every day for a year
	)

	dir := t.TempDir()
	st, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}

	models := []string{"claude-opus-5", "claude-sonnet-5", "claude-haiku-4-5"}
	tools := []string{"bash", "read_file", "edit", "grep", "web_search"}
	rng := rand.New(rand.NewSource(1))

	// Anchor the generated history to yesterday so every day is compactable.
	end := time.Now().UTC().Truncate(24*time.Hour).AddDate(0, 0, -1)
	start := end.AddDate(0, 0, -days+1)

	t0 := time.Now()
	for d := 0; d < days; d++ {
		day := start.AddDate(0, 0, d)
		for i := 0; i < turnsPerDay; i++ {
			ts := day.Add(time.Duration(i) * time.Minute)
			turnID := fmt.Sprintf("%d-%d", d, i)
			session := fmt.Sprintf("sess-%d-%d", d, i/12) // ~12 turns per session
			model := models[rng.Intn(len(models))]

			st.Append(trace.Event{
				Type: trace.EventRequest, Timestamp: ts, SessionID: session, TurnID: turnID,
				Model: model, Stream: true, MessageCount: 4 + i%20, SystemBlocks: 2,
				ToolsOffered: tools, MaxTokens: 4096,
				RequestBlob: "995d8f73806444b693d0e939cb5b2be06f3c8b54a085a06020d5e6c1c5dac6bb",
			})
			st.Append(trace.Event{
				Type: trace.EventResponse, Timestamp: ts.Add(2 * time.Second),
				SessionID: session, TurnID: turnID, Model: model,
				StatusCode: 200, StopReason: "tool_use", DurationMS: 2000, TTFBMS: 300,
				Usage: &trace.Usage{
					InputTokens: 1200, OutputTokens: 400,
					CacheCreationInputTokens: 2000, CacheReadInputTokens: 20000,
					CacheCreation: &trace.CacheCreation{Ephemeral5mInputTokens: 2000},
				},
				ToolCalls:    []trace.ToolCall{{ID: "tu_1", Name: tools[rng.Intn(len(tools))]}},
				ResponseBlob: "fdd19ee3503d72856fcac9be456cd8ac9a8bc1a7d5a5e27063bcfaea28e9b71d",
			})
		}
	}
	st.Close()
	t.Logf("generated %d turns across %d days in %v", days*turnsPerDay, days, time.Since(t0).Round(time.Millisecond))
	t.Logf("jsonl on disk: %s", humanSize(dirSize(t, filepath.Join(dir, "events"))))

	st2, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer st2.Close()
	c, err := compact.New(st2, dir)
	if err != nil {
		t.Fatal(err)
	}

	windows := []struct {
		label string
		days  int
	}{
		{"7 days", 7},
		{"30 days", 30},
		{"365 days", 365},
	}

	// --- before compaction: everything reads from JSONL ---
	t.Log("--- JSONL (uncompacted) ---")
	before := map[string][2]time.Duration{}
	for _, w := range windows {
		from, to := end.AddDate(0, 0, -w.days+1), end.Add(24*time.Hour)

		runtime.GC()
		t1 := time.Now()
		events, err := st2.EventsRange(from, to)
		if err != nil {
			t.Fatal(err)
		}
		sessions := query.BuildSessions(events)
		listMS := time.Since(t1)
		events, sessionCount := nil, len(sessions)
		_ = events

		runtime.GC()
		t2 := time.Now()
		agg, err := c.AggregateRange(from, to)
		if err != nil {
			t.Fatal(err)
		}
		statsMS := time.Since(t2)

		before[w.label] = [2]time.Duration{listMS, statsMS}
		t.Logf("%-9s sessions=%-7d list=%-9v stats=%-9v (turns=%d cost=$%.2f)",
			w.label, sessionCount, listMS.Round(time.Millisecond),
			statsMS.Round(time.Millisecond), agg.Turns, agg.CostUSD)
	}

	// --- compact ---
	t3 := time.Now()
	n, err := c.CompactAll()
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("--- compacted %d days in %v ---", n, time.Since(t3).Round(time.Millisecond))
	t.Logf("parquet on disk: %s", humanSize(dirSize(t, filepath.Join(dir, "compact"))))

	// The list endpoint pages in practice; measure what the first page costs
	// against reading the whole window.
	{
		from, to := end.AddDate(0, 0, -364), end.Add(24*time.Hour)
		runtime.GC()
		t0 := time.Now()
		page, err := c.SessionsPage(from, to, 50, "")
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("--- first page over a 365-day window ---")
		t.Logf("50 sessions in %v after scanning %d of 365 days",
			time.Since(t0).Round(time.Millisecond), page.DaysScanned)
	}

	// --- after compaction ---
	t.Log("--- Parquet (compacted) ---")
	for _, w := range windows {
		from, to := end.AddDate(0, 0, -w.days+1), end.Add(24*time.Hour)

		runtime.GC()
		t1 := time.Now()
		sessions, err := c.SessionsRange(from, to)
		if err != nil {
			t.Fatal(err)
		}
		listMS := time.Since(t1)
		sessionCount := len(sessions)
		sessions = nil
		_ = sessions

		runtime.GC()
		t2 := time.Now()
		agg, err := c.AggregateRange(from, to)
		if err != nil {
			t.Fatal(err)
		}
		statsMS := time.Since(t2)

		b := before[w.label]
		t.Logf("%-9s sessions=%-7d list=%-9v (%.1fx)  stats=%-9v (%.0fx)  (turns=%d cost=$%.2f)",
			w.label, sessionCount,
			listMS.Round(time.Millisecond), float64(b[0])/float64(listMS),
			statsMS.Round(time.Microsecond), float64(b[1])/float64(statsMS),
			agg.Turns, agg.CostUSD)
	}
}

func dirSize(t *testing.T, root string) int64 {
	t.Helper()
	var total int64
	filepath.Walk(root, func(_ string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() {
			total += info.Size()
		}
		return nil
	})
	return total
}

func humanSize(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for x := n / unit; x >= unit; x /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGT"[exp])
}
