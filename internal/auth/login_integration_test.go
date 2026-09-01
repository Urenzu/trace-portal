package auth

import (
	"bytes"
	"context"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Urenzu/trace-portal/internal/identity"
)

// The CLI and the server are written against a wire contract rather than
// against each other, so this is the test that proves the contract holds: the
// real identity.Login talks to the real auth.Server, and the enrollment file it
// leaves behind is the one the store reads to stamp turns.
//
// Without it, each half can pass its own tests while disagreeing about a field
// name, and the failure would surface only when someone runs the command.
func TestLoginEnrollsThroughTheRealServer(t *testing.T) {
	h := newHarness(t)

	// Poll fast, so the test does not wait out a five-second interval.
	h.device.Interval = 50 * time.Millisecond

	dataDir := t.TempDir()
	before, err := identity.Load(dataDir)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if !before.Local() {
		t.Fatal("a fresh data directory should be local")
	}

	// Approve from a browser as soon as the code appears. The CLI blocks, so
	// the approval has to happen alongside it — which is exactly the shape of
	// the real thing, where the person is at a browser while the terminal waits.
	approved := make(chan error, 1)
	codeSeen := make(chan string, 1)

	var out lockedBuffer
	go func() {
		code := <-codeSeen
		h.signIn(t)
		resp, err := h.client.PostForm(h.server.URL+"/auth/device",
			url.Values{"code": {code}, "decision": {"approve"}})
		if err != nil {
			approved <- err
			return
		}
		resp.Body.Close()
		approved <- nil
	}()

	// Watch the CLI's own output for the code it printed, rather than reaching
	// into the server for it. The code a person reads off the terminal is the
	// code that has to work.
	go func() {
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			if code := userCodeFrom(out.String()); code != "" {
				codeSeen <- code
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
		codeSeen <- ""
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	enrolled, err := identity.Login(ctx, identity.LoginOptions{
		Server:     h.server.URL,
		DataDir:    dataDir,
		Out:        &out,
		HTTPClient: h.client,
	})
	if err != nil {
		t.Fatalf("login: %v\nCLI output:\n%s", err, out.String())
	}
	if err := <-approved; err != nil {
		t.Fatalf("approval: %v", err)
	}

	if enrolled.Local() {
		t.Fatal("still local after a successful sign-in")
	}
	if enrolled.Token == "" {
		t.Fatal("no collector token stored")
	}
	// The machine id must survive sign-in. Minting a new one would make one
	// laptop look like a different machine after every login, and the
	// deduplication that keys on it would have nothing stable to work with.
	if enrolled.MachineID != before.MachineID {
		t.Fatalf("machine id changed on sign-in: %q then %q", before.MachineID, enrolled.MachineID)
	}

	// What was written is what a later process reads back, and it attributes.
	reloaded, err := identity.Load(dataDir)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if reloaded.TenantID != enrolled.TenantID || reloaded.UserID != enrolled.UserID {
		t.Fatalf("enrollment did not round-trip: %+v vs %+v", reloaded, enrolled)
	}
	if !reloaded.Identity().Attributed() {
		t.Fatal("an enrolled install must attribute its turns")
	}

	// The stored credential is a collector token and nothing more.
	tok, err := h.issuer.Verify(context.Background(), reloaded.Token, KindCollector)
	if err != nil {
		t.Fatalf("stored token is not a valid collector credential: %v", err)
	}
	if tok.MachineID != reloaded.MachineID {
		t.Fatalf("token bound to %q but stored under %q", tok.MachineID, reloaded.MachineID)
	}
	if _, err := h.issuer.Verify(context.Background(), reloaded.Token, KindSession); err == nil {
		t.Fatal("a token in a config file on a laptop was accepted as a browser session")
	}

	// The identity stamped onto events must never carry the credential.
	if id := reloaded.Identity(); strings.Contains(id.TenantID+id.UserID+id.MachineID, reloaded.Token) {
		t.Fatal("the collector token leaked into the identity stamped onto events")
	}
}

// Signing out returns to local capture without touching what was captured.
func TestLogoutReturnsToLocalCapture(t *testing.T) {
	dataDir := t.TempDir()
	before, err := identity.Load(dataDir)
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	if err := identity.Save(dataDir, identity.Enrollment{
		TenantID: "t_acme", UserID: "u_dana", MachineID: before.MachineID,
		Token: "secret", Server: "https://app.example.com",
	}); err != nil {
		t.Fatalf("save: %v", err)
	}

	after, err := identity.Logout(dataDir)
	if err != nil {
		t.Fatalf("logout: %v", err)
	}
	if !after.Local() {
		t.Fatalf("logout did not return to local capture: %+v", after)
	}
	if after.Token != "" || after.Server != "" {
		t.Fatalf("logout left a credential behind: %+v", after)
	}
	if after.MachineID != before.MachineID {
		t.Fatal("logout changed the machine id; signing out is not a new machine")
	}
	if !after.Identity().Attributed() {
		t.Fatal("a signed-out install must still attribute its turns, locally")
	}
}

// userCodeFrom finds the ABCD-EFGH code in whatever the CLI printed.
func userCodeFrom(s string) string {
	for _, field := range strings.Fields(s) {
		if len(field) != userCodeLength+1 || field[userCodeLength/2] != '-' {
			continue
		}
		ok := true
		for i, r := range field {
			if i == userCodeLength/2 {
				continue
			}
			if !strings.ContainsRune(userCodeAlphabet, r) {
				ok = false
				break
			}
		}
		if ok {
			return field
		}
	}
	return ""
}

// lockedBuffer is a bytes.Buffer safe for the reader goroutine that watches the
// CLI's output while the CLI is still writing to it.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}
