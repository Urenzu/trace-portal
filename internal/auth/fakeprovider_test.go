package auth

import (
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"
)

// fakeProvider is a minimal OpenID Connect provider: discovery, JWKS, and a
// token endpoint that mints signed id_tokens.
//
// Standing one up is worth the code. Everything interesting about this package
// is what it does with a real signed token — nonce binding, PKCE, the audience
// check — and a hand-rolled stub that returns claims directly would test none
// of it. It also keeps the tests honest about provider-agnosticism: nothing
// here is vendor-shaped, so a test that passes against it is a test that only
// used the standard.
type fakeProvider struct {
	server *httptest.Server
	key    *rsa.PrivateKey
	keyID  string

	// issued lets a test control what the next exchange returns.
	subject       string
	email         string
	emailVerified bool
	name          string
	audience      string

	// lastVerifier records the PKCE code_verifier the client presented, so a
	// test can assert the challenge was actually exercised.
	lastVerifier string
	// nonce is echoed into the id_token; a test overrides it to forge a replay.
	forceNonce   string
	pendingNonce string
}

func newFakeProvider(t *testing.T) *fakeProvider {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	p := &fakeProvider{
		key:           key,
		keyID:         "test-key-1",
		subject:       "sub-dana",
		email:         "dana@example.com",
		emailVerified: true,
		name:          "Dana",
		// The client authenticates with HTTP Basic rather than a form field, so
		// the audience is configured here rather than read off the token
		// request. A test overrides it to forge a token for another client.
		audience: "test-client",
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{
			"issuer":                                p.server.URL,
			"authorization_endpoint":                p.server.URL + "/authorize",
			"token_endpoint":                        p.server.URL + "/token",
			"jwks_uri":                              p.server.URL + "/jwks",
			"response_types_supported":              []string{"code"},
			"subject_types_supported":               []string{"public"},
			"id_token_signing_alg_values_supported": []string{"RS256"},
		})
	})
	mux.HandleFunc("/jwks", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{
			Key:       key.Public(),
			KeyID:     p.keyID,
			Algorithm: string(jose.RS256),
			Use:       "sig",
		}}})
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		p.lastVerifier = r.PostFormValue("code_verifier")

		nonce := p.pendingNonce
		if p.forceNonce != "" {
			nonce = p.forceNonce
		}
		aud := p.audience
		if aud == "" {
			aud = r.PostFormValue("client_id")
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"access_token": "at_" + p.subject,
			"token_type":   "Bearer",
			"expires_in":   3600,
			"id_token":     p.mintIDToken(t, aud, nonce),
		})
	})

	p.server = httptest.NewServer(mux)
	t.Cleanup(p.server.Close)
	return p
}

func (p *fakeProvider) mintIDToken(t *testing.T, audience, nonce string) string {
	t.Helper()
	now := time.Now()
	claims := map[string]any{
		"iss":            p.server.URL,
		"sub":            p.subject,
		"aud":            audience,
		"exp":            now.Add(time.Hour).Unix(),
		"iat":            now.Unix(),
		"email":          p.email,
		"email_verified": p.emailVerified,
		"name":           p.name,
	}
	if nonce != "" {
		claims["nonce"] = nonce
	}
	payload, err := json.Marshal(claims)
	if err != nil {
		t.Fatalf("marshal claims: %v", err)
	}

	signer, err := jose.NewSigner(
		jose.SigningKey{Algorithm: jose.RS256, Key: p.key},
		(&jose.SignerOptions{}).WithType("JWT").WithHeader("kid", p.keyID),
	)
	if err != nil {
		t.Fatalf("new signer: %v", err)
	}
	obj, err := signer.Sign(payload)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	raw, err := obj.CompactSerialize()
	if err != nil {
		t.Fatalf("serialize: %v", err)
	}
	return raw
}

// authorize simulates the browser leg: it reads the redirect the server built,
// remembers the nonce so the id_token can echo it, and returns the callback
// query the provider would send back.
func (p *fakeProvider) authorize(t *testing.T, authURL string) (code, state string) {
	t.Helper()
	u := mustParse(t, authURL)
	q := u.Query()

	if got := q.Get("code_challenge_method"); got != "S256" {
		t.Fatalf("PKCE challenge method should be S256, got %q", got)
	}
	if q.Get("code_challenge") == "" {
		t.Fatal("no PKCE code challenge on the authorize URL")
	}
	p.pendingNonce = q.Get("nonce")
	if p.pendingNonce == "" {
		t.Fatal("no nonce on the authorize URL")
	}
	return "auth-code-" + randomish(), q.Get("state")
}

func randomish() string {
	n, _ := rand.Int(rand.Reader, big.NewInt(1<<30))
	return base64.RawURLEncoding.EncodeToString(n.Bytes())
}
