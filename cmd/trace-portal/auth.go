package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/Urenzu/trace-portal/internal/auth"
	"github.com/Urenzu/trace-portal/internal/collect"
	"github.com/Urenzu/trace-portal/internal/tenant"
)

// Wiring for sign-in.
//
// Sign-in is off unless an issuer is configured, and that is the whole
// switch: with no issuer this is the single-user local tool it has always been,
// and the endpoints do not exist. There is deliberately no half-on state where
// accounts exist but are not enforced — that is the configuration in which
// someone believes their data is protected and it is not.

type authOptions struct {
	Issuer    string
	ClientID  string
	Secret    string
	PublicURL string
	Addr      string
	Log       *slog.Logger

	// Collect is the registry ingested batches are written into. Nil disables
	// the ingest endpoint, which is the local tool's case: it captures its own
	// turns and has nothing to receive.
	Collect *tenant.Registry
}

// signIn is what buildAuth hands back once sign-in is configured: the HTTP
// surface, and the sessions the read API resolves a tenant from. They are
// returned together because they must agree -- a read API routing on a
// different session store from the one the login flow writes to would serve
// every browser the same data while looking correct.
type signIn struct {
	Handler  http.Handler
	Sessions *auth.Server
}

// discoveryTimeout bounds OIDC discovery at startup. A provider that cannot be
// reached in ten seconds will not serve a sign-in either, and failing here —
// loudly, before the listener opens — beats failing under a person who just
// pressed a button.
const discoveryTimeout = 10 * time.Second

// buildAuth returns nil when sign-in is not configured.
func buildAuth(parent context.Context, o authOptions) (*signIn, error) {
	if strings.TrimSpace(o.Issuer) == "" {
		// Nothing configured, and nothing half-configured either: complain if
		// the other flags were set, because silently ignoring them is how a
		// deployment ends up open when its operator believes it is not.
		if o.ClientID != "" || o.Secret != "" {
			return nil, errors.New("-oidc-client-id set without -oidc-issuer; sign-in would be off")
		}
		return nil, nil
	}
	if o.ClientID == "" {
		return nil, errors.New("-oidc-issuer requires -oidc-client-id")
	}

	base := strings.TrimRight(o.PublicURL, "/")
	if base == "" {
		// The provider redirects a browser back here, and the device flow
		// prints a URL into a terminal that may be on another machine. Neither
		// can use a listen address like 0.0.0.0:8317, so this is required
		// rather than guessed.
		return nil, errors.New("-oidc-issuer requires -public-url, the address browsers reach this server on")
	}

	// Checked before discovery reaches the network: a deployment that would
	// carry session cookies over plain http is misconfigured, and saying so
	// immediately is better than saying so after a timeout.
	if !isLoopbackHTTP(base) && !strings.HasPrefix(base, "https://") {
		return nil, fmt.Errorf("-public-url must be https for a non-loopback deployment, got %q", base)
	}

	ctx, cancel := context.WithTimeout(parent, discoveryTimeout)
	defer cancel()

	provider, err := auth.NewProvider(ctx, auth.Config{
		IssuerURL:    o.Issuer,
		ClientID:     o.ClientID,
		ClientSecret: o.Secret,
		RedirectURL:  base + "/auth/callback",
	})
	if err != nil {
		return nil, err
	}

	// In-memory stores. Correct for a single-process deployment and a
	// deliberate limitation otherwise: sessions and pending sign-ins do not
	// survive a restart, and a second process would not see the first's. The
	// Postgres implementations land with the collector/server split; these
	// interfaces exist so that is a swap rather than a rewrite.
	directory := auth.NewMemoryDirectory(auth.TenantPerUser{})
	issuer := auth.NewIssuer(auth.NewMemoryTokens(), nil)
	device := auth.NewDeviceFlow(auth.NewMemoryDevices(), issuer, base+"/auth/device", nil)

	server, err := auth.NewServer(auth.ServerConfig{
		Provider:  provider,
		Directory: directory,
		Issuer:    issuer,
		Device:    device,
		Logger:    o.Log,
		AppURL:    base + "/",
		// Secure cookies everywhere except plain-http loopback, where a browser
		// would drop them and sign-in would fail with nothing to see.
		Secure: !isLoopbackHTTP(base),
	})
	if err != nil {
		return nil, err
	}

	mux := http.NewServeMux()
	server.Routes(mux)
	if o.Collect != nil {
		// The ingest endpoint exists only where sign-in does. A collector
		// presents a token, and without an issuer there is nothing that could
		// have minted one — an unauthenticated ingest endpoint would be an open
		// write path into somebody's archive.
		cs, err := collect.NewServer(o.Collect, server, o.Log, nil)
		if err != nil {
			return nil, err
		}
		cs.Routes(mux)
	}
	return &signIn{Handler: mux, Sessions: server}, nil
}

func isLoopbackHTTP(base string) bool {
	return strings.HasPrefix(base, "http://127.0.0.1") ||
		strings.HasPrefix(base, "http://localhost") ||
		strings.HasPrefix(base, "http://[::1]")
}

// envOverride lets an environment variable win over a flag default.
//
// Backwards from the usual precedence on purpose. Flags are how a person runs
// this at a terminal; environment variables are how a container is configured,
// and a container image carries the flag defaults baked into its CMD. If the
// flag won, an operator's environment would be silently ignored by the one
// deployment shape that has no other way to configure anything.
func envOverride(dst *string, key string) {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		*dst = v
	}
}
