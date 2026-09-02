// Package ingest drives the log sources: it backfills what is already on disk,
// then follows the files for new records.
//
// Nothing here sits in an agent's request path, so a failure only costs trace
// data. Every error is logged and the next pass retries.
package ingest

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/Urenzu/trace-portal/internal/eventstore"
	"github.com/Urenzu/trace-portal/internal/source"
	"github.com/Urenzu/trace-portal/internal/trace"
)

// checkpointFile records how far each log file has been read, so a restart
// resumes rather than re-reading months of transcripts. It also carries the
// coverage report, which describes the whole archive rather than one run: a
// process that finds nothing new to parse still has to be able to say what the
// data it already holds was missing.
const checkpointFile = "ingest.json"

// Ingester follows a set of sources into a store.
type Ingester struct {
	store   eventstore.Store
	blobs   eventstore.Blobs
	sources []source.Source
	log     *slog.Logger
	path    string

	// content records whether this run is capturing content, and recapture
	// whether it should re-read transcripts it has already consumed. See
	// Recapture.
	content   bool
	recapture bool

	// backfilled closes once the first pass over everything on disk has
	// finished, so a caller that has to act on what was read -- rebuilding
	// partitions after a re-read -- can wait for it rather than guess.
	backfilled chan struct{}
	once       sync.Once

	mu       sync.Mutex
	offsets  map[string]int64
	coverage map[string]*source.Coverage
}

// Recapture declares whether content is being captured, and whether to rewind.
//
// Offsets are why this exists. The ingester remembers how far into each
// transcript it has read, so switching content capture on captures only what
// has not been read yet -- and everything already on disk, up to the month
// Claude Code keeps, is already read. Rewinding is the only way to reach it,
// and it has to be asked for: re-reading appends the measurement events a
// second time as well, which turns collapse back into one turn apiece but which
// does roughly double the raw log until those days compact.
//
// Not rewinding is also a decision, and a permanent one: the source prunes
// after about a month, so a backlog not captured now is gone.
func (i *Ingester) Recapture(content, rewind bool) {
	i.content, i.recapture = content, rewind
}

// checkpoint is what survives a restart.
type checkpoint struct {
	Offsets  map[string]int64            `json:"offsets"`
	Coverage map[string]*source.Coverage `json:"coverage,omitempty"`

	// Content records whether the run that wrote these offsets was capturing
	// content, so a build that starts capturing can say that everything behind
	// the offsets does not have any.
	Content bool `json:"content,omitempty"`
}

// New builds an Ingester writing into st, checkpointing under dataDir.
func New(st eventstore.Store, dataDir string, log *slog.Logger, sources ...source.Source) *Ingester {
	if log == nil {
		log = slog.Default()
	}
	blobs, _ := st.(eventstore.Blobs)
	return &Ingester{
		backfilled: make(chan struct{}),
		store:      st,
		// Optional, and checked rather than required, so a store that only
		// appends is still a usable target. Without it captured content is
		// dropped at the seam instead of being written half-way.
		blobs:    blobs,
		sources:  sources,
		log:      log,
		path:     filepath.Join(dataDir, checkpointFile),
		offsets:  map[string]int64{},
		coverage: map[string]*source.Coverage{},
	}
}

// Backfilled is closed once the first pass over everything on disk has
// finished, successfully or not. It never fires if Run is not called.
func (i *Ingester) Backfilled() <-chan struct{} { return i.backfilled }

// Coverage reports what parsing understood across the whole archive: what this
// process read, folded onto what earlier runs recorded.
func (i *Ingester) Coverage() map[string]*source.Coverage {
	out := map[string]*source.Coverage{}

	i.mu.Lock()
	for name, saved := range i.coverage {
		merged := source.NewCoverage()
		merged.Merge(saved)
		out[name] = merged
	}
	i.mu.Unlock()

	for _, src := range i.sources {
		reporter, ok := src.(interface{ Coverage() *source.Coverage })
		if !ok {
			continue
		}
		if out[src.Name()] == nil {
			out[src.Name()] = source.NewCoverage()
		}
		out[src.Name()].Merge(reporter.Coverage())
	}
	return out
}

// snapshotCoverage folds this run's counts into the persisted totals and clears
// the live ones, so a later save cannot count the same records twice.
func (i *Ingester) snapshotCoverage() {
	for _, src := range i.sources {
		reporter, ok := src.(interface{ Coverage() *source.Coverage })
		if !ok {
			continue
		}
		i.mu.Lock()
		if i.coverage[src.Name()] == nil {
			i.coverage[src.Name()] = source.NewCoverage()
		}
		i.coverage[src.Name()].Merge(reporter.Coverage())
		i.mu.Unlock()
		reporter.Coverage().Reset()
	}
}

