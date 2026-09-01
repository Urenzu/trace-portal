// Package auth signs people in, and signs collectors in on their behalf.
//
// Two flows share one identity model:
//
//   - The browser flow is an ordinary OpenID Connect authorization code
//     exchange with PKCE. It is how a person reaches the UI.
//   - The CLI flow is RFC 8628 device authorization, served by this system
//     rather than by the identity provider. It is how a collector on an
//     engineer's laptop obtains a token without ever handling a password.
//
// Serving the device flow ourselves is the load-bearing decision. Most identity
// providers do not implement RFC 8628 at all, and those that do gate it behind
// per-provider configuration, so a CLI that spoke to the provider directly
// would be a CLI that worked with only some providers — the opposite of the
// requirement. Terminating both flows here means the provider only ever sees
// one client type, the ordinary web application it already knows how to serve,
// and the CLI's credential is ours to scope, expire and revoke.
//
// Provider-agnosticism has a second rule, enforced by what this file does not
// contain: nothing here reads a vendor-specific claim. Discovery supplies the
// endpoints, JWKS supplies the keys, and only claims defined by OIDC core are
// consulted. An organisation or a role asserted by a particular vendor is a
// mapping decision for the directory, not something the token reader assumes.
package auth

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"
)

// Config describes the identity provider. Everything except the issuer is
// per-deployment registration detail; the issuer is the only thing that decides
// which vendor is in use, and its endpoints are discovered rather than coded.
type Config struct {
	// IssuerURL is the OIDC issuer, e.g. https://accounts.example.com. Its
	// discovery document supplies every endpoint, so changing vendor is a
	// configuration change rather than a code change.
	IssuerURL string

	ClientID     string
	ClientSecret string

	// RedirectURL is this server's callback, registered with the provider. Only
	// the browser flow uses it; the device flow never leaves our own endpoints.
	RedirectURL string

	// Scopes beyond the openid/profile/email baseline. Left empty for most
	// deployments — asking for more than is needed is a liability rather than a
	// feature.
	Scopes []string

	// HTTPClient overrides the client used for discovery, JWKS and token
	// exchange. Tests point it at a local provider; production leaves it nil.
	HTTPClient *http.Client
}

func (c Config) validate() error {
	var missing []string
	if strings.TrimSpace(c.IssuerURL) == "" {
		missing = append(missing, "issuer URL")
	}
	if strings.TrimSpace(c.ClientID) == "" {
		missing = append(missing, "client id")
	}
	if strings.TrimSpace(c.RedirectURL) == "" {
		missing = append(missing, "redirect URL")
	}
	if len(missing) > 0 {
		return fmt.Errorf("auth config incomplete: missing %s", strings.Join(missing, ", "))
	}
	return nil
}

// Provider is a configured identity provider, ready to run the browser flow.
type Provider struct {
	verifier *oidc.IDTokenVerifier
	oauth    oauth2.Config
}

// NewProvider performs OIDC discovery against the issuer.
//
// It reaches the network, so it is called once at startup rather than per
// request. A provider that cannot be discovered is a fatal misconfiguration:
// starting anyway would mean serving a sign-in button that fails at the moment
// someone presses it.
func NewProvider(ctx context.Context, cfg Config) (*Provider, error) {
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	if cfg.HTTPClient != nil {
		ctx = oidc.ClientContext(ctx, cfg.HTTPClient)
	}

	p, err := oidc.NewProvider(ctx, strings.TrimRight(cfg.IssuerURL, "/"))
	if err != nil {
		return nil, fmt.Errorf("discover issuer %s: %w", cfg.IssuerURL, err)
	}

	scopes := append([]string{oidc.ScopeOpenID, "profile", "email"}, cfg.Scopes...)
	return &Provider{
		verifier: p.Verifier(&oidc.Config{ClientID: cfg.ClientID}),
		oauth: oauth2.Config{
			ClientID:     cfg.ClientID,
			ClientSecret: cfg.ClientSecret,
			RedirectURL:  cfg.RedirectURL,
			Endpoint:     p.Endpoint(),
			Scopes:       scopes,
		},
	}, nil
}

