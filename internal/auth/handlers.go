package auth

import (
	"encoding/json"
	"errors"
	"html/template"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"golang.org/x/oauth2"
)

// Server is the HTTP surface of both sign-in flows.
type Server struct {
	provider *Provider
	dir      Directory
	issuer   *Issuer
	device   *DeviceFlow
	log      *slog.Logger
	now      func() time.Time

	// Secure marks cookies Secure. It is on unless the deployment is plain
	// http on loopback, which is the only case where a Secure cookie would be
	// dropped and sign-in would silently fail.
	Secure bool

	// AppURL is where a completed browser sign-in lands.
	AppURL string
}

// ServerConfig collects what the HTTP surface needs.
type ServerConfig struct {
	Provider  *Provider
	Directory Directory
	Issuer    *Issuer
	Device    *DeviceFlow
	Logger    *slog.Logger
	Now       func() time.Time
	Secure    bool
	AppURL    string
}

// NewServer builds the sign-in endpoints.
func NewServer(cfg ServerConfig) (*Server, error) {
	if cfg.Provider == nil || cfg.Directory == nil || cfg.Issuer == nil {
		return nil, errors.New("auth server needs a provider, a directory and an issuer")
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.AppURL == "" {
		cfg.AppURL = "/"
	}
	return &Server{
		provider: cfg.Provider,
		dir:      cfg.Directory,
		issuer:   cfg.Issuer,
		device:   cfg.Device,
		log:      cfg.Logger,
		now:      cfg.Now,
		Secure:   cfg.Secure,
		AppURL:   cfg.AppURL,
	}, nil
}

// Cookie names. The session cookie is host-only and SameSite=Lax so it is not
// sent on cross-site requests; the transient one carries the state, nonce and
// PKCE verifier between the redirect out and the callback back.
const (
	sessionCookie = "tp_session"
	pendingCookie = "tp_auth"
)

// Routes registers every sign-in endpoint on mux.
func (s *Server) Routes(mux *http.ServeMux) {
	// Browser flow.
	mux.HandleFunc("GET /auth/login", s.handleLogin)
	mux.HandleFunc("GET /auth/callback", s.handleCallback)
	mux.HandleFunc("POST /auth/logout", s.handleLogout)
	mux.HandleFunc("GET /auth/me", s.handleMe)

	if s.device == nil {
		return
	}
	// CLI flow. The code and token endpoints are the CLI's; the device page and
	// its approval are the browser's, and both of those require a session.
	mux.HandleFunc("POST /auth/device/code", s.handleDeviceCode)
	mux.HandleFunc("POST /auth/device/token", s.handleDeviceToken)
	mux.HandleFunc("GET /auth/device", s.handleDevicePage)
	mux.HandleFunc("POST /auth/device", s.handleDeviceDecision)
}

// pending is what survives the round trip to the provider. It rides in a
// short-lived cookie rather than server memory so that a deployment behind more
// than one process does not need sticky sessions to complete a sign-in.
type pending struct {
	State    string `json:"state"`
	Nonce    string `json:"nonce"`
	Verifier string `json:"verifier"`
	// Return is where to land afterwards. Only ever a path on this site; see
	// safeReturn for why that is enforced rather than assumed.
	Return string `json:"return"`
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	p := pending{
		State:    newSecret(),
		Nonce:    newSecret(),
		Verifier: oauth2.GenerateVerifier(),
		Return:   safeReturn(r.URL.Query().Get("return")),
	}
	raw, err := json.Marshal(p)
	if err != nil {
		s.fail(w, http.StatusInternalServerError, "could not start sign-in")
		return
	}

	http.SetCookie(w, &http.Cookie{
		Name:     pendingCookie,
		Value:    encodeCookie(raw),
		Path:     "/auth",
		HttpOnly: true,
		Secure:   s.Secure,
		SameSite: http.SameSiteLaxMode,
		// The provider round trip is interactive, so this only has to outlive a
		// person reading a consent screen.
		MaxAge: int((10 * time.Minute).Seconds()),
	})
	http.Redirect(w, r, s.provider.AuthCodeURL(p.State, p.Nonce, p.Verifier), http.StatusFound)
}

func (s *Server) handleCallback(w http.ResponseWriter, r *http.Request) {
	// A provider that refuses says so in the query string. Surface it rather
	// than failing as though the exchange broke, since "you are not a member of
	// this workspace" is a different problem from "sign-in is down".
	if e := r.URL.Query().Get("error"); e != "" {
		s.log.Warn("provider refused sign-in", "error", e, "description", r.URL.Query().Get("error_description"))
		s.fail(w, http.StatusForbidden, "the identity provider refused this sign-in")
		return
	}

	c, err := r.Cookie(pendingCookie)
	if err != nil {
		s.fail(w, http.StatusBadRequest, "sign-in expired; start again")
		return
	}
	raw, err := decodeCookie(c.Value)
	if err != nil {
		s.fail(w, http.StatusBadRequest, "sign-in expired; start again")
		return
	}
	var p pending
	if err := json.Unmarshal(raw, &p); err != nil {
		s.fail(w, http.StatusBadRequest, "sign-in expired; start again")
		return
	}
	// Clear it immediately: this is single-use, and leaving it set turns a
	// back-button press into a confusing second exchange.
	s.clearCookie(w, pendingCookie, "/auth")

	if p.State == "" || !constantTimeEqual(p.State, r.URL.Query().Get("state")) {
		s.fail(w, http.StatusBadRequest, "sign-in state did not match; start again")
		return
	}
	code := r.URL.Query().Get("code")
	if code == "" {
		s.fail(w, http.StatusBadRequest, "no authorization code returned")
		return
	}

	claims, err := s.provider.Exchange(r.Context(), code, p.Verifier, p.Nonce)
	if err != nil {
		s.log.Warn("sign-in exchange failed", "err", err)
		s.fail(w, http.StatusForbidden, "could not verify this sign-in")
		return
	}

	acct, err := s.dir.Resolve(r.Context(), claims)
	if err != nil {
		s.log.Error("directory refused a verified sign-in", "err", err)
		s.fail(w, http.StatusInternalServerError, "could not resolve this account")
		return
	}

	_, secret, err := s.issuer.IssueSession(r.Context(), acct)
	if err != nil {
		s.fail(w, http.StatusInternalServerError, "could not start a session")
		return
	}
	s.setSession(w, secret)

	dest := p.Return
	if dest == "" {
		dest = s.AppURL
	}
	http.Redirect(w, r, dest, http.StatusFound)
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(sessionCookie); err == nil {
		if tok, err := s.issuer.Verify(r.Context(), c.Value, KindSession); err == nil {
			// Revoke rather than only clearing the cookie: a cookie copied off
			// a machine would otherwise stay valid after the person believed
			// they had signed out.
			_ = s.issuer.Revoke(r.Context(), tok.ID)
		}
	}
	s.clearCookie(w, sessionCookie, "/")
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleMe(w http.ResponseWriter, r *http.Request) {
	acct, ok := s.Session(r)
	if !ok {
		writeJSON(w, http.StatusOK, map[string]any{"signed_in": false})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"signed_in": true,
		"tenant_id": acct.TenantID,
		"user_id":   acct.UserID,
		"email":     acct.Email,
		"name":      acct.Name,
	})
}

// Session resolves the caller's browser session, if any.
func (s *Server) Session(r *http.Request) (Account, bool) {
	c, err := r.Cookie(sessionCookie)
	if err != nil {
		return Account{}, false
	}
	tok, err := s.issuer.Verify(r.Context(), c.Value, KindSession)
	if err != nil {
		return Account{}, false
	}
	acct, ok, err := s.dir.Lookup(r.Context(), tok.UserID)
	if err != nil || !ok {
		return Account{}, false
	}
	return acct, true
}

// Collector resolves a machine credential from the Authorization header. It is
// what the ingest endpoint will call.
func (s *Server) Collector(r *http.Request) (Token, error) {
	secret := BearerToken(r.Header.Get("Authorization"))
	if secret == "" {
		return Token{}, ErrTokenRejected
	}
	return s.issuer.Verify(r.Context(), secret, KindCollector)
}

// --- CLI flow ---------------------------------------------------------------

func (s *Server) handleDeviceCode(w http.ResponseWriter, r *http.Request) {
	var body struct {
		MachineID    string `json:"machine_id"`
		MachineLabel string `json:"machine_label"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&body); err != nil {
		writeOAuthError(w, http.StatusBadRequest, "invalid_request", "could not read the request")
		return
	}
	req, err := s.device.Start(r.Context(), body.MachineID, body.MachineLabel)
	if err != nil {
		writeOAuthError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}

	verificationURI := s.device.VerificationURI
	writeJSON(w, http.StatusOK, map[string]any{
		"device_code":      req.DeviceCode,
		"user_code":        req.UserCode,
		"verification_uri": verificationURI,
		// The complete form prefills the code. Offered alongside the plain one
		// rather than instead of it, because the person may be reading the
		// terminal on a machine with no browser and typing the code on a phone.
		"verification_uri_complete": verificationURI + "?code=" + url.QueryEscape(req.UserCode),
		"expires_in":                int(time.Until(req.ExpiresAt).Seconds()),
		"interval":                  int(req.Interval.Seconds()),
	})
}

func (s *Server) handleDeviceToken(w http.ResponseWriter, r *http.Request) {
	var body struct {
		DeviceCode string `json:"device_code"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&body); err != nil {
		writeOAuthError(w, http.StatusBadRequest, "invalid_request", "could not read the request")
		return
	}

	tok, secret, err := s.device.Poll(r.Context(), body.DeviceCode)
	if err != nil {
		// RFC 8628 pending and slow_down are not failures; they are the normal
		// shape of a poll, and a CLI branches on the code rather than the text.
		status := http.StatusBadRequest
		if errors.Is(err, ErrAuthorizationPending) || errors.Is(err, ErrSlowDown) {
			status = http.StatusAccepted
		}
		writeOAuthError(w, status, err.Error(), "")
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"access_token": secret,
		"token_type":   "Bearer",
		"tenant_id":    tok.TenantID,
		"user_id":      tok.UserID,
		"machine_id":   tok.MachineID,
	})
}

