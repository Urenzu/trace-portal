// Package store persists trace events. The hot path only ever appends: narrow
// events go to a daily JSONL file, full payloads go to a content-addressed blob
// store that the UI reads lazily.
package store

import (
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Urenzu/trace-portal/internal/trace"
)

// Store owns the trace directory layout:
//
//	<root>/events/2026-08-28.jsonl
//	<root>/blobs/ab/cdef...json.gz
type Store struct {
	root string

	mu   sync.Mutex
	day  string
	file *os.File
	enc  *json.Encoder
}

// Open prepares the directory layout under root, creating it if needed.
func Open(root string) (*Store, error) {
	for _, dir := range []string{"events", "blobs"} {
		if err := os.MkdirAll(filepath.Join(root, dir), 0o755); err != nil {
			return nil, fmt.Errorf("create %s dir: %w", dir, err)
		}
	}
	return &Store{root: root}, nil
}

// Append writes one event to today's JSONL file, rotating at UTC midnight.
func (s *Store) Append(ev trace.Event) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	day := ev.Timestamp.UTC().Format("2006-01-02")
	if s.file == nil || s.day != day {
		if err := s.rotateLocked(day); err != nil {
			return err
		}
	}
	if err := s.enc.Encode(ev); err != nil {
		return fmt.Errorf("encode event: %w", err)
	}
	return nil
}

func (s *Store) rotateLocked(day string) error {
	if s.file != nil {
		s.file.Close()
		s.file, s.enc = nil, nil
	}
	path := filepath.Join(s.root, "events", day+".jsonl")
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("open event log: %w", err)
	}
	s.file, s.enc, s.day = f, json.NewEncoder(f), day
	return nil
}

// PutBlob stores a payload and returns its reference (the content hash). Blobs
// are content-addressed, so identical payloads are written once.
func (s *Store) PutBlob(payload []byte) (string, error) {
	sum := sha256.Sum256(payload)
	ref := hex.EncodeToString(sum[:])
	path := s.blobPath(ref)

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", fmt.Errorf("create blob dir: %w", err)
	}
	if _, err := os.Stat(path); err == nil {
		return ref, nil // already stored
	}

	// Write to a temp file first so a crash never leaves a truncated blob
	// under a hash that claims to describe complete content.
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return "", fmt.Errorf("create temp blob: %w", err)
	}
	defer os.Remove(tmp.Name())

	zw := gzip.NewWriter(tmp)
	if _, err := zw.Write(payload); err != nil {
		tmp.Close()
		return "", fmt.Errorf("write blob: %w", err)
	}
	if err := zw.Close(); err != nil {
		tmp.Close()
		return "", fmt.Errorf("flush blob: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return "", fmt.Errorf("close blob: %w", err)
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return "", fmt.Errorf("commit blob: %w", err)
	}
	return ref, nil
}

// GetBlob returns a previously stored payload.
func (s *Store) GetBlob(ref string) ([]byte, error) {
	if len(ref) != 64 {
		return nil, fmt.Errorf("invalid blob ref %q", ref)
	}
	f, err := os.Open(s.blobPath(ref))
	if err != nil {
		return nil, err
	}
	defer f.Close()

	zr, err := gzip.NewReader(f)
	if err != nil {
		return nil, fmt.Errorf("open blob %s: %w", ref, err)
	}
	defer zr.Close()
	return readAll(zr)
}

func (s *Store) blobPath(ref string) string {
	return filepath.Join(s.root, "blobs", ref[:2], ref[2:]+".json.gz")
}

// Events reads back every event for a UTC day, oldest first. Compaction and the
// query API will replace this, but it keeps the pipeline testable end to end.
func (s *Store) Events(day time.Time) ([]trace.Event, error) {
	path := filepath.Join(s.root, "events", day.UTC().Format("2006-01-02")+".jsonl")
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()

	var events []trace.Event
	dec := json.NewDecoder(f)
	for dec.More() {
		var ev trace.Event
		if err := dec.Decode(&ev); err != nil {
			return events, fmt.Errorf("decode event log: %w", err)
		}
		events = append(events, ev)
	}
	return events, nil
}

// EventsRange reads every event between from and to inclusive, walking one
// daily file per day. Days with no traces are skipped rather than erroring.
func (s *Store) EventsRange(from, to time.Time) ([]trace.Event, error) {
	from, to = from.UTC(), to.UTC()
	if to.Before(from) {
		from, to = to, from
	}

	var events []trace.Event
	day := time.Date(from.Year(), from.Month(), from.Day(), 0, 0, 0, 0, time.UTC)
	last := time.Date(to.Year(), to.Month(), to.Day(), 0, 0, 0, 0, time.UTC)
	for !day.After(last) {
		dayEvents, err := s.Events(day)
		if err != nil {
			return events, err
		}
		events = append(events, dayEvents...)
		day = day.AddDate(0, 0, 1)
	}
	return events, nil
}

// Days lists the UTC days that have an event log, oldest first.
func (s *Store) Days() ([]time.Time, error) {
	entries, err := os.ReadDir(filepath.Join(s.root, "events"))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}

	var days []time.Time
	for _, e := range entries {
		name := strings.TrimSuffix(e.Name(), ".jsonl")
		if name == e.Name() {
			continue
		}
		day, err := time.ParseInLocation("2006-01-02", name, time.UTC)
		if err != nil {
			continue
		}
		days = append(days, day)
	}
	sort.Slice(days, func(i, j int) bool { return days[i].Before(days[j]) })
	return days, nil
}

// Close flushes and releases the current event log.
func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.file == nil {
		return nil
	}
	err := s.file.Close()
	s.file, s.enc = nil, nil
	return err
}

// Root is the directory holding every event log and blob.
func (s *Store) Root() string { return s.root }
