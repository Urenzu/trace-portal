package api

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Urenzu/trace-portal/internal/auth"
	"github.com/Urenzu/trace-portal/internal/tenant"
	"github.com/Urenzu/trace-portal/internal/trace"
)

// fakeSessions answers with whatever account the cookie names, so the test can
// switch identity without running an OIDC flow.
type fakeSessions map[string]auth.Account

func (f fakeSessions) Session(r *http.Request) (auth.Account, bool) {
	c, err := r.Cookie("who")
	if err != nil {
		return auth.Account{}, false
	}
	acct, ok := f[c.Value]
	return acct, ok
}

// TestReadsAreScopedToTheSession is the test the read API existed without.
//
// Ingest has resolved the tenant from a credential since it was written, but
// every query answered from the process's own archive, so a server holding two
// customers would have shown each of them the other's spend. This drives two
// tenants' turns into one registry and asserts that a request carrying one
// session cannot see the other's session ids or token totals.
func TestReadsAreScopedToTheSession(t *testing.T) {
	now := time.Now().UTC().Add(-time.Minute)
	registry, err := tenant.NewPartitioned(t.TempDir())
	if err != nil {
		t.Fatalf("registry: %v", err)
	}
	t.Cleanup(func() { registry.Close() })

	write := func(tenantID, session string, tokens int) {
		st, err := registry.For(tenantID)
		if err != nil {
			t.Fatalf("open %s: %v", tenantID, err)
		}
		evs := []trace.Event{
			{Type: trace.EventRequest, Timestamp: now, SessionID: session, TurnID: session + "-t1",
				Model: "claude-opus-5", MessageID: "msg_" + session},
			{Type: trace.EventResponse, Timestamp: now.Add(time.Second), SessionID: session, TurnID: session + "-t1",
				Model: "claude-opus-5", MessageID: "msg_" + session, StatusCode: 200,
				Usage: &trace.Usage{InputTokens: tokens, OutputTokens: 1}},
		}
		for _, ev := range evs {
			if err := st.Store.Append(context.Background(), ev); err != nil {
				t.Fatalf("append to %s: %v", tenantID, err)
			}
		}
	}
	write("acme", "acme_session", 1000)
	write("globex", "globex_session", 7000)

	sessions := fakeSessions{
		"alice": {TenantID: "acme", UserID: "u_alice"},
		"bob":   {TenantID: "globex", UserID: "u_bob"},
	}
	h := New(FromSession(sessions, registry), slog.New(slog.NewTextHandler(io.Discard, nil))).Handler()

	as := func(who, path string, into any) int {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, path, nil)
		if who != "" {
			req.AddCookie(&http.Cookie{Name: "who", Value: who})
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if into != nil && rec.Code == http.StatusOK {
			if err := json.Unmarshal(rec.Body.Bytes(), into); err != nil {
				t.Fatalf("decode %s: %v (%s)", path, err, rec.Body.String())
			}
		}
		return rec.Code
	}

	for _, tc := range []struct {
		who, session string
		tokens       int
	}{
		{"alice", "acme_session", 1000},
		{"bob", "globex_session", 7000},
	} {
		var page struct {
			Sessions []struct {
				ID string `json:"id"`
			} `json:"sessions"`
		}
		if code := as(tc.who, "/api/sessions", &page); code != http.StatusOK {
			t.Fatalf("%s sessions: status %d", tc.who, code)
		}
		if len(page.Sessions) != 1 || page.Sessions[0].ID != tc.session {
			t.Fatalf("%s saw %+v, want only %s", tc.who, page.Sessions, tc.session)
		}

		var stats Stats
		if code := as(tc.who, "/api/stats", &stats); code != http.StatusOK {
			t.Fatalf("%s stats: status %d", tc.who, code)
		}
		if stats.Usage.InputTokens != tc.tokens {
			t.Fatalf("%s saw %d input tokens, want %d — that is another tenant's spend",
				tc.who, stats.Usage.InputTokens, tc.tokens)
		}
	}

	// No session is a 401 rather than somebody's data, and health still answers
	// so a container probe and a signed-out UI both work.
	if code := as("", "/api/sessions", nil); code != http.StatusUnauthorized {
		t.Fatalf("anonymous sessions: status %d, want 401", code)
	}
	var health map[string]any
	if code := as("", "/api/health", &health); code != http.StatusOK {
		t.Fatalf("anonymous health: status %d, want 200", code)
	}
	if health["signed_in"] != false {
		t.Fatalf("anonymous health said signed_in=%v", health["signed_in"])
	}
	if _, leaked := health["days_captured"]; leaked {
		t.Fatal("anonymous health disclosed how much data the server holds")
	}
}
