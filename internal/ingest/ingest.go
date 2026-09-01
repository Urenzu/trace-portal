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
	sources []source.Source
	log     *slog.Logger
	path    string

	mu       sync.Mutex
	offsets  map[string]int64
	coverage map[string]*source.Coverage
}

// checkpoint is what survives a restart.
type checkpoint struct {
	Offsets  map[string]int64            `json:"offsets"`
	Coverage map[string]*source.Coverage `json:"coverage,omitempty"`
}

// New builds an Ingester writing into st, checkpointing under dataDir.
func New(st eventstore.Store, dataDir string, log *slog.Logger, sources ...source.Source) *Ingester {
	if log == nil {
		log = slog.Default()
	}
	return &Ingester{
		store:    st,
		sources:  sources,
		log:      log,
		path:     filepath.Join(dataDir, checkpointFile),
		offsets:  map[string]int64{},
		coverage: map[string]*source.Coverage{},
	}
}

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
	return nil
}

func (i *Ingester) saveCheckpoint() error {
	i.snapshotCoverage()

	i.mu.Lock()
	raw, err := json.Marshal(checkpoint{Offsets: i.offsets, Coverage: i.coverage})
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