// devicePage is intentionally plain server-rendered HTML rather than part of the
// React bundle. It has to work in a browser the person may have just opened on
// a phone, and it must show what is being approved before anything is approved.
var devicePage = template.Must(template.New("device").Parse(`<!doctype html>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<title>Connect a machine</title>
<style>
 body{font:16px/1.5 system-ui,sans-serif;max-width:34rem;margin:4rem auto;padding:0 1rem}
 code{font-size:1.4rem;letter-spacing:.12em}
 .row{display:flex;gap:.75rem;margin-top:1.5rem}
 button{font:inherit;padding:.5rem 1rem;cursor:pointer}
 .muted{color:#666}
</style>
<h1>Connect a machine</h1>
{{if .Error}}<p role="alert">{{.Error}}</p>{{end}}
{{if .Request}}
<p>A collector on <strong>{{.Label}}</strong> is asking to send agent traces to your account.</p>
<p class="muted">Code <code>{{.Request.UserCode}}</code> — approve this only if you just ran
<code>trace-portal login</code> on that machine.</p>
<form method="post" class="row">
  <input type="hidden" name="code" value="{{.Request.UserCode}}">
  <button name="decision" value="approve">Approve</button>
  <button name="decision" value="deny">Deny</button>
</form>
{{else}}
<form method="post">
  <label>Enter the code shown in your terminal<br><input name="code" autofocus autocomplete="off"></label>
  <div class="row"><button name="decision" value="lookup">Continue</button></div>
</form>
{{end}}
`))

