package postgres

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"testing"
	"time"

	"github.com/Urenzu/trace-portal/internal/eventstore"
	"github.com/Urenzu/trace-portal/internal/trace"
)

// These run against a real Postgres, not a fake.
//
// A fake would prove nothing here. Everything worth testing in this package is
// behaviour Postgres provides — ON CONFLICT making ingest idempotent, a date
// column matching what Go computed as the UTC day, jsonb round-tripping an event
// without losing a field — and a stand-in would simply agree with whatever the
// code already believed.
//
//	docker compose up -d postgres
//	TRACE_PORTAL_TEST_POSTGRES=postgres://trace:trace@localhost:5432/trace_portal?sslmode=disable go test ./internal/postgres/
func testStore(t *testing.T, tenantID string) *Store {
	t.Helper()
	url := os.Getenv("TRACE_PORTAL_TEST_POSTGRES")
	if url == "" {
		t.Skip("set TRACE_PORTAL_TEST_POSTGRES to run these (docker compose up -d postgres)")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	s, err := Connect(ctx, Config{URL: url}, tenantID, trace.Identity{
		TenantID: tenantID, UserID: "u_test", MachineID: "m_test",
	})
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() {
		// Each test owns a tenant of its own, so cleaning up is deleting that
		// tenant's rows rather than truncating a shared table — which also means
		// these can run in parallel with each other and against a database
		// somebody is using.
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_, _ = s.pool.Exec(ctx, `DELETE FROM events WHERE tenant_id = $1`, tenantID)
		_, _ = s.pool.Exec(ctx, `DELETE FROM blobs WHERE tenant_id = $1`, tenantID)
		s.Close()
	})
	return s
}

// The Postgres store must satisfy the same interface the file store does. If it
// does not, the failure is a compile error here rather than a runtime surprise
// on the one deployment that uses it.
var _ eventstore.Store = (*Store)(nil)
var _ eventstore.Blobs = (*Store)(nil)

func event(ts time.Time, session, msg string) trace.Event {
	return trace.Event{
		Type:      trace.EventResponse,
		Timestamp: ts,
		SessionID: session,
		TurnID:    "turn-" + msg,
		MessageID: msg,
		Model:     "claude-opus-5",
		Usage: &trace.Usage{
			InputTokens: 12, OutputTokens: 34,
			CacheCreationInputTokens: 100, CacheReadInputTokens: 9000,
			CacheCreation: &trace.CacheCreation{Ephemeral5mInputTokens: 100},
		},
		Project:   "trace-portal",
		ProjectID: "abc123",
		GitBranch: "main",
	}
}

