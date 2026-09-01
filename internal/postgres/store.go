// Package postgres implements the hot window for a server deployment.
//
// It holds the days that are still being written to, and nothing else. Once a
// day is complete, compaction rewrites it into a Parquet partition plus rollups
// and the database rows become redundant — the same division the local tool
// makes between its JSONL log and its partitions, for the same reason.
//
// That division is worth defending against the instinct to put everything in
// SQL. Measured on 365 days of heavy use, a full aggregate is 4,345 ms read
// row-wise and 3 ms read from the rollups, and the archive is 298 MB as JSON
// against 6.2 MB as Parquet. Postgres is here because object storage cannot
// append and because a server needs concurrent writers — not because it is a
// better place to answer "what did the last year cost".
package postgres

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Urenzu/trace-portal/internal/trace"
)

// Store is one tenant's hot window.
//
// Scoped to a tenant rather than shared across them, and the tenant id is
// supplied once at construction from an authenticated credential. Every
// statement below carries `tenant_id = $1` as a consequence of that, not as
// something a caller has to remember — which is the same property the
// filesystem layout gives by putting the tenant in the path. A query here
// cannot accidentally omit the predicate, because no method takes a tenant.
type Store struct {
	pool     *pgxpool.Pool
	tenantID string
	identity trace.Identity

	// owned reports whether closing this Store should close the pool. A server
	// shares one pool across many tenants; a test that opened its own does not.
	owned bool
}

// Config describes a connection.
type Config struct {
	// URL is a libpq connection string, e.g.
	// postgres://user:pass@host:5432/trace_portal?sslmode=disable
	URL string

	// MaxConns bounds the pool. Zero uses pgx's default, which is derived from
	// the number of CPUs.
	MaxConns int32
}

// Pool opens a connection pool and applies the schema.
//
// The pool is shared across tenants: connections are the scarce resource on a
// database server, and a pool per tenant would exhaust them long before the
// data did. Isolation is in the statements, which always carry the tenant, not
// in the connection.
func Pool(ctx context.Context, cfg Config) (*pgxpool.Pool, error) {
	pc, err := pgxpool.ParseConfig(cfg.URL)
	if err != nil {
		return nil, fmt.Errorf("parse postgres url: %w", err)
	}
	if cfg.MaxConns > 0 {
		pc.MaxConns = cfg.MaxConns
	}

	pool, err := pgxpool.NewWithConfig(ctx, pc)
	if err != nil {
		return nil, fmt.Errorf("connect to postgres: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping postgres: %w", err)
	}
	if err := migrate(ctx, pool); err != nil {
		pool.Close()
		return nil, err
	}
	return pool, nil
}

// Open builds a Store for one tenant over an existing pool.
func Open(pool *pgxpool.Pool, tenantID string, id trace.Identity) (*Store, error) {
	if pool == nil {
		return nil, errors.New("postgres store needs a pool")
	}
	if tenantID == "" {
		return nil, errors.New("postgres store needs a tenant")
	}
	return &Store{pool: pool, tenantID: tenantID, identity: id}, nil
}

// Connect opens a pool and a Store together, for a caller that has only one
// tenant — a test, or a single-tenant deployment.
func Connect(ctx context.Context, cfg Config, tenantID string, id trace.Identity) (*Store, error) {
	pool, err := Pool(ctx, cfg)
	if err != nil {
		return nil, err
	}
	s, err := Open(pool, tenantID, id)
	if err != nil {
		pool.Close()
		return nil, err
	}
	s.owned = true
	return s, nil
}

