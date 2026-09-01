package auth

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func mustParse(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse %q: %v", raw, err)
	}
	return u
}

// harness wires a fake provider to a real auth server, with an http client that
// keeps cookies and does not follow redirects — so a test can inspect each hop
// the way a browser would traverse it.
type harness struct {
	provider *fakeProvider
	auth     *Server
	dir      *MemoryDirectory
	issuer   *Issuer
	device   *DeviceFlow
	server   *httptest.Server
	client   *http.Client
	now      func() time.Time
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	fp := newFakeProvider(t)

	mux := http.NewServeMux()
	site := httptest.NewServer(mux)
	t.Cleanup(site.Close)

	provider, err := NewProvider(context.Background(), Config{
		IssuerURL:   fp.server.URL,
		ClientID:    "test-client",
		RedirectURL: site.URL + "/auth/callback",
		HTTPClient:  fp.server.Client(),
	})
	if err != nil {
		t.Fatalf("new provider: %v", err)
	}

	// The clock is read through the harness so advance() moves it for every
	// component at once; capturing a local would leave each holding its own.
	h := &harness{provider: fp, server: site}
	h.now = time.Now
	clock := func() time.Time { return h.now() }

	h.dir = NewMemoryDirectory(TenantPerUser{})
	h.issuer = NewIssuer(NewMemoryTokens(), clock)
	h.device = NewDeviceFlow(NewMemoryDevices(), h.issuer, site.URL+"/auth/device", clock)

	as, err := NewServer(ServerConfig{
		Provider: provider, Directory: h.dir, Issuer: h.issuer, Device: h.device,
		AppURL: "/", Now: clock,
	})
	if err != nil {
		t.Fatalf("new auth server: %v", err)
	}
	as.Routes(mux)
	h.auth = as

	jar, _ := cookiejar.New(nil)
	h.client = &http.Client{
		Jar:           jar,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	return h
}

// signIn runs the whole browser flow and leaves the session cookie in the jar.
func (h *harness) signIn(t *testing.T) {
	t.Helper()
	resp, err := h.client.Get(h.server.URL + "/auth/login")
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("login should redirect, got %d", resp.StatusCode)
	}

	code, state := h.provider.authorize(t, resp.Header.Get("Location"))
	cb := h.server.URL + "/auth/callback?code=" + url.QueryEscape(code) + "&state=" + url.QueryEscape(state)
	resp, err = h.client.Get(cb)
	if err != nil {
		t.Fatalf("callback: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("callback should redirect on success, got %d", resp.StatusCode)
	}
}

// The browser flow, end to end against a real signed id_token.
func TestBrowserFlowSignsIn(t *testing.T) {
	h := newHarness(t)
	h.signIn(t)

	resp, err := h.client.Get(h.server.URL + "/auth/me")
	if err != nil {
		t.Fatalf("me: %v", err)
	}
	defer resp.Body.Close()

	var me struct {
		SignedIn bool   `json:"signed_in"`
		TenantID string `json:"tenant_id"`
		UserID   string `json:"user_id"`
		Email    string `json:"email"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&me); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !me.SignedIn {
		t.Fatal("not signed in after a successful callback")
	}
	if me.Email != "dana@example.com" {
		t.Fatalf("wrong email: %q", me.Email)
	}
	if !strings.HasPrefix(me.UserID, "u_") || !strings.HasPrefix(me.TenantID, "t_") {
		t.Fatalf("ids should be ours, not the provider's: %+v", me)
	}
	if me.UserID == h.provider.subject {
		t.Fatal("the provider's subject leaked into our user id")
	}
	if h.provider.lastVerifier == "" {
		t.Fatal("PKCE verifier was never presented at the token endpoint")
	}
}

// A callback whose state does not match the cookie is a forged callback. This
// is the check that stops an attacker completing a sign-in into a victim's
// browser using a code they obtained themselves.
func TestCallbackRejectsMismatchedState(t *testing.T) {
	h := newHarness(t)

	resp, err := h.client.Get(h.server.URL + "/auth/login")
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	resp.Body.Close()
	code, _ := h.provider.authorize(t, resp.Header.Get("Location"))

	resp, err = h.client.Get(h.server.URL + "/auth/callback?code=" + code + "&state=not-the-one")
	if err != nil {
		t.Fatalf("callback: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("a mismatched state should be refused, got %d", resp.StatusCode)
	}
}

// An id_token carrying somebody else's nonce is a replay. It verifies
// perfectly — right signature, right audience — so the nonce check is the only
// thing standing between a captured token and a session.
func TestCallbackRejectsReplayedNonce(t *testing.T) {
	h := newHarness(t)

	resp, err := h.client.Get(h.server.URL + "/auth/login")
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	resp.Body.Close()
	code, state := h.provider.authorize(t, resp.Header.Get("Location"))

	h.provider.forceNonce = "a-nonce-from-another-sign-in"

	resp, err = h.client.Get(h.server.URL + "/auth/callback?code=" + code + "&state=" + url.QueryEscape(state))
	if err != nil {
		t.Fatalf("callback: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("a replayed nonce should be refused, got %d", resp.StatusCode)
	}
}

// An open redirect on a sign-in endpoint hands an attacker a phishing page on
// the real domain, reached through a genuine sign-in.
func TestLoginRefusesOffSiteReturns(t *testing.T) {
	for _, bad := range []string{
		"https://evil.example.com/",
		"//evil.example.com/",
		"javascript:alert(1)",
	} {
		if got := safeReturn(bad); got != "" {
			t.Errorf("safeReturn(%q) = %q, want empty", bad, got)
		}
	}
	if got := safeReturn("/auth/device?code=ABCD-EFGH"); got != "/auth/device?code=ABCD-EFGH" {
		t.Errorf("a same-site path should survive, got %q", got)
	}
}

// The whole point of the CLI flow: a terminal ends up holding a collector token
// without ever seeing a password or talking to the identity provider.
func TestDeviceFlowEnrollsACollector(t *testing.T) {
	h := newHarness(t)

	// 1. The CLI asks for a code pair. It is unauthenticated here.
	start := h.postJSON(t, "/auth/device/code", map[string]any{
		"machine_id": "m_laptop", "machine_label": "dana-laptop",
	}, nil)
	var pair struct {
		DeviceCode      string `json:"device_code"`
		UserCode        string `json:"user_code"`
		VerificationURI string `json:"verification_uri"`
		Interval        int    `json:"interval"`
	}
	decode(t, start, &pair)
	if pair.DeviceCode == "" || pair.UserCode == "" {
		t.Fatalf("incomplete code pair: %+v", pair)
	}

	// 2. Polling before approval is pending, not an error.
	poll := h.postJSON(t, "/auth/device/token", map[string]any{"device_code": pair.DeviceCode}, nil)
	var pollBody struct {
		Error       string `json:"error"`
		AccessToken string `json:"access_token"`
	}
	decode(t, poll, &pollBody)
	if pollBody.Error != "authorization_pending" {
		t.Fatalf("first poll should be pending, got %+v", pollBody)
	}

	// 3. The person signs in through the browser flow and approves.
	h.signIn(t)
	form := url.Values{"code": {pair.UserCode}, "decision": {"approve"}}
	resp, err := h.client.PostForm(h.server.URL+"/auth/device", form)
	if err != nil {
		t.Fatalf("approve: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("approval failed with %d", resp.StatusCode)
	}

	// 4. The next poll returns the collector token. The interval is honoured by
	//    moving the clock rather than sleeping.
	h.advance(time.Duration(pair.Interval+1) * time.Second)
	poll = h.postJSON(t, "/auth/device/token", map[string]any{"device_code": pair.DeviceCode}, nil)
	var issued struct {
		AccessToken string `json:"access_token"`
		TenantID    string `json:"tenant_id"`
		UserID      string `json:"user_id"`
		MachineID   string `json:"machine_id"`
	}
	decode(t, poll, &issued)
	if issued.AccessToken == "" {
		t.Fatalf("no token issued after approval: %+v", issued)
	}
	if issued.MachineID != "m_laptop" {
		t.Fatalf("token not bound to the machine that asked: %q", issued.MachineID)
	}

	// The identity the CLI receives must be the approver's, and it must match
	// what the UI reports for that same person.
	meResp, err := h.client.Get(h.server.URL + "/auth/me")
	if err != nil {
		t.Fatalf("me: %v", err)
	}
	var me struct {
		TenantID string `json:"tenant_id"`
		UserID   string `json:"user_id"`
	}
	decode(t, meResp, &me)
	if issued.UserID != me.UserID || issued.TenantID != me.TenantID {
		t.Fatalf("collector enrolled into the wrong account: token %+v, session %+v", issued, me)
	}

	// 5. The token works as a collector credential and not as a session.
	tok, err := h.issuer.Verify(context.Background(), issued.AccessToken, KindCollector)
	if err != nil {
		t.Fatalf("collector token rejected: %v", err)
	}
	if tok.UserID != me.UserID {
		t.Fatalf("token resolves to the wrong user")
	}
	if _, err := h.issuer.Verify(context.Background(), issued.AccessToken, KindSession); err == nil {
		t.Fatal("a collector token was accepted as a browser session")
	}
}

// One approval must mint exactly one token. Without this, a replayed poll — a
// retry after a dropped connection, or a deliberate one — quietly issues a
// second credential that nobody knows exists and nobody will revoke.
func TestDeviceCodeCannotBeRedeemedTwice(t *testing.T) {
	h := newHarness(t)

	start := h.postJSON(t, "/auth/device/code", map[string]any{"machine_id": "m_laptop"}, nil)
	var pair struct {
		DeviceCode string `json:"device_code"`
		UserCode   string `json:"user_code"`
		Interval   int    `json:"interval"`
	}
	decode(t, start, &pair)

	h.signIn(t)
	resp, err := h.client.PostForm(h.server.URL+"/auth/device",
		url.Values{"code": {pair.UserCode}, "decision": {"approve"}})
	if err != nil {
		t.Fatalf("approve: %v", err)
	}
	resp.Body.Close()

	first := h.postJSON(t, "/auth/device/token", map[string]any{"device_code": pair.DeviceCode}, nil)
	var issued struct {
		AccessToken string `json:"access_token"`
	}
	decode(t, first, &issued)
	if issued.AccessToken == "" {
		t.Fatal("first redemption returned no token")
	}

	h.advance(time.Duration(pair.Interval+1) * time.Second)
	second := h.postJSON(t, "/auth/device/token", map[string]any{"device_code": pair.DeviceCode}, nil)
	var again struct {
		Error       string `json:"error"`
		AccessToken string `json:"access_token"`
	}
	decode(t, second, &again)
	if again.AccessToken != "" {
		t.Fatal("a redeemed device code minted a second token")
	}
	if again.Error != "invalid_grant" {
		t.Fatalf("want invalid_grant on a reused code, got %q", again.Error)
	}
}

// A CLI that ignores the interval is told to back off. RFC 8628 requires this,
// and without it an unattended process polls the token endpoint in a hot loop
// for the whole ten-minute window.
func TestDeviceFlowRateLimitsPolling(t *testing.T) {
	h := newHarness(t)
	start := h.postJSON(t, "/auth/device/code", map[string]any{"machine_id": "m_laptop"}, nil)
	var pair struct {
		DeviceCode string `json:"device_code"`
	}
	decode(t, start, &pair)

	first := h.postJSON(t, "/auth/device/token", map[string]any{"device_code": pair.DeviceCode}, nil)
	var a struct {
		Error string `json:"error"`
	}
	decode(t, first, &a)
	if a.Error != "authorization_pending" {
		t.Fatalf("first poll: want authorization_pending, got %q", a.Error)
	}

	second := h.postJSON(t, "/auth/device/token", map[string]any{"device_code": pair.DeviceCode}, nil)
	var b struct {
		Error string `json:"error"`
	}
	decode(t, second, &b)
	if b.Error != "slow_down" {
		t.Fatalf("an immediate second poll should be told to slow down, got %q", b.Error)
	}
}

// Approval requires a signed-in browser. This is what makes a short, retypable
// user code safe: guessing one reveals that a request exists and nothing more,
// because approving it still requires being somebody.
func TestDeviceApprovalRequiresASession(t *testing.T) {
	h := newHarness(t)
	start := h.postJSON(t, "/auth/device/code", map[string]any{"machine_id": "m_laptop"}, nil)
	var pair struct {
		UserCode string `json:"user_code"`
	}
	decode(t, start, &pair)

	resp, err := h.client.PostForm(h.server.URL+"/auth/device",
		url.Values{"code": {pair.UserCode}, "decision": {"approve"}})
	if err != nil {
		t.Fatalf("approve: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("an anonymous approval should be redirected to sign in, got %d", resp.StatusCode)
	}
	if loc := resp.Header.Get("Location"); !strings.HasPrefix(loc, "/auth/login") {
		t.Fatalf("should redirect to sign-in, got %q", loc)
	}
}

// People retype this code off a terminal. Rejecting a correct code because it
// was typed in lower case, or without the dash, is rejecting it for a
// formatting reason on the one screen where the person is already doing
// something tedious by hand.
func TestUserCodeNormalization(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"ABCD-EFGH", "ABCD-EFGH"},
		{"abcd-efgh", "ABCD-EFGH"},
		{"abcdefgh", "ABCD-EFGH"},
		{"  abcd efgh  ", "ABCD-EFGH"},
	} {
		if got := NormalizeUserCode(tc.in); got != tc.want {
			t.Errorf("NormalizeUserCode(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// The user code alphabet must not contain a pair a person can misread between
// a terminal and a browser.
func TestUserCodeAlphabetHasNoAmbiguousGlyphs(t *testing.T) {
	for _, bad := range []rune{'O', '0', 'I', '1', 'L', 'U', 'V'} {
		if strings.ContainsRune(userCodeAlphabet, bad) {
			t.Errorf("alphabet contains the ambiguous glyph %q", bad)
		}
	}
}

// Two people are two accounts, even when a provider reuses an email address —
// which happens whenever a departing employee's address is handed on.
func TestDirectoryKeysOnIssuerAndSubjectNotEmail(t *testing.T) {
	dir := NewMemoryDirectory(TenantPerUser{})
	ctx := context.Background()

	first, err := dir.Resolve(ctx, Claims{Issuer: "https://idp", Subject: "sub-1", Email: "shared@example.com", EmailVerified: true})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	second, err := dir.Resolve(ctx, Claims{Issuer: "https://idp", Subject: "sub-2", Email: "shared@example.com", EmailVerified: true})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if first.UserID == second.UserID {
		t.Fatal("two subjects sharing an email were merged into one user")
	}

	// The same subject signing in again is the same person, even if their
	// address has changed since.
	again, err := dir.Resolve(ctx, Claims{Issuer: "https://idp", Subject: "sub-1", Email: "renamed@example.com", EmailVerified: true})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if again.UserID != first.UserID {
		t.Fatal("a changed email produced a new user")
	}
	if again.Email != "renamed@example.com" {
		t.Fatalf("display email was not refreshed: %q", again.Email)
	}
}

// The same subject from a different issuer is a different person. Subjects are
// only unique within an issuer, so a deployment that trusts two providers would
// otherwise let one hand out another's account.
func TestDirectorySeparatesIssuers(t *testing.T) {
	dir := NewMemoryDirectory(TenantPerUser{})
	ctx := context.Background()

	a, _ := dir.Resolve(ctx, Claims{Issuer: "https://idp-a", Subject: "sub-1", Email: "a@example.com"})
	b, _ := dir.Resolve(ctx, Claims{Issuer: "https://idp-b", Subject: "sub-1", Email: "b@example.com"})
	if a.UserID == b.UserID {
		t.Fatal("the same subject from two issuers resolved to one user")
	}
}

// Domain grouping must honour only a verified address, or anyone can join a
// company's tenant by typing its domain into a sign-up form.
func TestTenantPerEmailDomainIgnoresUnverifiedAddresses(t *testing.T) {
	p := &TenantPerEmailDomain{}
	ctx := context.Background()

	real1, _ := p.TenantFor(ctx, Claims{Email: "a@acme.com", EmailVerified: true})
	real2, _ := p.TenantFor(ctx, Claims{Email: "b@acme.com", EmailVerified: true})
	if real1 != real2 {
		t.Fatal("two verified addresses on one domain should share a tenant")
	}

	impostor, _ := p.TenantFor(ctx, Claims{Email: "c@acme.com", EmailVerified: false})
	if impostor == real1 {
		t.Fatal("an unverified address joined a domain tenant")
	}

	// Consumer domains are not organisations.
	g1, _ := p.TenantFor(ctx, Claims{Email: "someone@gmail.com", EmailVerified: true})
	g2, _ := p.TenantFor(ctx, Claims{Email: "another@gmail.com", EmailVerified: true})
	if g1 == g2 {
		t.Fatal("two unrelated gmail users were merged into one tenant")
	}
}

// Signing out must revoke, not merely clear the cookie: a cookie copied off a
// machine would otherwise stay valid after the person believed they had ended
// the session.
func TestLogoutRevokesTheSession(t *testing.T) {
	h := newHarness(t)
	h.signIn(t)

	u := mustParse(t, h.server.URL)
	var secret string
	for _, c := range h.client.Jar.Cookies(u) {
		if c.Name == sessionCookie {
			secret = c.Value
		}
	}
	if secret == "" {
		t.Fatal("no session cookie after sign-in")
	}

	resp, err := h.client.Post(h.server.URL+"/auth/logout", "application/json", nil)
	if err != nil {
		t.Fatalf("logout: %v", err)
	}
	resp.Body.Close()

	if _, err := h.issuer.Verify(context.Background(), secret, KindSession); err == nil {
		t.Fatal("the session secret still verifies after sign-out")
	}
}

// An expired session stops working without anything having to sweep it.
func TestSessionExpires(t *testing.T) {
	h := newHarness(t)
	h.signIn(t)

	h.advance(h.issuer.SessionTTL + time.Minute)

	resp, err := h.client.Get(h.server.URL + "/auth/me")
	if err != nil {
		t.Fatalf("me: %v", err)
	}
	defer resp.Body.Close()
	var me struct {
		SignedIn bool `json:"signed_in"`
	}
	decode(t, resp, &me)
	if me.SignedIn {
		t.Fatal("an expired session still reports as signed in")
	}
}

// --- harness helpers --------------------------------------------------------

func (h *harness) advance(d time.Duration) {
	base := h.now()
	h.now = func() time.Time { return base.Add(d) }
}

func (h *harness) postJSON(t *testing.T, path string, body any, _ http.Header) *http.Response {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	resp, err := h.client.Post(h.server.URL+path, "application/json", strings.NewReader(string(raw)))
	if err != nil {
		t.Fatalf("post %s: %v", path, err)
	}
	return resp
}

func decode(t *testing.T, resp *http.Response, v any) {
	t.Helper()
	defer resp.Body.Close()
	if err := json.NewDecoder(resp.Body).Decode(v); err != nil {
		t.Fatalf("decode response: %v", err)
	}
}