func TestAppendAndReadBack(t *testing.T) {
	s := testStore(t, "t_roundtrip")
	ctx := context.Background()

	ts := time.Date(2026, 8, 28, 10, 0, 0, 0, time.UTC)
	for i, msg := range []string{"msg_1", "msg_2", "msg_3"} {
		if err := s.Append(ctx, event(ts.Add(time.Duration(i)*time.Minute), "s1", msg)); err != nil {
			t.Fatalf("append: %v", err)
		}
	}

	got, err := s.Events(ctx, ts)
	if err != nil {
		t.Fatalf("events: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("read %d events, want 3", len(got))
	}
	// Oldest first, like the file store.
	if got[0].MessageID != "msg_1" || got[2].MessageID != "msg_3" {
		t.Fatalf("wrong order: %s then %s", got[0].MessageID, got[2].MessageID)
	}

	// The event model is what everything downstream reads. A field lost in the
	// jsonb round trip would show up as a quietly wrong dashboard rather than
	// as an error, which is the failure mode this project spends most of its
	// design avoiding.
	first := got[0]
	if first.Usage == nil {
		t.Fatal("usage lost in the round trip")
	}
	if first.Usage.CacheReadInputTokens != 9000 {
		t.Errorf("cache reads = %d, want 9000", first.Usage.CacheReadInputTokens)
	}
	if first.Usage.CacheCreation == nil || first.Usage.CacheCreation.Ephemeral5mInputTokens != 100 {
		t.Errorf("cache write TTL split lost: %+v", first.Usage.CacheCreation)
	}
	if first.Project != "trace-portal" || first.GitBranch != "main" {
		t.Errorf("attribution lost: %+v", first)
	}
	if !first.Identity.Attributed() {
		t.Errorf("identity lost: %+v", first.Identity)
	}
}

// A collector that cannot tell whether a batch landed is required to send it
// again, so appending the same event twice must be one event's worth of truth.
// The file store relies on message-id keying at read time; here the database
// can do it at write time, and this is the test that says it does.
func TestAppendIsIdempotent(t *testing.T) {
	s := testStore(t, "t_idempotent")
	ctx := context.Background()

	ts := time.Date(2026, 8, 28, 11, 0, 0, 0, time.UTC)
	ev := event(ts, "s1", "msg_1")

	for i := 0; i < 3; i++ {
		if err := s.Append(ctx, ev); err != nil {
			t.Fatalf("append %d: %v", i, err)
		}
	}

	got, err := s.Events(ctx, ts)
	if err != nil {
		t.Fatalf("events: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("the same event appended 3 times produced %d rows", len(got))
	}
}

// A batch is how ingest actually arrives. A round trip per event is what makes
// a catching-up collector slow, so the batch path exists — and it has to agree
// with the single-append path about what it stored.
func TestAppendBatchMatchesAppend(t *testing.T) {
	s := testStore(t, "t_batch")
	ctx := context.Background()

	ts := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	var events []trace.Event
	for i := 0; i < 50; i++ {
		events = append(events, event(ts.Add(time.Duration(i)*time.Second), "s1", "msg_"+string(rune('a'+i%26))+string(rune('a'+i/26))))
	}

	n, err := s.AppendBatch(ctx, events)
	if err != nil {
		t.Fatalf("batch: %v", err)
	}
	if n != len(events) {
		t.Fatalf("batch reported %d of %d", n, len(events))
	}

	// Replaying the batch must change nothing.
	if _, err := s.AppendBatch(ctx, events); err != nil {
		t.Fatalf("replay: %v", err)
	}

	got, err := s.Events(ctx, ts)
	if err != nil {
		t.Fatalf("events: %v", err)
	}
	if len(got) != len(events) {
		t.Fatalf("stored %d rows for %d distinct events", len(got), len(events))
	}
}

// Days and ranges have to agree with Go's idea of a UTC day, or a turn near
// midnight lands in the wrong partition when compaction reads it back.
func TestDaysAndRangeUseUTCDays(t *testing.T) {
	s := testStore(t, "t_days")
	ctx := context.Background()

	// 23:30 on one day and 00:30 on the next, in UTC. A server running in any
	// other timezone must still see two days here.
	first := time.Date(2026, 8, 28, 23, 30, 0, 0, time.UTC)
	second := time.Date(2026, 8, 29, 0, 30, 0, 0, time.UTC)
	if err := s.Append(ctx, event(first, "s1", "msg_late")); err != nil {
		t.Fatalf("append: %v", err)
	}
	if err := s.Append(ctx, event(second, "s1", "msg_early")); err != nil {
		t.Fatalf("append: %v", err)
	}

	days, err := s.Days(ctx)
	if err != nil {
		t.Fatalf("days: %v", err)
	}
	if len(days) != 2 {
		t.Fatalf("got %d days, want 2: %v", len(days), days)
	}
	if days[0].Format("2006-01-02") != "2026-08-28" || days[1].Format("2006-01-02") != "2026-08-29" {
		t.Fatalf("wrong days: %v", days)
	}
	for _, d := range days {
		if d.Location() != time.UTC {
			t.Errorf("day %v is not UTC", d)
		}
	}

	// Each day holds exactly its own event.
	early, err := s.Events(ctx, second)
	if err != nil {
		t.Fatalf("events: %v", err)
	}
	if len(early) != 1 || early[0].MessageID != "msg_early" {
		t.Fatalf("day boundary is wrong: %+v", early)
	}

	both, err := s.EventsRange(ctx, first.Add(-time.Hour), second.Add(time.Hour))
	if err != nil {
		t.Fatalf("range: %v", err)
	}
	if len(both) != 2 {
		t.Fatalf("range returned %d events, want 2", len(both))
	}
}

// A range that excludes an event by time must exclude it, even when that
// event's day is inside the range. The query filters on the day column for the
// index and on the timestamp for the partial days at each end; dropping the
// second half would silently widen every window.
func TestEventsRangeRespectsPartialDays(t *testing.T) {
	s := testStore(t, "t_partial")
	ctx := context.Background()

	day := time.Date(2026, 8, 28, 0, 0, 0, 0, time.UTC)
	morning := day.Add(9 * time.Hour)
	evening := day.Add(21 * time.Hour)
	if err := s.Append(ctx, event(morning, "s1", "msg_morning")); err != nil {
		t.Fatalf("append: %v", err)
	}
	if err := s.Append(ctx, event(evening, "s1", "msg_evening")); err != nil {
		t.Fatalf("append: %v", err)
	}

	got, err := s.EventsRange(ctx, day.Add(12*time.Hour), day.Add(23*time.Hour))
	if err != nil {
		t.Fatalf("range: %v", err)
	}
	if len(got) != 1 || got[0].MessageID != "msg_evening" {
		t.Fatalf("range ignored the time bounds: %d events %+v", len(got), got)
	}
}

// Two tenants sharing a table must not see each other. This is the same
// property the filesystem gives by putting the tenant in the path; here it is
// the predicate on every statement, and it is worth a test precisely because a
// predicate is the kind of thing that can be omitted.
func TestTenantsAreIsolated(t *testing.T) {
	acme := testStore(t, "t_iso_acme")
	beta := testStore(t, "t_iso_beta")
	ctx := context.Background()

	ts := time.Date(2026, 8, 28, 14, 0, 0, 0, time.UTC)
	if err := acme.Append(ctx, event(ts, "s_acme", "msg_acme")); err != nil {
		t.Fatalf("append acme: %v", err)
	}
	if err := beta.Append(ctx, event(ts, "s_beta", "msg_beta")); err != nil {
		t.Fatalf("append beta: %v", err)
	}

	got, err := acme.Events(ctx, ts)
	if err != nil {
		t.Fatalf("events: %v", err)
	}
	if len(got) != 1 || got[0].SessionID != "s_acme" {
		t.Fatalf("acme sees %d events: %+v", len(got), got)
	}

	days, err := beta.Days(ctx)
	if err != nil {
		t.Fatalf("days: %v", err)
	}
	if len(days) != 1 {
		t.Fatalf("beta sees %d days; tenants are leaking", len(days))
	}
}

// Blob dedup must be per tenant. Hashing globally sounds like a storage saving
// and is actually a disclosure: identical payloads collapse across customers, so
// the existence of a blob becomes observable to anyone who can guess its hash.
func TestBlobDedupIsPerTenant(t *testing.T) {
	acme := testStore(t, "t_blob_acme")
	beta := testStore(t, "t_blob_beta")
	ctx := context.Background()

	payload := []byte(`{"secret":"a payload both tenants happen to have"}`)

	refA, err := acme.Put(ctx, payload)
	if err != nil {
		t.Fatalf("put acme: %v", err)
	}

	// Before beta stores it, that reference must not resolve for beta — even
	// though the bytes are already in the table under another tenant.
	if _, err := beta.Get(ctx, refA); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("one tenant read another's blob by guessing its hash (err = %v)", err)
	}

	refB, err := beta.Put(ctx, payload)
	if err != nil {
		t.Fatalf("put beta: %v", err)
	}
	if refA != refB {
		t.Fatal("the same payload should hash to the same reference")
	}
	got, err := beta.Get(ctx, refB)
	if err != nil {
		t.Fatalf("get beta: %v", err)
	}
	if string(got) != string(payload) {
		t.Fatal("blob did not round-trip")
	}
}

// Both backends must refuse exactly the same references. One that works against
// files and not against Postgres is a bug that only appears in production.
func TestBlobRefValidationMatchesTheFileStore(t *testing.T) {
	for _, bad := range []string{
		"", "short",
		"../../etc/passwd",
		"ABCDEF0123456789abcdef0123456789abcdef0123456789abcdef0123456789", // uppercase
		"zzzzzz0123456789abcdef0123456789abcdef0123456789abcdef0123456789", // not hex
	} {
		if err := validBlobRef(bad); err == nil {
			t.Errorf("validBlobRef(%q) accepted it", bad)
		}
	}
	good := "995d8f73806444b693d0e939cb5b2be06f3c8b54a085a06020d5e6c1c5dac6bb"
	if err := validBlobRef(good); err != nil {
		t.Errorf("validBlobRef rejected a legitimate reference: %v", err)
	}
}

// Dropping a compacted day removes the redundant window copy and nothing else.
func TestDropDayLeavesOtherDays(t *testing.T) {
	s := testStore(t, "t_drop")
	ctx := context.Background()

	first := time.Date(2026, 8, 28, 10, 0, 0, 0, time.UTC)
	second := time.Date(2026, 8, 29, 10, 0, 0, 0, time.UTC)
	if err := s.Append(ctx, event(first, "s1", "msg_1")); err != nil {
		t.Fatalf("append: %v", err)
	}
	if err := s.Append(ctx, event(second, "s1", "msg_2")); err != nil {
		t.Fatalf("append: %v", err)
	}

	if err := s.DropDay(ctx, first); err != nil {
		t.Fatalf("drop: %v", err)
	}

	days, err := s.Days(ctx)
	if err != nil {
		t.Fatalf("days: %v", err)
	}
	if len(days) != 1 || days[0].Format("2006-01-02") != "2026-08-29" {
		t.Fatalf("drop removed the wrong day: %v", days)
	}
}