// schema is applied on every start.
//
// Written to be safe to re-run rather than versioned with a migration tool.
// There is one table shape and it is append-only; when that stops being true a
// real migration story is needed, and pretending to have one now would be
// ceremony rather than safety.
const schema = `
CREATE TABLE IF NOT EXISTS events (
    tenant_id  text        NOT NULL,
    -- The content hash of the event. It is the primary key together with the
    -- tenant, which is what makes ingest idempotent: a collector that cannot
    -- tell whether a batch landed is required to send it again, and doing so
    -- must not double anything.
    event_hash bytea       NOT NULL,
    ts         timestamptz NOT NULL,
    -- The UTC day, stored rather than derived so a day lookup is an index seek
    -- instead of a function over every row.
    day        date        NOT NULL,
    payload    jsonb       NOT NULL,
    PRIMARY KEY (tenant_id, event_hash)
);

-- Every read is "this tenant, these days", in time order.
CREATE INDEX IF NOT EXISTS events_tenant_day_ts ON events (tenant_id, day, ts);

CREATE TABLE IF NOT EXISTS blobs (
    tenant_id text  NOT NULL,
    -- The payload hash. Scoped by tenant on purpose: hashing globally would
    -- dedup identical payloads across customers, which sounds like a saving and
    -- is actually a disclosure — the existence of a blob becomes observable to
    -- anyone who can guess its hash.
    ref       text  NOT NULL,
    payload   bytea NOT NULL,
    stored_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, ref)
);
`

func migrate(ctx context.Context, pool *pgxpool.Pool) error {
	if _, err := pool.Exec(ctx, schema); err != nil {
		return fmt.Errorf("apply schema: %w", err)
	}
	return nil
}

// Identity implements eventstore.Store.
func (s *Store) Identity() trace.Identity { return s.identity }

// Append implements eventstore.Store.
//
// Idempotent by content hash. Two identical events collapse to one row, so a
// replayed batch costs a write that does nothing rather than a duplicate. The
// local file store keeps both copies and relies on message-id keying at read
// time to collapse them; both are correct, and here the database can do it at
// write time for free.
func (s *Store) Append(ctx context.Context, ev trace.Event) error {
	ev.Identity.Merge(s.identity)

	payload, err := json.Marshal(ev)
	if err != nil {
		return fmt.Errorf("encode event: %w", err)
	}
	sum := sha256.Sum256(payload)
	ts := ev.Timestamp.UTC()

	_, err = s.pool.Exec(ctx, `
		INSERT INTO events (tenant_id, event_hash, ts, day, payload)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (tenant_id, event_hash) DO NOTHING`,
		s.tenantID, sum[:], ts, ts.Format("2006-01-02"), payload)
	if err != nil {
		return fmt.Errorf("append event: %w", err)
	}
	return nil
}

// AppendBatch writes many events in one round trip.
//
// Ingest arrives in batches, and a round trip per event is what makes a
// catching-up collector slow — a laptop back from a week offline ships tens of
// thousands of events, and at one round trip each that is minutes of latency
// spent on nothing.
func (s *Store) AppendBatch(ctx context.Context, events []trace.Event) (int, error) {
	if len(events) == 0 {
		return 0, nil
	}

	batch := &pgx.Batch{}
	for _, ev := range events {
		ev.Identity.Merge(s.identity)
		payload, err := json.Marshal(ev)
		if err != nil {
			return 0, fmt.Errorf("encode event: %w", err)
		}
		sum := sha256.Sum256(payload)
		ts := ev.Timestamp.UTC()
		batch.Queue(`
			INSERT INTO events (tenant_id, event_hash, ts, day, payload)
			VALUES ($1, $2, $3, $4, $5)
			ON CONFLICT (tenant_id, event_hash) DO NOTHING`,
			s.tenantID, sum[:], ts, ts.Format("2006-01-02"), payload)
	}

	results := s.pool.SendBatch(ctx, batch)
	defer results.Close()
	for range events {
		if _, err := results.Exec(); err != nil {
			return 0, fmt.Errorf("append batch: %w", err)
		}
	}
	return len(events), nil
}

