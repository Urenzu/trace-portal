package store

import (
	"context"
	"testing"
	"time"

	"github.com/Urenzu/trace-portal/internal/identity"
	"github.com/Urenzu/trace-portal/internal/trace"
)

func day(t *testing.T, s *Store, ts time.Time) []trace.Event {
	t.Helper()
	events, err := s.Events(context.Background(), ts)
	if err != nil {
		t.Fatalf("read events: %v", err)
	}
	return events
}

// Every event written through a store must come out attributed. Nothing else in
// this model has that property — a measurement absent today can be recovered by
// re-reading the transcript tomorrow, but nothing in a transcript says who ran
// it, so a turn written without an owner never gets one.
func TestAppendStampsIdentityOnEveryEvent(t *testing.T) {
	dir := t.TempDir()
	st, err := Open(dir)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer st.Close()

	ts := time.Date(2026, 8, 28, 10, 0, 0, 0, time.UTC)
	if err := st.Append(context.Background(), trace.Event{Type: trace.EventResponse, Timestamp: ts, SessionID: "s1"}); err != nil {
		t.Fatalf("append: %v", err)
	}

	events := day(t, st, ts)
	if len(events) != 1 {
		t.Fatalf("want 1 event, got %d", len(events))
	}
	if !events[0].Identity.Attributed() {
		t.Fatalf("event written with no owner: %+v", events[0].Identity)
	}
	if events[0].TenantID != identity.LocalTenant {
		t.Fatalf("want the local tenant, got %q", events[0].TenantID)
	}
	if events[0].MachineID == "" {
		t.Fatal("no machine id stamped")
	}
}

// A server receiving a collector's batch must keep the identity the collector
// stamped. If the store overwrote it with its own enrollment, every customer's
// turns would be attributed to the server — which is the same bug as having no
// identity at all, only harder to notice because the column is populated.
func TestAppendPreservesAnIdentityTheEventAlreadyCarries(t *testing.T) {
	dir := t.TempDir()
	st, err := Open(dir)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer st.Close()

	ts := time.Date(2026, 8, 28, 11, 0, 0, 0, time.UTC)
	shipped := trace.Identity{TenantID: "t_acme", UserID: "u_dana", MachineID: "m_laptop"}
	if err := st.Append(context.Background(), trace.Event{
		Type:      trace.EventResponse,
		Timestamp: ts,
		SessionID: "s2",
		Identity:  shipped,
	}); err != nil {
		t.Fatalf("append: %v", err)
	}

	events := day(t, st, ts)
	if len(events) != 1 {
		t.Fatalf("want 1 event, got %d", len(events))
	}
	if events[0].Identity != shipped {
		t.Fatalf("store rewrote a collector's identity: want %+v, got %+v", shipped, events[0].Identity)
	}
}

// A partially stamped event fills only what is missing. Two sources can observe
// one call and only one may know the identity, which is the same merge rule the
// rest of the event model uses.
func TestAppendFillsOnlyTheMissingIdentityFields(t *testing.T) {
	dir := t.TempDir()
	st, err := Open(dir)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer st.Close()

	ts := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	if err := st.Append(context.Background(), trace.Event{
		Type:      trace.EventResponse,
		Timestamp: ts,
		SessionID: "s3",
		Identity:  trace.Identity{UserID: "u_dana"},
	}); err != nil {
		t.Fatalf("append: %v", err)
	}

	got := day(t, st, ts)[0].Identity
	if got.UserID != "u_dana" {
		t.Fatalf("known user overwritten: %+v", got)
	}
	if got.TenantID != identity.LocalTenant || got.MachineID == "" {
		t.Fatalf("missing fields not filled: %+v", got)
	}
}
