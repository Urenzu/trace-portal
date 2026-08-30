package store

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Urenzu/trace-portal/internal/trace"
)

func TestAppendAndRead(t *testing.T) {
	st, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer st.Close()

	now := time.Now().UTC()
	for i, id := range []string{"a", "b", "c"} {
		ev := trace.Event{Type: trace.EventRequest, Timestamp: now, TurnID: id, MessageCount: i}
		if err := st.Append(ev); err != nil {
			t.Fatalf("append: %v", err)
		}
	}

	got, err := st.Events(now)
	if err != nil {
		t.Fatalf("events: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d events, want 3", len(got))
	}
	// Append-only means insertion order is preserved.
	if got[0].TurnID != "a" || got[2].TurnID != "c" {
		t.Errorf("order not preserved: %v %v %v", got[0].TurnID, got[1].TurnID, got[2].TurnID)
	}
}

func TestAppendRotatesByDay(t *testing.T) {
	root := t.TempDir()
	st, err := Open(root)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer st.Close()

	day1 := time.Date(2026, 8, 27, 23, 59, 0, 0, time.UTC)
	day2 := day1.Add(2 * time.Minute)

	for _, ts := range []time.Time{day1, day2} {
		if err := st.Append(trace.Event{Type: trace.EventRequest, Timestamp: ts}); err != nil {
			t.Fatalf("append: %v", err)
		}
	}

	for _, name := range []string{"2026-08-27.jsonl", "2026-08-28.jsonl"} {
		if _, err := os.Stat(filepath.Join(root, "events", name)); err != nil {
			t.Errorf("expected %s: %v", name, err)
		}
	}
	if got, _ := st.Events(day1); len(got) != 1 {
		t.Errorf("day1 has %d events, want 1", len(got))
	}
}

func TestEventsMissingDayIsEmpty(t *testing.T) {
	st, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer st.Close()

	got, err := st.Events(time.Now())
	if err != nil {
		t.Errorf("unexpected error for a day with no traces: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("got %d events, want 0", len(got))
	}
}

func TestBlobRoundTripIsContentAddressed(t *testing.T) {
	st, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer st.Close()

	payload := []byte(`{"messages":[{"role":"user","content":"hello"}]}`)
	ref, err := st.PutBlob(payload)
	if err != nil {
		t.Fatalf("put: %v", err)
	}

	// Identical payloads dedupe to one blob.
	again, err := st.PutBlob(payload)
	if err != nil {
		t.Fatalf("put again: %v", err)
	}
	if again != ref {
		t.Errorf("same payload got refs %s and %s", ref, again)
	}
	other, err := st.PutBlob([]byte(`{"messages":[]}`))
	if err != nil {
		t.Fatalf("put other: %v", err)
	}
	if other == ref {
		t.Error("distinct payloads collided")
	}

	got, err := st.GetBlob(ref)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if string(got) != string(payload) {
		t.Errorf("blob = %s, want %s", got, payload)
	}
}

func TestGetBlobRejectsBadRef(t *testing.T) {
	st, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer st.Close()

	if _, err := st.GetBlob("../../etc/passwd"); err == nil {
		t.Error("expected an error for a malformed blob ref")
	}
}
