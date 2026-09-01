// Package eventstore is the contract between capture and storage.
//
// It exists because the hot path and the analytical path want opposite things.
// Appending wants a medium that accepts one small record at a time, cheaply and
// crash-safely; querying a year wants columns. The design resolves that by
// having both: a hot window that is appended to, and completed days compacted
// into Parquet partitions plus rollups that answer a dashboard in milliseconds.
//
// This interface is only the hot window. Two implementations:
//
//   - files, for the local tool — a JSONL file per UTC day, which is what makes
//     "one binary, nothing to install" true. A local install that needed a
//     database running before it could show you your own dashboard would have
//     given that up.
//   - Postgres, for the server — because object storage cannot append, and a
//     server holding many tenants needs concurrent writers, which a file per
//     day does not give.
//
// What deliberately does *not* vary is everything below the hot window. Parquet
// partitions and rollups are the measured core of the product — a 365-day
// aggregate is 4,345 ms read row-wise and 3 ms read from rollups, on an archive
// that is 298 MB as JSON and 6.2 MB as Parquet. Moving that into SQL would
// trade the thing that makes this fast for the convenience of one storage
// system, so the database holds the window and Parquet holds the history.
package eventstore

import (
	"context"
	"time"

	"github.com/Urenzu/trace-portal/internal/trace"
)

// Store is the hot window: append, and read back by day.
//
// Every method takes a context because one implementation talks to a database
// over a network. The file implementation ignores it, which is honest — a local
// read is not cancellable in any meaningful way — but the signature has to admit
// the possibility or the Postgres one could never cancel a query whose caller
// has gone away.
type Store interface {
	// Append writes one event. Implementations must be idempotent: the same
	// event appended twice is one event's worth of truth, because a collector
	// that is unsure whether a batch landed is required to send it again.
	Append(ctx context.Context, ev trace.Event) error

	// Events returns one UTC day, oldest first.
	Events(ctx context.Context, day time.Time) ([]trace.Event, error)

	// EventsRange returns everything between two instants, inclusive.
	EventsRange(ctx context.Context, from, to time.Time) ([]trace.Event, error)

	// Days lists the UTC days holding events, oldest first.
	Days(ctx context.Context) ([]time.Time, error)

	// Identity is what this store stamps onto events that arrive without one.
	// See the local implementation for why stamping lives in the store rather
	// than at each capture site.
	Identity() trace.Identity

	Close() error
}

// Blobs holds payloads too large to sit inside an event.
//
// Separate from Store because the access pattern is opposite: blobs are written
// once, read almost never, and read whole. Keeping them out of the event stream
// is what lets an analytical query stay narrow no matter what a payload weighs.
type Blobs interface {
	// Put stores a payload and returns its reference.
	//
	// References are content hashes, so an identical payload stored twice is
	// stored once. That dedup must be per tenant: hashing globally means the
	// *existence* of a payload is observable to anyone who can guess its hash,
	// which across tenants is a disclosure. The storage saving was never the
	// point.
	Put(ctx context.Context, payload []byte) (string, error)

	// Get returns a stored payload.
	Get(ctx context.Context, ref string) ([]byte, error)
}
