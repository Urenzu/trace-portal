package compact

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/Urenzu/trace-portal/internal/store"
	"github.com/Urenzu/trace-portal/internal/trace"
)

// prunable is a hot window that can drop a day, which the local file store
// deliberately cannot: on a laptop the day file is the archive. It stands in
// for the Postgres window, where the day is a second copy of something object
// storage already holds.
type prunable struct {
	*store.Store
	dropped map[string]bool
}

func (p *prunable) DropDay(ctx context.Context, day time.Time) error {
	p.dropped[day.UTC().Format(dayLayout)] = true
	return nil
}

func (p *prunable) Days(ctx context.Context) ([]time.Time, error) {
	days, err := p.Store.Days(ctx)
	if err != nil {
		return nil, err
	}
	var kept []time.Time
	for _, d := range days {
		if !p.dropped[d.UTC().Format(dayLayout)] {
			kept = append(kept, d)
		}
	}
	return kept, nil
}

func (p *prunable) Events(ctx context.Context, day time.Time) ([]trace.Event, error) {
	if p.dropped[day.UTC().Format(dayLayout)] {
		return nil, nil
	}
	return p.Store.Events(ctx, day)
}

func newPrunableCompactor(t *testing.T, events ...trace.Event) (*Compactor, *prunable) {
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
	hot := &prunable{Store: st, dropped: map[string]bool{}}
	c, err := New(hot, dir)
	if err != nil {
		t.Fatalf("new compactor: %v", err)
	}
	c.PruneCompacted(true)
	return c, hot
}

// TestCompactionReleasesTheHotWindow is the cost bug this fixes: DropDay
// existed and nothing called it, so a deployment paid for every day twice --
// once in Postgres and once as Parquet -- forever.
func TestCompactionReleasesTheHotWindow(t *testing.T) {
	usage := trace.Usage{InputTokens: 1000, OutputTokens: 300}
	older, newer := yesterday().AddDate(0, 0, -1), yesterday()

	var events []trace.Event
	events = append(events, dayEvents(older, "s1", "t1", "claude-opus-5", usage, "bash")...)
	events = append(events, dayEvents(newer, "s2", "t2", "claude-opus-5", usage, "grep")...)
	c, hot := newPrunableCompactor(t, events...)

	if _, err := c.CompactAll(); err != nil {
		t.Fatalf("compact: %v", err)
	}

	for _, day := range []time.Time{older, newer} {
		if !hot.dropped[day.UTC().Format(dayLayout)] {
			t.Fatalf("%s was compacted but never released from the hot window", day.Format(dayLayout))
		}
	}

	// Released, not lost. The history still answers, and the archive still
	// knows how far back it goes -- which is why health asks the compactor for
	// days rather than the store.
	days, err := c.Days(context.Background())
	if err != nil {
		t.Fatalf("days: %v", err)
	}
	if len(days) != 2 {
		t.Fatalf("archive reports %d days after pruning, want 2", len(days))
	}
	// To the end of the newer day: the range trims to instants, and both days'
	// turns sit at 03:00.
	turns, err := c.TurnsRange(older, newer.Add(24*time.Hour))
	if err != nil {
		t.Fatalf("turns: %v", err)
	}
	if len(turns) != 2 {
		t.Fatalf("read back %d turns after pruning, want 2", len(turns))
	}
}

// TestPruneRefusesWhenThePartitionDisagrees is the guard that makes the delete
// safe. A partition that reads back short is the one failure mode that turns a
// storage saving into permanent data loss, and it is invisible to a
// write-then-delete sequence that only checks the write's error.
func TestPruneRefusesWhenThePartitionDisagrees(t *testing.T) {
	usage := trace.Usage{InputTokens: 1000, OutputTokens: 300}
	day := yesterday()
	c, hot := newPrunableCompactor(t, dayEvents(day, "s1", "t1", "claude-opus-5", usage, "bash")...)

	if _, err := c.CompactAll(); err != nil {
		t.Fatalf("compact: %v", err)
	}
	if !hot.dropped[day.UTC().Format(dayLayout)] {
		t.Fatal("expected the first sweep to release the day")
	}

	// Now the hot window holds a turn the partition does not, which is what a
	// short or stale partition looks like from here.
	hot.dropped = map[string]bool{}
	for _, ev := range dayEvents(day, "s9", "t9", "claude-opus-5", usage, "read_file") {
		if err := hot.Store.Append(context.Background(), ev); err != nil {
			t.Fatalf("append: %v", err)
		}
	}

	err := c.pruneHotWindow()
	if err == nil {
		t.Fatal("pruned a day whose partition disagreed with the hot window")
	}
	if !strings.Contains(err.Error(), "not dropping") {
		t.Fatalf("unexpected error: %v", err)
	}
	if hot.dropped[day.UTC().Format(dayLayout)] {
		t.Fatal("dropped the day anyway")
	}
}
