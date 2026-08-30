// Package source turns what agent tools already write to disk into trace
// events.
//
// Agent CLIs keep local session logs because they need them for resume and
// context management, and token usage rides along because the tool needs it to
// manage its own context window. Reading those logs gets the same numbers as a
// proxy without sitting in the request path, so a failure here can never stop
// an agent from working.
package source

import (
	"time"

	"github.com/Urenzu/trace-portal/internal/trace"
)

// Emit receives one event. Returning an error aborts the scan.
type Emit func(trace.Event) error

// Source reads one tool's local logs.
type Source interface {
	// Name identifies the source in events and diagnostics.
	Name() string

	// Root is the directory the source reads. It may not exist, which simply
	// means the tool is not installed.
	Root() string

	// Files lists the log files currently present, oldest first.
	Files() ([]string, error)

	// Parse reads one log file starting at byte offset from, emitting an event
	// per completed record, and returns the offset it consumed up to.
	//
	// Only whole lines are consumed: a file being appended to as it is read
	// will end in a partial record, and resuming from the returned offset
	// picks it up once it is complete.
	Parse(path string, from int64, emit Emit) (int64, error)
}

// parseTimestamp accepts the RFC3339 forms these logs use, returning the zero
// time for anything unrecognised rather than failing the record.
func parseTimestamp(raw string) time.Time {
	if raw == "" {
		return time.Time{}
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339} {
		if t, err := time.Parse(layout, raw); err == nil {
			return t.UTC()
		}
	}
	return time.Time{}
}