func (s *Server) handleDevicePage(w http.ResponseWriter, r *http.Request) {
	acct, ok := s.Session(r)
	if !ok {
		// Send them through the browser flow and back here. This is the join
		// between the two flows: the CLI's approval is authorised by an
		// ordinary OIDC sign-in, so the CLI itself never needs one.
		s.redirectToLogin(w, r)
		return
	}
	_ = acct

	data := struct {
		Request *DeviceRequest
		Label   string
		Error   string
	}{}

	if code := r.URL.Query().Get("code"); code != "" {
		req, err := s.device.Lookup(r.Context(), code)
		if err != nil {
			data.Error = deviceLookupMessage(err)
		} else {
			data.Request, data.Label = &req, displayLabel(req)
		}
	}
	renderDevice(w, data.Request, data.Label, data.Error)
}

func (s *Server) handleDeviceDecision(w http.ResponseWriter, r *http.Request) {
	acct, ok := s.Session(r)
	if !ok {
		s.redirectToLogin(w, r)
		return
	}
	if err := r.ParseForm(); err != nil {
		renderDevice(w, nil, "", "Could not read that form.")
		return
	}
	code := r.PostFormValue("code")

	switch r.PostFormValue("decision") {
	case "approve":
		if err := s.device.Approve(r.Context(), code, acct); err != nil {
			renderDevice(w, nil, "", deviceLookupMessage(err))
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(`<!doctype html><meta charset="utf-8"><title>Connected</title>
<p style="font:16px system-ui;max-width:34rem;margin:4rem auto">Machine connected. You can close this tab and return to your terminal.</p>`))
	case "deny":
		_ = s.device.Deny(r.Context(), code)
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(`<!doctype html><meta charset="utf-8"><title>Denied</title>
<p style="font:16px system-ui;max-width:34rem;margin:4rem auto">Denied. Nothing was connected.</p>`))
	default:
		req, err := s.device.Lookup(r.Context(), code)
		if err != nil {
			renderDevice(w, nil, "", deviceLookupMessage(err))
			return
		}
		renderDevice(w, &req, displayLabel(req), "")
	}
}

