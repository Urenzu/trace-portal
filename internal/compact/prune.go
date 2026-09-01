package compact

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/Urenzu/trace-portal/internal/query"
)

// Pruner is a hot window that can release a day once it has been compacted.
//
// Optional, and deliberately so. The local tool's hot window is a JSONL file
// per day, and that file *is* the archive -- deleting it would delete the only
// copy of anything Parquet does not project. A server's hot window is a
// database table that costs money per gigabyte and holds the same days as
// object storage at roughly fifty times the size, so there the copy is the
// thing worth releasing.
type Pruner interface {
	DropDay(ctx context.Context, day time.Time) error
}

// PruneCompacted makes the compactor release each day from the hot window once
// its partition is written and verified.
//
// Off unless asked for. It is a data-deleting behaviour, and the invariant this
// project holds -- nothing deletes an ingested trace -- is only preserved
// because the partition is read back and checked first, which is what pruneDay
// does before it drops anything.
func (c *Compactor) PruneCompacted(on bool) { c.prune = on }

// pruneHotWindow releases every day the hot window still holds that already has
// a verified partition.
//
// Today is never touched, for the same reason it is never compacted: it is
// still being appended to.
func (c *Compactor) pruneHotWindow() error {
	if _, ok := c.store.(Pruner); !ok {
		return nil
	}
	days, err := c.store.Days(storeContext())
	if err != nil {
		return err
	}
	today := truncateDay(time.Now())
	for _, day := range days {
		day = truncateDay(day)
		if !day.Before(today) || !c.IsCompacted(day) {
			continue
		}
		if err := c.pruneDay(day); err != nil {
			return err
		}
	}
	return nil
}

// pruneDay releases one compacted day from the hot window.
//
// It reads the partition back and compares it against the day still sitting in
// the hot window before deleting anything. A write that returned nil and
// produced an unreadable or short file is the one failure that would turn a
// cost saving into data loss, and it is exactly the failure a write-then-delete
// sequence cannot notice on its own. The read costs one object fetch per
// compacted day, once, against a delete that is permanent.
func (c *Compactor) pruneDay(day time.Time) error {
	dropper, ok := c.store.(Pruner)
	if !ok {
		return nil
	}

	turns, found, err := c.ReadTurns(day)
	if err != nil {
		return fmt.Errorf("verify partition for %s: %w", partitionKey(day), err)
	}
	if !found {
		return fmt.Errorf("partition for %s does not exist", partitionKey(day))
	}

	events, err := c.store.Events(storeContext(), day)
	if err != nil {
		return fmt.Errorf("re-read %s from the hot window: %w", partitionKey(day), err)
	}
	if want := len(query.BuildTurns(events)); len(turns) != want {
		// The two disagree, so one of them is wrong and there is no way to tell
		// which. Refusing costs storage; guessing costs turns.
		return fmt.Errorf("partition for %s holds %d turns, the hot window holds %d; not dropping",
			partitionKey(day), len(turns), want)
	}

	return dropper.DropDay(storeContext(), day)
}

// Days lists every day the archive holds, from the partitions and from the hot
// window, oldest first.
//
// The hot window alone stopped being the answer the moment compacted days
// started being released from it: asking the store would report an archive that
// begins whenever the last compaction ran, and a UI showing "12 days captured"
// on a year of history is worse than showing nothing.
func (c *Compactor) Days(ctx context.Context) ([]time.Time, error) {
	hot, err := c.store.Days(ctx)
	if err != nil {
		return nil, err
	}
	cold, err := c.partitionDays()
	if err != nil {
		return nil, err
	}

	seen := make(map[string]bool, len(hot)+len(cold))
	days := make([]time.Time, 0, len(hot)+len(cold))
	for _, set := range [][]time.Time{cold, hot} {
		for _, d := range set {
			key := d.UTC().Format(dayLayout)
			if seen[key] {
				continue
			}
			seen[key] = true
			days = append(days, truncateDay(d))
		}
	}
	sort.Slice(days, func(i, j int) bool { return days[i].Before(days[j]) })
	return days, nil
}