// storeContent moves a captured payload into the blob store and leaves the
// event holding only its reference.
//
// The parser hands over bytes because a component that reads files has no
// business holding something it could write the archive with. The write happens
// here, once, and the event that reaches storage is the same fixed width it has
// always been -- which is what keeps a compaction pass over a year of turns
// independent of how much anybody typed.
//
// Blob references are content hashes, so the same file read twice, or the same
// system prompt sent on every turn, is stored once.
func (i *Ingester) storeContent(ctx context.Context, ev *trace.Event) error {
	if len(ev.Content) == 0 {
		return nil
	}
	payload := ev.Content
	// Cleared unconditionally: it is marked json:"-" so it would not be
	// serialised anyway, but leaving it set would keep the bytes alive for as
	// long as anything held the event.
	ev.Content = nil
	if i.blobs == nil {
		return nil
	}
	ref, err := i.blobs.Put(ctx, payload)
	if err != nil {
		return err
	}
	ev.ContentBlob = ref
	return nil
}

// Result reports what one pass ingested.
type Result struct {
	Events int
	Files  int
}

// Run performs an initial pass over everything on disk, then polls.
//
// Polling rather than filesystem notifications: transcripts are appended
// continuously, a poll is a stat per file, and it behaves the same on every
// platform. The lag is bounded by the interval, and agents write these files
// while they work, so a couple of seconds is already close to live.
func (i *Ingester) Run(ctx context.Context, every time.Duration) error {
	if err := i.loadCheckpoint(); err != nil {
		i.log.Warn("could not read ingest checkpoint; starting from scratch", "err", err)
	}

	start := time.Now()
	res, err := i.Pass(ctx)
	i.once.Do(func() { close(i.backfilled) })
	if err != nil {
		i.log.Warn("initial ingest pass failed", "err", err)
	}
	if res.Events > 0 {
		i.log.Info("backfilled agent logs",
			"events", res.Events, "files", res.Files, "took", time.Since(start).Round(time.Millisecond))
	}

	if every <= 0 {
		return nil
	}
	ticker := time.NewTicker(every)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return i.saveCheckpoint()
		case <-ticker.C:
			res, err := i.Pass(ctx)
			if err != nil {
				i.log.Warn("ingest pass failed", "err", err)
				continue
			}
			if res.Events > 0 {
				i.log.Debug("ingested new turns", "events", res.Events)
			}
		}
	}
}

// Pass reads every source once, from where it last stopped.
func (i *Ingester) Pass(ctx context.Context) (Result, error) {
	var res Result
	for _, src := range i.sources {
		files, err := src.Files()
		if err != nil {
			i.log.Warn("listing source files failed", "source", src.Name(), "err", err)
			continue
		}

		for _, path := range files {
			// Skip files that have not grown since the last pass.
			info, err := os.Stat(path)
			if err != nil {
				continue
			}
			key := src.Name() + "\x00" + path

			i.mu.Lock()
			offset := i.offsets[key]
			i.mu.Unlock()

			if info.Size() == offset {
				continue
			}

			var events int
			next, err := src.Parse(path, offset, func(ev trace.Event) error {
				if ev.Timestamp.IsZero() {
					ev.Timestamp = info.ModTime().UTC()
				}
				if err := i.storeContent(ctx, &ev); err != nil {
					return err
				}
				if err := i.store.Append(ctx, ev); err != nil {
					return err
				}
				events++
				return nil
			})
			if err != nil {
				// Record whatever was consumed before the failure so the next
				// pass resumes there rather than replaying.
				i.log.Warn("parsing log failed", "source", src.Name(), "file", filepath.Base(path), "err", err)
			}

			i.mu.Lock()
			i.offsets[key] = next
			i.mu.Unlock()

			if events > 0 {
				res.Events += events
				res.Files++
			}
		}
	}

	if res.Events > 0 {
		if err := i.saveCheckpoint(); err != nil {
			i.log.Warn("saving ingest checkpoint failed", "err", err)
		}
	}
	return res, nil
}

// Close persists offsets and coverage. Callers that stop polling should call it
// so a clean shutdown does not lose the current run's counts.
func (i *Ingester) Close() error { return i.saveCheckpoint() }

func (i *Ingester) loadCheckpoint() error {
	raw, err := os.ReadFile(i.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	var cp checkpoint
	if err := json.Unmarshal(raw, &cp); err != nil {
		return fmt.Errorf("decode checkpoint: %w", err)
	}
	i.mu.Lock()
	defer i.mu.Unlock()
	if cp.Offsets != nil {
		i.offsets = cp.Offsets
	}
	if cp.Coverage != nil {
		i.coverage = cp.Coverage
	}

	switch {
	case i.recapture:
		i.log.Info("re-reading every transcript to capture content",
			"files", len(i.offsets), "note", "measurements already recorded collapse into the same turns")
		i.offsets = map[string]int64{}
	case i.content && !cp.Content:
		// Loud, because the window closes. Everything already read stays
		// content-free unless somebody asks for it, and the transcripts it
		// would have come from are deleted about a month after they are written.
		i.log.Warn("content capture is on, but transcripts read before now hold none",
			"hint", "run once with -recapture to read them again before the source prunes them")
	}
	return nil
}

func (i *Ingester) saveCheckpoint() error {
	i.snapshotCoverage()

	i.mu.Lock()
	raw, err := json.Marshal(checkpoint{Offsets: i.offsets, Coverage: i.coverage, Content: i.content})
	i.mu.Unlock()
	if err != nil {
		return err
	}

	// Write and rename, so an interrupted save cannot leave a truncated
	// checkpoint that would silently re-ingest or skip records.
	tmp := i.path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, i.path)
}