func renderDevice(w http.ResponseWriter, req *DeviceRequest, label, errMsg string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = devicePage.Execute(w, struct {
		Request *DeviceRequest
		Label   string
		Error   string
	}{Request: req, Label: label, Error: errMsg})
}

// displayLabel picks what to show for the machine being enrolled. The label is
// supplied by whatever ran the CLI, so it is untrusted input — html/template
// escapes it, and it is never matched against anything.
func displayLabel(r DeviceRequest) string {
	if strings.TrimSpace(r.MachineLabel) != "" {
		return r.MachineLabel
	}
	return "an unnamed machine"
}

func deviceLookupMessage(err error) string {
	switch {
	case errors.Is(err, ErrExpiredToken):
		return "That code has expired. Run trace-portal login again."
	default:
		return "That code was not recognised. Check it and try again."
	}
}

func (s *Server) redirectToLogin(w http.ResponseWriter, r *http.Request) {
	http.Redirect(w, r, "/auth/login?return="+url.QueryEscape(safeReturn(r.URL.RequestURI())), http.StatusFound)
}

// --- helpers ----------------------------------------------------------------

func (s *Server) setSession(w http.ResponseWriter, secret string) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    secret,
		Path:     "/",
		HttpOnly: true,
		Secure:   s.Secure,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   int(s.issuer.SessionTTL.Seconds()),
	})
}

func (s *Server) clearCookie(w http.ResponseWriter, name, path string) {
	http.SetCookie(w, &http.Cookie{
		Name: name, Value: "", Path: path,
		HttpOnly: true, Secure: s.Secure, SameSite: http.SameSiteLaxMode, MaxAge: -1,
	})
}

// safeReturn keeps a post-sign-in redirect on this site.
//
// An open redirect on a login endpoint is how a phishing link acquires a
// legitimate-looking domain: the victim really does sign in to the real site,
// and is then bounced to an attacker's page that asks them to do it again. Only
// a path is accepted — never a scheme, a host, or a protocol-relative //path
// that a browser reads as a host.
func safeReturn(raw string) string {
	if raw == "" {
		return ""
	}
	if strings.HasPrefix(raw, "//") || strings.Contains(raw, ":") {
		return ""
	}
	if !strings.HasPrefix(raw, "/") {
		return ""
	}
	return raw
}

func encodeCookie(raw []byte) string { return base64URL.EncodeToString(raw) }

func decodeCookie(s string) ([]byte, error) { return base64URL.DecodeString(s) }

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// writeOAuthError uses the error shape RFC 6749 and RFC 8628 define, because
// the CLI polling this endpoint branches on the code.
func writeOAuthError(w http.ResponseWriter, status int, code, description string) {
	body := map[string]any{"error": code}
	if description != "" {
		body["error_description"] = description
	}
	writeJSON(w, status, body)
}

func (s *Server) fail(w http.ResponseWriter, status int, msg string) {
	http.Error(w, msg, status)
}
