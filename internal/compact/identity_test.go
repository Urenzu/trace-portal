package compact

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Urenzu/trace-portal/internal/store"
	"github.com/Urenzu/trace-portal/internal/trace"
)

// Identity has to survive the durable form, not just the hot path. Compaction
// rewrites a day into Parquet and the JSONL behind it is what gets aged out, so
// a field that is dropped here is gone for good — which is exactly the failure
// this whole pass exists to avoid.
func TestIdentitySurvivesCompaction(t *testing.T) {
	day := yesterday()
	who := trace.Identity{TenantID: "t_acme", UserID: "u_dana", MachineID: "m_laptop"}

	events := dayEvents(day, "s1", "t1", "claude-opus-5",
		trace.Usage{InputTokens: 100, OutputTokens: 50, CacheReadInputTokens: 9000}, "bash")
	for i := range events {
		events[i].Identity = who
	}

	c, _ := newCompactor(t, events...)
	if _, err := c.CompactDay(day, false); err != nil {
		t.Fatalf("compact: %v", err)
	}

	turns, err := c.TurnsRange(day, day.Add(24*time.Hour))
	if err != nil {
		t.Fatalf("read turns: %v", err)
	}
	if len(turns) != 1 {
		t.Fatalf("want 1 turn, got %d", len(turns))
	}
	if turns[0].Identity != who {
		t.Fatalf("identity lost in Parquet: want %+v, got %+v", who, turns[0].Identity)
	}
}

// The session list reads a narrow projection to avoid pulling the wide columns
// off disk. Columns are matched by name, so a projection that forgot these
// would not fail loudly — it would quietly return sessions with no owner.
func TestIdentitySurvivesTheNarrowSessionProjection(t *testing.T) {
	day := yesterday()
	who := trace.Identity{TenantID: "t_acme", UserID: "u_dana", MachineID: "m_laptop"}

	events := dayEvents(day, "s1", "t1", "claude-opus-5",
		trace.Usage{InputTokens: 100, OutputTokens: 50, CacheReadInputTokens: 9000}, "bash")
	for i := range events {
		events[i].Identity = who
	}

	c, _ := newCompactor(t, events...)
	if _, err := c.CompactDay(day, false); err != nil {
		t.Fatalf("compact: %v", err)
	}

	sessions, err := c.SessionsRange(day, day.Add(24*time.Hour))
	if err != nil {
		t.Fatalf("list sessions: %v", err)
	}
	if len(sessions) != 1 {
		t.Fatalf("want 1 session, got %d", len(sessions))
	}
	if sessions[0].TenantID != who.TenantID || sessions[0].UserID != who.UserID {
		t.Fatalf("session lost its owner: %+v", sessions[0].Identity)
	}
}

// Records written before identity existed must still read back. The archive
// outlives its source and is never rewritten, so every build from here on has
// to cope with a population of turns that predate this field — reading them as
// unattributed rather than failing to decode them at all.
func TestTurnsPredatingIdentityStillRead(t *testing.T) {
	day := yesterday()
	events := dayEvents(day, "s_old", "t_old", "claude-opus-5",
		trace.Usage{InputTokens: 10, OutputTokens: 5}, "bash")

	c, st := newCompactor(t)
	// Bypass the store's stamping to simulate a log written by an earlier
	// build: the JSONL on disk simply has no identity keys.
	if err := writeRawEvents(st, events); err != nil {
		t.Fatalf("write raw: %v", err)
	}
	if _, err := c.CompactDay(day, false); err != nil {
		t.Fatalf("compact: %v", err)
	}

	turns, err := c.TurnsRange(day, day.Add(24*time.Hour))
	if err != nil {
		t.Fatalf("read turns: %v", err)
	}
	if len(turns) != 1 {
		t.Fatalf("want 1 turn, got %d", len(turns))
	}
	if turns[0].Identity.Attributed() {
		t.Fatalf("a pre-identity turn should read as unattributed, got %+v", turns[0].Identity)
	}
}

// writeRawEvents appends events to the day log without going through
// Store.Append, which would stamp an identity onto them. It is the only way to
// produce the shape an older build left behind.
func writeRawEvents(st *store.Store, events []trace.Event) error {
	if len(events) == 0 {
		return nil
	}
	day := events[0].Timestamp.UTC().Format("2006-01-02")
	path := filepath.Join(st.Root(), "events", day+".jsonl")
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()

	enc := json.NewEncoder(f)
	for _, ev := range events {
		if !ev.Identity.Zero() {
			return fmt.Errorf("event already carries an identity: %+v", ev.Identity)
		}
		if err := enc.Encode(ev); err != nil {
			return err
		}
	}
	return nil
}