// AuthCodeURL builds the URL a browser is sent to.
//
// state defends the callback against cross-site request forgery, nonce binds
// the returned id_token to this particular request, and the PKCE verifier
// defends the code itself. All three are used rather than only the first: PKCE
// is no longer confined to public clients, and a confidential client that skips
// it is still exposed to a code intercepted at the redirect.
func (p *Provider) AuthCodeURL(state, nonce, pkceVerifier string) string {
	return p.oauth.AuthCodeURL(state,
		oidc.Nonce(nonce),
		oauth2.S256ChallengeOption(pkceVerifier),
	)
}

// Claims is the subset of an id_token this system is willing to depend on.
//
// Deliberately narrow. Every field here is defined by OIDC core, so a
// deployment can change identity vendor without any of it changing meaning.
type Claims struct {
	// Issuer and Subject together are the federated identity. Neither alone is
	// enough: subjects are only unique within an issuer.
	Issuer  string
	Subject string

	Email         string
	EmailVerified bool
	Name          string
}

// Federated returns the key a user record is looked up by.
//
// Email is deliberately not that key. It is reassignable — an address freed by
// a departing employee is routinely handed to a new one — and it is not unique
// across issuers, so keying on it would let one person inherit another's
// history. Issuer plus subject is the only pair the specification promises is
// both stable and unique.
func (c Claims) Federated() string { return c.Issuer + "|" + c.Subject }

// Exchange trades an authorization code for a verified identity.
//
// The nonce is checked against the one this server generated. Without that
// check an id_token captured from an unrelated sign-in could be replayed into
// this callback and would verify perfectly: the signature is valid and the
// audience is correct, because it was issued to this same client.
func (p *Provider) Exchange(ctx context.Context, code, pkceVerifier, nonce string) (Claims, error) {
	tok, err := p.oauth.Exchange(ctx, code, oauth2.VerifierOption(pkceVerifier))
	if err != nil {
		return Claims{}, fmt.Errorf("exchange authorization code: %w", err)
	}

	raw, ok := tok.Extra("id_token").(string)
	if !ok || raw == "" {
		// A response with no id_token has authenticated nobody. An access token
		// says the client may call an API; only an id_token says who signed in,
		// and treating the former as the latter is a long-standing way to
		// accept an identity that was never asserted.
		return Claims{}, errors.New("provider returned no id_token; the openid scope may not have been granted")
	}

	idToken, err := p.verifier.Verify(ctx, raw)
	if err != nil {
		return Claims{}, fmt.Errorf("verify id_token: %w", err)
	}
	if idToken.Nonce != nonce {
		return Claims{}, errors.New("id_token nonce does not match the request")
	}
	return readClaims(idToken)
}

// VerifyIDToken verifies a raw id_token without an exchange. Ordinary sign-in
// goes through Exchange; this is the seam for a token minted elsewhere, and for
// tests.
func (p *Provider) VerifyIDToken(ctx context.Context, raw string) (Claims, error) {
	idToken, err := p.verifier.Verify(ctx, raw)
	if err != nil {
		return Claims{}, err
	}
	return readClaims(idToken)
}

func readClaims(idToken *oidc.IDToken) (Claims, error) {
	var body struct {
		Email         string `json:"email"`
		EmailVerified bool   `json:"email_verified"`
		Name          string `json:"name"`
	}
	if err := idToken.Claims(&body); err != nil {
		return Claims{}, fmt.Errorf("read id_token claims: %w", err)
	}
	return Claims{
		Issuer:        idToken.Issuer,
		Subject:       idToken.Subject,
		Email:         body.Email,
		EmailVerified: body.EmailVerified,
		Name:          body.Name,
	}, nil
}