// Events implements eventstore.Store.
func (s *Store) Events(ctx context.Context, day time.Time) ([]trace.Event, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT payload FROM events
		WHERE tenant_id = $1 AND day = $2
		ORDER BY ts`,
		s.tenantID, day.UTC().Format("2006-01-02"))
	if err != nil {
		return nil, fmt.Errorf("read day: %w", err)
	}
	return scanEvents(rows)
}

// EventsRange implements eventstore.Store.
func (s *Store) EventsRange(ctx context.Context, from, to time.Time) ([]trace.Event, error) {
	from, to = from.UTC(), to.UTC()
	if to.Before(from) {
		from, to = to, from
	}
	// Filtered on the day column rather than on ts so the index is used, then
	// on ts for the partial days at each end.
	rows, err := s.pool.Query(ctx, `
		SELECT payload FROM events
		WHERE tenant_id = $1 AND day BETWEEN $2 AND $3 AND ts >= $4 AND ts <= $5
		ORDER BY ts`,
		s.tenantID,
		from.Format("2006-01-02"), to.Format("2006-01-02"),
		from, to)
	if err != nil {
		return nil, fmt.Errorf("read range: %w", err)
	}
	return scanEvents(rows)
}

// Days implements eventstore.Store.
func (s *Store) Days(ctx context.Context) ([]time.Time, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT DISTINCT day FROM events WHERE tenant_id = $1 ORDER BY day`, s.tenantID)
	if err != nil {
		return nil, fmt.Errorf("list days: %w", err)
	}
	defer rows.Close()

	var days []time.Time
	for rows.Next() {
		var d time.Time
		if err := rows.Scan(&d); err != nil {
			return nil, err
		}
		days = append(days, time.Date(d.Year(), d.Month(), d.Day(), 0, 0, 0, 0, time.UTC))
	}
	return days, rows.Err()
}

// DropDay removes a compacted day.
//
// Called only after a partition is durable, and it is not a violation of
// "nothing here ever deletes an ingested trace": the turns are in the Parquet
// partition, which is the durable form. What is dropped is the redundant copy
// in the write-ahead window.
func (s *Store) DropDay(ctx context.Context, day time.Time) error {
	_, err := s.pool.Exec(ctx, `
		DELETE FROM events WHERE tenant_id = $1 AND day = $2`,
		s.tenantID, day.UTC().Format("2006-01-02"))
	if err != nil {
		return fmt.Errorf("drop day: %w", err)
	}
	return nil
}

// Put implements eventstore.Blobs.
func (s *Store) Put(ctx context.Context, payload []byte) (string, error) {
	sum := sha256.Sum256(payload)
	ref := hex.EncodeToString(sum[:])
	_, err := s.pool.Exec(ctx, `
		INSERT INTO blobs (tenant_id, ref, payload) VALUES ($1, $2, $3)
		ON CONFLICT (tenant_id, ref) DO NOTHING`,
		s.tenantID, ref, payload)
	if err != nil {
		return "", fmt.Errorf("store blob: %w", err)
	}
	return ref, nil
}

// Get implements eventstore.Blobs.
//
// A missing blob returns fs.ErrNotExist so callers can distinguish it from a
// failure without knowing which storage backend they are on — the API turns
// that into a 404, and a database error into a 500.
func (s *Store) Get(ctx context.Context, ref string) ([]byte, error) {
	if err := validBlobRef(ref); err != nil {
		return nil, err
	}
	var payload []byte
	err := s.pool.QueryRow(ctx, `
		SELECT payload FROM blobs WHERE tenant_id = $1 AND ref = $2`,
		s.tenantID, ref).Scan(&payload)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fs.ErrNotExist
	}
	if err != nil {
		return nil, fmt.Errorf("read blob: %w", err)
	}
	return payload, nil
}

// validBlobRef mirrors the check the file store makes.
//
// The file store needed it because a reference becomes a path, and this one
// does not — a parameterised query cannot be escaped by a value. It is here
// anyway so that the two backends refuse exactly the same inputs: a reference
// that works against one and not the other is a bug that only appears in
// production.
func validBlobRef(ref string) error {
	if len(ref) != 64 {
		return fmt.Errorf("blob reference must be 64 hex characters")
	}
	for i := 0; i < len(ref); i++ {
		c := ref[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return fmt.Errorf("blob reference must be 64 hex characters")
		}
	}
	return nil
}

// Close releases the pool if this Store owns it.
func (s *Store) Close() error {
	if s.owned && s.pool != nil {
		s.pool.Close()
	}
	return nil
}

func scanEvents(rows pgx.Rows) ([]trace.Event, error) {
	defer rows.Close()
	var events []trace.Event
	for rows.Next() {
		var payload []byte
		if err := rows.Scan(&payload); err != nil {
			return nil, err
		}
		var ev trace.Event
		if err := json.Unmarshal(payload, &ev); err != nil {
			// One unreadable row must not lose the rest of the day. The event
			// model is deliberately additive, so this should be impossible;
			// counting it as lost beats returning nothing.
			continue
		}
		events = append(events, ev)
	}
	return events, rows.Err()
}
