package compact

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/Urenzu/trace-portal/internal/objectstore"
	"github.com/Urenzu/trace-portal/internal/trace"
)

// countingStore records how many object reads a query costs.
//
// The number is the point. Against a local directory a read is microseconds and
// nothing here would ever be noticed; against a bucket it is a network round
// trip, so "how many reads" is the difference between a dashboard that answers
// instantly and one that does not.
type countingStore struct {
	objectstore.Store
	mu    sync.Mutex
	gets  int
	exist int
}

func (c *countingStore) Get(ctx context.Context, key string) ([]byte, error) {
	c.mu.Lock()
	c.gets++
	c.mu.Unlock()
	return c.Store.Get(ctx, key)
}

func (c *countingStore) Exists(ctx context.Context, key string) (bool, error) {
	c.mu.Lock()
	c.exist++
	c.mu.Unlock()
	return c.Store.Exists(ctx, key)
}

func (c *countingStore) reads() (int, int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.gets, c.exist
}

func countingCompactor(t *testing.T, events ...trace.Event) (*Compactor, *countingStore) {
	t.Helper()
	c, _ := newCompactor(t, events...)
	counted := &countingStore{Store: c.objects}
	c.objects = counted
	return c, counted
}

// A window over days that hold nothing must cost nothing.
//
// This was the real cost against object storage, and it is worth a test rather
// than a comment: a day the rollup does not cover used to fall through to a
// partition read and then a hot-window read. Locally those were two failed file
// opens and invisible. Against MinIO on the same machine a 365-day window over
// an archive with 15 active days took 1.2 seconds, almost all of it spent
// proving that 350 empty days were empty.
func TestEmptyDaysCostNoObjectReads(t *testing.T) {
	day := yesterday()
	events := dayEvents(day, "s1", "t1", "claude-opus-5",
		trace.Usage{InputTokens: 100, OutputTokens: 50, CacheReadInputTokens: 9000}, "bash")

	c, counted := countingCompactor(t, events...)
	if _, err := c.CompactDay(day, false); err != nil {
		t.Fatalf("compact: %v", err)
	}
	if err := c.RebuildIndex(); err != nil {
		t.Fatalf("rebuild index: %v", err)
	}

	// A narrow window that includes the one day with data.
	if _, err := c.DailyRange(day.AddDate(0, 0, -1), day.AddDate(0, 0, 1)); err != nil {
		t.Fatalf("narrow range: %v", err)
	}
	narrowGets, narrowExists := counted.reads()

	// A year, of which all but one day is empty. It must not cost meaningfully
	// more than the narrow one.
	if _, err := c.DailyRange(day.AddDate(-1, 0, 0), day.AddDate(0, 0, 1)); err != nil {
		t.Fatalf("wide range: %v", err)
	}
	wideGets, wideExists := counted.reads()

	extraGets := wideGets - narrowGets
	extraExists := wideExists - narrowExists
	if extraGets > 2 || extraExists > 2 {
		t.Fatalf("a 365-day window cost %d extra gets and %d extra exists over a 3-day one; "+
			"empty days are still being probed", extraGets, extraExists)
	}
}

// The rollup is read once and then held. Five object reads per dashboard load
// is five network round trips against a bucket, for 14 KB that changes only
// when compaction runs.
func TestRollupIsCachedBetweenQueries(t *testing.T) {
	day := yesterday()
	events := dayEvents(day, "s1", "t1", "claude-opus-5",
		trace.Usage{InputTokens: 100, OutputTokens: 50, CacheReadInputTokens: 9000}, "bash")

	c, counted := countingCompactor(t, events...)
	if _, err := c.CompactDay(day, false); err != nil {
		t.Fatalf("compact: %v", err)
	}
	if err := c.RebuildIndex(); err != nil {
		t.Fatalf("rebuild index: %v", err)
	}

	from, to := day.AddDate(0, 0, -1), day.AddDate(0, 0, 1)
	if _, err := c.DailyRange(from, to); err != nil {
		t.Fatalf("first: %v", err)
	}
	firstGets, _ := counted.reads()

	for i := 0; i < 5; i++ {
		if _, err := c.DailyRange(from, to); err != nil {
			t.Fatalf("repeat %d: %v", i, err)
		}
	}
	repeatGets, _ := counted.reads()

	perQuery := float64(repeatGets-firstGets) / 5
	if perQuery >= 5 {
		t.Fatalf("each repeat query cost %.1f object reads; the rollup is not cached", perQuery)
	}
}

// Caching is only safe if the cached copy is dropped when the rollup changes.
// A dashboard that kept showing yesterday's totals after a compaction would be
// a worse failure than the latency the cache exists to remove.
func TestCompactionInvalidatesTheCachedRollup(t *testing.T) {
	first := yesterday().AddDate(0, 0, -1)
	second := yesterday()

	c, _ := newCompactor(t,
		dayEvents(first, "s1", "t1", "claude-opus-5",
			trace.Usage{InputTokens: 100, OutputTokens: 50}, "bash")...)

	if _, err := c.CompactDay(first, false); err != nil {
		t.Fatalf("compact first: %v", err)
	}
	if err := c.RebuildIndex(); err != nil {
		t.Fatalf("rebuild: %v", err)
	}

	// Read it, so the rollup is cached.
	before, err := c.DailyRange(first.AddDate(0, 0, -1), second.AddDate(0, 0, 1))
	if err != nil {
		t.Fatalf("before: %v", err)
	}
	if len(before) != 1 {
		t.Fatalf("expected 1 day before the second compaction, got %d", len(before))
	}

	// Add another day and compact it.
	for _, ev := range dayEvents(second, "s2", "t2", "claude-opus-5",
		trace.Usage{InputTokens: 200, OutputTokens: 80}, "bash") {
		if err := c.store.Append(context.Background(), ev); err != nil {
			t.Fatalf("append: %v", err)
		}
	}
	if _, err := c.CompactDay(second, false); err != nil {
		t.Fatalf("compact second: %v", err)
	}
	if err := c.RebuildIndex(); err != nil {
		t.Fatalf("rebuild: %v", err)
	}

	after, err := c.DailyRange(first.AddDate(0, 0, -1), second.AddDate(0, 0, 1))
	if err != nil {
		t.Fatalf("after: %v", err)
	}
	if len(after) != 2 {
		t.Fatalf("the cached rollup survived a compaction: still showing %d days", len(after))
	}
}

// The cache is read from request goroutines and written by a background
// compaction, so it has to be safe under both at once.
func TestRollupCacheIsConcurrencySafe(t *testing.T) {
	day := yesterday()
	c, _ := newCompactor(t, dayEvents(day, "s1", "t1", "claude-opus-5",
		trace.Usage{InputTokens: 100, OutputTokens: 50}, "bash")...)
	if _, err := c.CompactDay(day, false); err != nil {
		t.Fatalf("compact: %v", err)
	}

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 20; j++ {
				if _, err := c.DailyRange(day.AddDate(0, 0, -1), day.AddDate(0, 0, 1)); err != nil {
					t.Errorf("read: %v", err)
					return
				}
			}
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for j := 0; j < 10; j++ {
			if err := c.RebuildIndex(); err != nil {
				t.Errorf("rebuild: %v", err)
				return
			}
			time.Sleep(time.Millisecond)
		}
	}()
	wg.Wait()
}
