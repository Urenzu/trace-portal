package collect

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Urenzu/trace-portal/internal/auth"
	"github.com/Urenzu/trace-portal/internal/tenant"
	"github.com/Urenzu/trace-portal/internal/trace"
)

// fixedAuth resolves a bearer secret to a token from a fixed table, standing in
// for the real issuer so these tests exercise ingest rather than sign-in.
type fixedAuth map[string]auth.Token

func (f fixedAuth) Collector(r *http.Request) (auth.Token, error) {
	secret := auth.BearerToken(r.Header.Get("Authorization"))
	tok, ok := f[secret]
	if !ok {
		return auth.Token{}, auth.ErrTokenRejected
	}
	return tok, nil
}

type rig struct {
	server   *httptest.Server
	registry *tenant.Registry
	root     string
}

func newRig(t *testing.T, tokens fixedAuth) *rig {
	t.Helper()
	root := t.TempDir()
	registry, err := tenant.NewPartitioned(root)
	if err != nil {
		t.Fatalf("registry: %v", err)
	}
	t.Cleanup(func() { registry.Close() })

	cs, err := NewServer(registry, tokens, discardLogger(), nil)
	if err != nil {
		t.Fatalf("collect server: %v", err)
	}
	mux := http.NewServeMux()
	cs.Routes(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	return &rig{server: srv, registry: registry, root: root}
}

func event(ts time.Time, session, msg string) trace.Event {
	return trace.Event{
		Type:      trace.EventResponse,
		Timestamp: ts,
		SessionID: session,
		TurnID:    "turn-" + msg,
		MessageID: msg,
		Model:     "claude-opus-5",
		Usage:     &trace.Usage{InputTokens: 10, OutputTokens: 5},
	}
}

// A collector ships, the server stores it under the right tenant, and the
// identity on disk is the one the credential authorises.
func TestCollectStoresUnderTheAuthenticatedTenant(t *testing.T) {
	tok := auth.Token{
		ID: "tok_1", Kind: auth.KindCollector,
		TenantID: "t_acme", UserID: "u_dana", MachineID: "m_laptop",
	}
	r := newRig(t, fixedAuth{"secret": tok})

	shipper, err := NewShipper(r.server.URL, "secret", r.server.Client(), discardLogger())
	if err != nil {
		t.Fatalf("shipper: %v", err)
	}

	ts := time.Now().UTC().Add(-time.Hour)
	res, err := shipper.Send(context.Background(), []trace.Event{event(ts, "s1", "msg_1"), event(ts, "s1", "msg_2")})
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	if res.Accepted != 2 {
		t.Fatalf("accepted %d, want 2", res.Accepted)
	}

	storage, err := r.registry.For("t_acme")
	if err != nil {
		t.Fatalf("open tenant: %v", err)
	}
	events, err := storage.Store.Events(context.Background(), ts)
	if err != nil {
		t.Fatalf("read events: %v", err)
	}
	if len(events) != 2 {
		t.Fatalf("stored %d events, want 2", len(events))
	}
	for _, ev := range events {
		if ev.TenantID != "t_acme" || ev.UserID != "u_dana" || ev.MachineID != "m_laptop" {
			t.Fatalf("wrong identity on a stored event: %+v", ev.Identity)
		}
	}
}

// The attack this endpoint exists to refuse: a modified collector that stamps
// another tenant's id onto its events and ships them.
//
// The events must land under the tenant the credential names, and nothing the
// body said may survive. Merging instead of overwriting — filling only what the
// payload left empty — would let this succeed, which is why the distinction is
// worth a test rather than a comment.
func TestCollectIgnoresIdentityClaimedInTheBody(t *testing.T) {
	tok := auth.Token{
		ID: "tok_1", Kind: auth.KindCollector,
		TenantID: "t_acme", UserID: "u_dana", MachineID: "m_laptop",
	}
	r := newRig(t, fixedAuth{"secret": tok})

	ts := time.Now().UTC().Add(-time.Hour)
	forged := event(ts, "s1", "msg_1")
	forged.Identity = trace.Identity{
		TenantID:  "t_victim",
		UserID:    "u_someone_else",
		MachineID: "m_not_mine",
	}

	res := postBatch(t, r, "secret", Batch{Events: []trace.Event{forged}})
	if res.Accepted != 1 {
		t.Fatalf("accepted %d, want 1", res.Accepted)
	}

	// Nothing may have been written for the tenant named in the body.
	victimDir := filepath.Join(r.root, "tenants", "t_victim")
	if _, err := os.Stat(victimDir); !os.IsNotExist(err) {
		t.Fatalf("a claimed tenant id created storage at %s", victimDir)
	}

	storage, err := r.registry.For("t_acme")
	if err != nil {
		t.Fatalf("open tenant: %v", err)
	}
	events, err := storage.Store.Events(context.Background(), ts)
	if err != nil {
		t.Fatalf("read events: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("stored %d events, want 1", len(events))
	}
	got := events[0].Identity
	if got.TenantID != "t_acme" || got.UserID != "u_dana" || got.MachineID != "m_laptop" {
		t.Fatalf("forged identity survived: %+v", got)
	}
}

// Two tenants must not share a byte. This is the structural half of isolation:
// the roots differ, so a read of one cannot reach the other even if every
// filter above it were removed.
func TestTenantsGetSeparateRoots(t *testing.T) {
	r := newRig(t, fixedAuth{
		"acme-secret": {ID: "tok_a", Kind: auth.KindCollector, TenantID: "t_acme", UserID: "u_a", MachineID: "m_a"},
		"beta-secret": {ID: "tok_b", Kind: auth.KindCollector, TenantID: "t_beta", UserID: "u_b", MachineID: "m_b"},
	})

	ts := time.Now().UTC().Add(-time.Hour)
	postBatch(t, r, "acme-secret", Batch{Events: []trace.Event{event(ts, "s_acme", "msg_a")}})
	postBatch(t, r, "beta-secret", Batch{Events: []trace.Event{event(ts, "s_beta", "msg_b")}})

	acme, err := r.registry.For("t_acme")
	if err != nil {
		t.Fatalf("open acme: %v", err)
	}
	beta, err := r.registry.For("t_beta")
	if err != nil {
		t.Fatalf("open beta: %v", err)
	}
	if acme.Root == beta.Root {
		t.Fatal("two tenants share a storage root")
	}
	if strings.HasPrefix(beta.Root, acme.Root+string(filepath.Separator)) {
		t.Fatal("one tenant's root is nested inside another's")
	}

	acmeEvents, _ := acme.Store.Events(context.Background(), ts)
	if len(acmeEvents) != 1 || acmeEvents[0].SessionID != "s_acme" {
		t.Fatalf("acme sees the wrong data: %+v", acmeEvents)
	}
	betaEvents, _ := beta.Store.Events(context.Background(), ts)
	if len(betaEvents) != 1 || betaEvents[0].SessionID != "s_beta" {
		t.Fatalf("beta sees the wrong data: %+v", betaEvents)
	}
}

// No credential, no ingest.
func TestCollectRefusesWithoutACredential(t *testing.T) {
	r := newRig(t, fixedAuth{})

	body, _ := json.Marshal(Batch{Events: []trace.Event{event(time.Now(), "s1", "msg_1")}})
	resp, err := r.server.Client().Post(r.server.URL+"/api/collect", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("want 401, got %d", resp.StatusCode)
	}
	if resp.Header.Get("WWW-Authenticate") == "" {
		t.Error("a 401 should say what credential it wants")
	}
}

// A timestamp far outside plausible range is refused at write time.
//
// This repository already fixed the read-side version of this: ?days=200000
// walked every day back to 1478, three failed file opens each, 15.5 seconds of
// syscalls from one URL. Accepting an event dated 1478 would recreate exactly
// that cost for every subsequent query, reachable by writing instead of
// reading, and unlike the query it would be permanent.
func TestCollectRejectsImplausibleTimestamps(t *testing.T) {
	tok := auth.Token{ID: "tok_1", Kind: auth.KindCollector, TenantID: "t_acme", UserID: "u_d", MachineID: "m_l"}
	r := newRig(t, fixedAuth{"secret": tok})

	now := time.Now().UTC()
	batch := Batch{Events: []trace.Event{
		event(now.Add(-time.Hour), "s1", "msg_ok"),
		event(time.Date(1478, 1, 1, 0, 0, 0, 0, time.UTC), "s1", "msg_ancient"),
		event(now.AddDate(50, 0, 0), "s1", "msg_future"),
		{Type: trace.EventResponse, SessionID: "s1", MessageID: "msg_no_ts"},
	}}

	res := postBatch(t, r, "secret", batch)
	if res.Accepted != 1 {
		t.Fatalf("accepted %d, want 1", res.Accepted)
	}
	if res.Rejected != 3 {
		t.Fatalf("rejected %d, want 3", res.Rejected)
	}

	storage, _ := r.registry.For("t_acme")
	days, err := storage.Store.Days(context.Background())
	if err != nil {
		t.Fatalf("days: %v", err)
	}
	if len(days) != 1 {
		t.Fatalf("the archive holds %d days; an implausible timestamp created a partition", len(days))
	}
}

// A compressed body that expands past the limit must be refused on what it
// expands to, not on what arrived. The compressed size of a bomb tells you
// nothing about the cost of reading it.
func TestCollectRefusesACompressionBomb(t *testing.T) {
	tok := auth.Token{ID: "tok_1", Kind: auth.KindCollector, TenantID: "t_acme", UserID: "u_d", MachineID: "m_l"}
	r := newRig(t, fixedAuth{"secret": tok})

	// Valid JSON throughout, so the decoder is forced to read every byte
	// rather than stopping early on a syntax error — the limit has to be what
	// refuses this, not the parser happening to give up first.
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write([]byte(`{"collector_version":"`)); err != nil {
		t.Fatalf("write: %v", err)
	}
	chunk := bytes.Repeat([]byte("a"), 1<<20)
	for written := 0; written < MaxBatchBytes*4; written += len(chunk) {
		if _, err := zw.Write(chunk); err != nil {
			t.Fatalf("write: %v", err)
		}
	}
	if _, err := zw.Write([]byte(`"}`)); err != nil {
		t.Fatalf("write: %v", err)
	}
	zw.Close()

	if buf.Len() >= MaxBatchBytes {
		t.Skipf("compressed payload is %d bytes; not a bomb on this platform", buf.Len())
	}

	req, _ := http.NewRequest(http.MethodPost, r.server.URL+"/api/collect", bytes.NewReader(buf.Bytes()))
	req.Header.Set("Authorization", "Bearer secret")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Content-Encoding", "gzip")

	resp, err := r.server.Client().Do(req)
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("a bomb decompressing to %d MiB returned %d, want 413",
			(MaxBatchBytes*4)>>20, resp.StatusCode)
	}
}

// A refused batch is halved and retried rather than abandoned. Giving up would
// strand a backlog permanently, and for an archive whose source is pruned after
// a month that means losing it.
func TestSendAllHalvesOnRefusal(t *testing.T) {
	var seen []int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reader, err := batchReader(w, r)
		if err != nil {
			http.Error(w, "bad", http.StatusBadRequest)
			return
		}
		defer reader.Close()
		var b Batch
		if err := json.NewDecoder(reader).Decode(&b); err != nil {
			http.Error(w, "bad", http.StatusBadRequest)
			return
		}
		seen = append(seen, len(b.Events))
		if len(b.Events) > 2 {
			http.Error(w, "batch too large", http.StatusRequestEntityTooLarge)
			return
		}
		writeJSON(w, http.StatusOK, Result{Accepted: len(b.Events)})
	}))
	defer srv.Close()

	shipper, err := NewShipper(srv.URL, "secret", srv.Client(), discardLogger())
	if err != nil {
		t.Fatalf("shipper: %v", err)
	}
	shipper.BatchSize = 8

	ts := time.Now().UTC().Add(-time.Hour)
	var events []trace.Event
	for i := 0; i < 8; i++ {
		events = append(events, event(ts, "s1", "msg_"+string(rune('a'+i))))
	}

	res, err := shipper.SendAll(context.Background(), events)
	if err != nil {
		t.Fatalf("send all: %v", err)
	}
	if res.Accepted != 8 {
		t.Fatalf("accepted %d, want 8", res.Accepted)
	}
	if len(seen) < 3 {
		t.Fatalf("expected the batch to be halved more than once, saw sizes %v", seen)
	}
}

// A revoked credential is the one failure retrying cannot fix, so the shipper
// has to name it rather than fold it into a generic error and loop forever.
func TestShipperReportsARejectedCredentialDistinctly(t *testing.T) {
	r := newRig(t, fixedAuth{})
	shipper, err := NewShipper(r.server.URL, "not-a-token", r.server.Client(), discardLogger())
	if err != nil {
		t.Fatalf("shipper: %v", err)
	}
	_, err = shipper.Send(context.Background(), []trace.Event{event(time.Now().UTC(), "s1", "msg_1")})
	if err != ErrCredentialRejected {
		t.Fatalf("want ErrCredentialRejected, got %v", err)
	}
}

func postBatch(t *testing.T, r *rig, secret string, b Batch) Result {
	t.Helper()
	body, err := json.Marshal(b)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	req, _ := http.NewRequest(http.MethodPost, r.server.URL+"/api/collect", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+secret)
	req.Header.Set("Content-Type", "application/json")

	resp, err := r.server.Client().Do(req)
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("post returned %d", resp.StatusCode)
	}
	var res Result
	if err := json.NewDecoder(resp.Body).Decode(&res); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return res
}
