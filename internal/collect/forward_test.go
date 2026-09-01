package collect

import (
	"context"
	"testing"
	"time"

	"github.com/Urenzu/trace-portal/internal/auth"
	"github.com/Urenzu/trace-portal/internal/store"
	"github.com/Urenzu/trace-portal/internal/trace"
)

func localStore(t *testing.T) (*store.Store, string) {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(dir)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st, dir
}

// The whole split, end to end: events captured locally reach the server's
// tenant store, and the local archive keeps them too.
func TestForwarderShipsTheLocalArchive(t *testing.T) {
	tok := auth.Token{ID: "tok_1", Kind: auth.KindCollector, TenantID: "t_acme", UserID: "u_dana", MachineID: "m_laptop"}
	r := newRig(t, fixedAuth{"secret": tok})

	st, dir := localStore(t)
	ts := time.Now().UTC().Add(-2 * time.Hour)
	for i, msg := range []string{"msg_1", "msg_2", "msg_3"} {
		if err := st.Append(context.Background(), event(ts.Add(time.Duration(i)*time.Minute), "s1", msg)); err != nil {
			t.Fatalf("append: %v", err)
		}
	}

	shipper, err := NewShipper(r.server.URL, "secret", r.server.Client(), discardLogger())
	if err != nil {
		t.Fatalf("shipper: %v", err)
	}
	fwd, err := NewForwarder(st, shipper, dir, discardLogger())
	if err != nil {
		t.Fatalf("forwarder: %v", err)
	}

	n, err := fwd.Pass(context.Background())
	if err != nil {
		t.Fatalf("pass: %v", err)
	}
	if n != 3 {
		t.Fatalf("shipped %d, want 3", n)
	}

	remote, err := r.registry.For("t_acme")
	if err != nil {
		t.Fatalf("open tenant: %v", err)
	}
	got, err := remote.Store.Events(context.Background(), ts)
	if err != nil {
		t.Fatalf("read remote: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("server holds %d events, want 3", len(got))
	}

	// The local archive is untouched by shipping. This is the property that
	// makes a shipping failure cost nothing, so it is worth asserting rather
	// than assuming.
	local, err := st.Events(context.Background(), ts)
	if err != nil {
		t.Fatalf("read local: %v", err)
	}
	if len(local) != 3 {
		t.Fatalf("local archive holds %d events after shipping, want 3", len(local))
	}
}

// A second pass with nothing new must not re-ship the whole archive. The
// overlap re-sends a little on purpose; it must not re-send everything.
func TestForwarderCheckpointStopsResendingEverything(t *testing.T) {
	tok := auth.Token{ID: "tok_1", Kind: auth.KindCollector, TenantID: "t_acme", UserID: "u_dana", MachineID: "m_laptop"}
	r := newRig(t, fixedAuth{"secret": tok})

	st, dir := localStore(t)
	old := time.Now().UTC().Add(-72 * time.Hour)
	for _, msg := range []string{"msg_1", "msg_2"} {
		if err := st.Append(context.Background(), event(old, "s1", msg)); err != nil {
			t.Fatalf("append: %v", err)
		}
	}

	shipper, _ := NewShipper(r.server.URL, "secret", r.server.Client(), discardLogger())
	fwd, _ := NewForwarder(st, shipper, dir, discardLogger())

	first, err := fwd.Pass(context.Background())
	if err != nil {
		t.Fatalf("first pass: %v", err)
	}
	if first != 2 {
		t.Fatalf("first pass shipped %d, want 2", first)
	}

	second, err := fwd.Pass(context.Background())
	if err != nil {
		t.Fatalf("second pass: %v", err)
	}
	if second != 0 {
		t.Fatalf("second pass re-shipped %d events; the checkpoint did not hold", second)
	}
}

// Duplicates are the price of never losing a turn, and they must be harmless.
// Turns are keyed by the API's message id, so an exchange shipped twice has to
// collapse into one turn rather than double a total.
func TestReshippingDoesNotDoubleCount(t *testing.T) {
	tok := auth.Token{ID: "tok_1", Kind: auth.KindCollector, TenantID: "t_acme", UserID: "u_dana", MachineID: "m_laptop"}
	r := newRig(t, fixedAuth{"secret": tok})

	ts := time.Now().UTC().Add(-time.Hour)
	events := []trace.Event{event(ts, "s1", "msg_1"), event(ts, "s1", "msg_2")}

	postBatch(t, r, "secret", Batch{Events: events})
	postBatch(t, r, "secret", Batch{Events: events})

	remote, _ := r.registry.For("t_acme")
	stored, err := remote.Store.Events(context.Background(), ts)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	// The raw log is append-only, so both copies are on disk — that is correct
	// and is what makes a replayed batch safe to accept.
	if len(stored) != 4 {
		t.Fatalf("append-only log holds %d records, want 4", len(stored))
	}

	// What must not double is the turns those records fold into.
	turns, err := remote.Compactor.TurnsRange(ts.Add(-time.Hour), ts.Add(time.Hour))
	if err != nil {
		t.Fatalf("turns: %v", err)
	}
	if len(turns) != 2 {
		t.Fatalf("re-shipping produced %d turns from 2 exchanges", len(turns))
	}
}

// Pointing a collector at a different server must ship the whole archive to it,
// not just what happens to be new. Otherwise the new server holds a history
// with a permanent hole at the front.
func TestChangingServerResetsTheCheckpoint(t *testing.T) {
	tok := auth.Token{ID: "tok_1", Kind: auth.KindCollector, TenantID: "t_acme", UserID: "u_dana", MachineID: "m_laptop"}
	first := newRig(t, fixedAuth{"secret": tok})
	second := newRig(t, fixedAuth{"secret": tok})

	st, dir := localStore(t)
	ts := time.Now().UTC().Add(-4 * time.Hour)
	for _, msg := range []string{"msg_1", "msg_2"} {
		if err := st.Append(context.Background(), event(ts, "s1", msg)); err != nil {
			t.Fatalf("append: %v", err)
		}
	}

	shipA, _ := NewShipper(first.server.URL, "secret", first.server.Client(), discardLogger())
	fwdA, _ := NewForwarder(st, shipA, dir, discardLogger())
	if n, err := fwdA.Pass(context.Background()); err != nil || n != 2 {
		t.Fatalf("first server: shipped %d, err %v", n, err)
	}

	shipB, _ := NewShipper(second.server.URL, "secret", second.server.Client(), discardLogger())
	fwdB, _ := NewForwarder(st, shipB, dir, discardLogger())
	n, err := fwdB.Pass(context.Background())
	if err != nil {
		t.Fatalf("second server: %v", err)
	}
	if n != 2 {
		t.Fatalf("the new server received %d events, want the whole archive (2)", n)
	}
}
