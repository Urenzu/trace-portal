// Command trace-portal runs the capturing reverse proxy for the Anthropic API.
//
// Point an instrumented app at it with no other changes:
//
//	ANTHROPIC_BASE_URL=http://localhost:8317 your-app
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Urenzu/trace-portal/internal/api"
	"github.com/Urenzu/trace-portal/internal/collect"
	"github.com/Urenzu/trace-portal/internal/identity"
	"github.com/Urenzu/trace-portal/internal/ingest"
	"github.com/Urenzu/trace-portal/internal/objectstore"
	"github.com/Urenzu/trace-portal/internal/postgres"
	"github.com/Urenzu/trace-portal/internal/proxy"
	"github.com/Urenzu/trace-portal/internal/source"
	"github.com/Urenzu/trace-portal/internal/tenant"
	"github.com/Urenzu/trace-portal/internal/trace"
	"github.com/Urenzu/trace-portal/internal/web"
)

func main() {
	if err := dispatch(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "trace-portal:", err)
		os.Exit(1)
	}
}

// dispatch routes the subcommands that manage enrollment, and falls through to
// the server for everything else.
//
// Falling through rather than requiring a verb keeps the one command anybody
// runs — `trace-portal`, which starts capturing — exactly as it was. A tool
// that grew a mandatory `serve` the day it learned to sign in would have broken
// every existing invocation for the sake of symmetry.
func dispatch(args []string) error {
	if len(args) > 0 {
		switch args[0] {
		case "login":
			return runLogin(args[1:])
		case "logout":
			return runLogout(args[1:])
		case "whoami":
			return runWhoami(args[1:])
		}
	}
	return run(args)
}

func run(args []string) error {
	var (
		addr         = flag.String("addr", "127.0.0.1:8317", "address to listen on")
		dataDir      = flag.String("data", defaultDataDir(), "directory for traces and blobs")
		upstream     = flag.String("upstream", proxy.DefaultUpstream, "upstream API base URL")
		verbose      = flag.Bool("v", false, "verbose logging")
		compactEvery = flag.Duration("compact-every", time.Hour, "how often to compact completed days into Parquet (0 disables)")
		pollEvery    = flag.Duration("poll", 2*time.Second, "how often to check agent logs for new turns (0 disables tailing)")
		enableProxy  = flag.Bool("proxy", false, "also accept proxied API traffic (only needed for tools that keep no local log)")
		claudeDir    = flag.String("claude-dir", "", "Claude Code transcript directory (default ~/.claude/projects)")

		// Content capture. On by default, and the default is the whole point:
		// Claude Code prunes its transcripts after about a month, so a day this
		// spends off is a day of prompts, tool arguments and tool outputs that
		// no later build can recover. It stays on this machine -- the collector
		// ships measurements, not text; see the forwarder.
		captureContent = flag.Bool("content", true, "capture prompts, tool inputs and tool outputs alongside the measurements")
		maxContent     = flag.Int("max-content", source.DefaultMaxContent, "bytes kept per captured block")
		recapture      = flag.Bool("recapture", false, "re-read transcripts already consumed, to capture content from them before the source prunes them")

		// Sign-in. Absent, this is the single-user local tool it has always
		// been: no accounts, no cookies, no sign-in button. Present, the same
		// binary serves both flows. Nothing in between — a half-configured
		// provider is refused at startup rather than discovered by the first
		// person to press the button.
		oidcIssuer   = flag.String("oidc-issuer", "", "OpenID Connect issuer URL; enables sign-in when set")
		oidcClientID = flag.String("oidc-client-id", "", "OAuth client id registered with the issuer")
		oidcSecret   = flag.String("oidc-client-secret", "", "OAuth client secret (prefer TRACE_PORTAL_OIDC_SECRET)")
		publicURL    = flag.String("public-url", "", "externally reachable base URL, e.g. https://app.example.com")
		shipEvery    = flag.Duration("ship", 30*time.Second, "how often to ship captured turns to the server, once signed in (0 disables)")
		postgresURL  = flag.String("postgres", "", "Postgres URL for the hot window; empty keeps the local file log (prefer TRACE_PORTAL_POSTGRES)")
		s3Endpoint   = flag.String("s3-endpoint", "", "S3-compatible endpoint for compacted history; empty keeps partitions on local disk")
		s3Bucket     = flag.String("s3-bucket", "trace-portal", "bucket holding compacted partitions")
		s3Region     = flag.String("s3-region", "auto", "bucket region (\"auto\" for R2)")
		s3Key        = flag.String("s3-key", "", "S3 access key (prefer TRACE_PORTAL_S3_KEY)")
		s3Secret     = flag.String("s3-secret", "", "S3 secret key (prefer TRACE_PORTAL_S3_SECRET)")
	)
	if err := flag.CommandLine.Parse(args); err != nil {
		return err
	}

	level := slog.LevelInfo
	if *verbose {
		level = slog.LevelDebug
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))

	// Configuration is read from the environment in preference to flags, because
	// a container is configured with environment variables and because a secret
	// on a command line is visible in the process table to every other user on
	// the machine, and ends up in shell history.
	envOverride(oidcSecret, "TRACE_PORTAL_OIDC_SECRET")
	envOverride(oidcIssuer, "TRACE_PORTAL_OIDC_ISSUER")
	envOverride(oidcClientID, "TRACE_PORTAL_OIDC_CLIENT_ID")
	envOverride(publicURL, "TRACE_PORTAL_PUBLIC_URL")
	envOverride(postgresURL, "TRACE_PORTAL_POSTGRES")
	envOverride(s3Endpoint, "TRACE_PORTAL_S3_ENDPOINT")
	envOverride(s3Bucket, "TRACE_PORTAL_S3_BUCKET")
	envOverride(s3Region, "TRACE_PORTAL_S3_REGION")
	envOverride(s3Key, "TRACE_PORTAL_S3_KEY")
	envOverride(s3Secret, "TRACE_PORTAL_S3_SECRET")

	// Every read and write goes through a tenant registry, even here where
	// there is exactly one tenant. Routing the local tool through the same call
	// the server uses means the isolation path is exercised by every request
	// anybody makes, rather than being a branch that only runs in production.
	enrollment, err := identity.Load(*dataDir)
	if err != nil {
		return err
	}
	registry, err := tenant.NewSingle(*dataDir, enrollment.TenantID)
	if err != nil {
		return err
	}
	defer registry.Close()

	// The storage flags say which backend, not which mode.
	//
	// They apply to this process's own capture as much as to anything it
	// receives, which is what makes the deployment shape runnable on a laptop:
	// point a local trace-portal at Postgres and a bucket and it behaves exactly
	// like a deployed one, against real transcripts. Unset -- the default, and
	// what anybody who just runs the binary gets -- both stay local files, and
	// "one binary, nothing to install" is intact.
	var (
		pool    *pgxpool.Pool
		objects func(prefix string) (objectstore.Store, error)
	)
	if *postgresURL != "" {
		pool, err = postgres.Pool(context.Background(), postgres.Config{URL: *postgresURL})
		if err != nil {
			return err
		}
		defer pool.Close()
		registry = registry.WithPostgres(pool, enrollment.Identity())
		log.Info("hot window in postgres")
	}
	if *s3Endpoint != "" {
		cfg := objectstore.S3Config{
			Endpoint:  *s3Endpoint,
			Bucket:    *s3Bucket,
			AccessKey: *s3Key,
			SecretKey: *s3Secret,
			Region:    *s3Region,
		}
		// Connected once here so a bad endpoint or a wrong key fails at startup
		// rather than on the first compaction, hours later, in a background job
		// nobody is watching.
		probeCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		_, err := objectstore.NewS3(probeCtx, cfg)
		cancel()
		if err != nil {
			return fmt.Errorf("object storage: %w", err)
		}
		objects = func(prefix string) (objectstore.Store, error) {
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			s3, err := objectstore.NewS3(ctx, cfg)
			if err != nil {
				return nil, err
			}
			return objectstore.Prefixed(s3, prefix), nil
		}
		registry = registry.WithObjectStorage(objects)
		log.Info("compacted history in object storage", "endpoint", *s3Endpoint, "bucket", *s3Bucket)
	}

	storage, err := registry.For(enrollment.TenantID)
	if err != nil {
		return err
	}
	st := storage.Store

	p, err := proxy.New(proxy.Config{Upstream: *upstream, Store: st, Logger: log})
	if err != nil {
		return err
	}

	compactor := storage.Compactor

	// Tailing agent logs is the default way traces are collected: it reads what
	// the tools already write, so nothing sits in an agent's request path and
	// a failure here can never stop an agent from working.
	claudeCode := source.NewClaudeCode(*claudeDir)
	claudeCode.CaptureContent(*captureContent, *maxContent)
	sources := []source.Source{claudeCode}
	for _, src := range sources {
		if files, err := src.Files(); err == nil && len(files) > 0 {
			log.Info("watching agent logs", "source", src.Name(), "files", len(files), "dir", src.Root())
		}
	}
	if *captureContent {
		log.Info("capturing content", "max_block_bytes", *maxContent, "shipped", false)
	}
	ingester := ingest.New(st, *dataDir, log, sources...)
	ingester.Recapture(*captureContent, *recapture)
	// Persist offsets and coverage on the way out, so a clean shutdown keeps
	// what this run learned.
	defer ingester.Close()

	// Receiving is a separate registry from the one this process captures into.
	// A server holds many tenants under <data>/tenants/<id>/; the local archive
	// is this machine own and stays where it has always been. Keeping them
	// apart is what stops a server own captured turns from landing in a
	// customer subtree, or the reverse.
	//
	// The backends are shared: one connection pool and one bucket, scoped per
	// tenant inside the registry rather than by opening a second connection.
	var receiving *tenant.Registry
	if *oidcIssuer != "" {
		if receiving, err = tenant.NewPartitioned(*dataDir); err != nil {
			return err
		}
		defer receiving.Close()

		if pool != nil {
			receiving = receiving.WithPostgres(pool, trace.Identity{})
		} else {
			log.Warn("no -postgres configured; received data goes to the local file log",
				"hint", "set TRACE_PORTAL_POSTGRES for a real deployment")
		}
		if objects != nil {
			receiving = receiving.WithObjectStorage(objects)
		} else {
			log.Warn("no -s3-endpoint configured; compacted partitions stay on local disk",
				"hint", "set TRACE_PORTAL_S3_ENDPOINT for a real deployment")
		}
	}

	signin, err := buildAuth(context.Background(), authOptions{
		Issuer:    *oidcIssuer,
		ClientID:  *oidcClientID,
		Secret:    *oidcSecret,
		PublicURL: *publicURL,
		Addr:      *addr,
		Log:       log,
		Collect:   receiving,
	})
	if err != nil {
		return err
	}
	var authHandler http.Handler
	// The read API resolves its tenant the same way ingest does: from a
	// credential. With sign-in configured that is the session cookie, and a
	// browser sees its own account's data and nothing else. Without it, this is
	// the local tool and the one archive answers every request.
	//
	// Coverage is attached only in the local case, because it describes the
	// tailer reading this machine's transcripts -- there is no such thing for a
	// tenant whose turns arrived over the wire.
	resolver := api.Fixed(api.Scope{Store: st, Compact: compactor, Coverage: ingester})
	if signin != nil {
		authHandler = signin.Handler
		resolver = api.FromSession(signin.Sessions, receiving)
		log.Info("sign-in enabled", "issuer", *oidcIssuer)
	}
	apiHandler := api.New(resolver, log).Handler()

	srv := &http.Server{
		Addr:              *addr,
		Handler:           route(p, apiHandler, web.Handler(), authHandler, *enableProxy),
		ReadHeaderTimeout: 10 * time.Second,
		// No write timeout: streaming responses are long-lived by design.
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	// Every registry, not just this process's own. A server sweeping only the
	// tenant it happens to capture into would compact one archive and leave
	// every customer's window growing in Postgres forever.
	registries := []*tenant.Registry{registry}
	if receiving != nil {
		registries = append(registries, receiving)
	}
	// A re-read has to be followed by a rebuild, and cannot overlap with one:
	// a partition rebuilt halfway through the re-read would be rebuilt without
	// the rest. Ordinary startup waits for nothing.
	var afterBackfill <-chan struct{}
	if *recapture {
		afterBackfill = ingester.Backfilled()
	}
	go runCompaction(ctx, registries, *compactEvery, log, afterBackfill)

	go func() {
		defer func() {
			// A panic here must not take the process down with it.
			if r := recover(); r != nil {
				log.Error("ingest stopped after a panic", "panic", r)
			}
		}()
		if err := ingester.Run(ctx, *pollEvery); err != nil {
			log.Warn("ingest stopped", "err", err)
		}
	}()

	// Shipping runs only when this installation has signed in, and it ships
	// from the archive rather than from the tailer. That ordering is the whole
	// reliability argument for the collector: what is captured is durable
	// locally before anything is sent, so a server that is down, slow or
	// mid-upgrade delays delivery and can never cost a turn.
	if !enrollment.Local() && enrollment.Server != "" && enrollment.Token != "" {
		shipper, err := collect.NewShipper(enrollment.Server, enrollment.Token, nil, log)
		if err != nil {
			return err
		}
		shipper.Version = version
		forwarder, err := collect.NewForwarder(st, shipper, *dataDir, log)
		if err != nil {
			return err
		}
		log.Info("shipping to server", "server", enrollment.Server, "user", enrollment.UserID)
		go func() {
			defer func() {
				if r := recover(); r != nil {
					log.Error("shipping stopped after a panic", "panic", r)
				}
			}()
			if err := forwarder.Run(ctx, *shipEvery); err != nil {
				log.Warn("shipping stopped", "err", err)
			}
		}()
	}

	errc := make(chan error, 1)
	go func() {
		log.Info("listening", "addr", *addr, "upstream", *upstream, "data", *dataDir)
		if *enableProxy {
			log.Info("proxy enabled", "env", "ANTHROPIC_BASE_URL=http://"+*addr)
		}
		if web.Available() {
			log.Info("open the ui", "url", "http://"+*addr)
		} else {
			log.Warn("frontend not built; api only", "hint", "run: cd web && npm install && npm run build")
		}
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errc <- err
		}
	}()

	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
		log.Info("shutting down")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return srv.Shutdown(shutdownCtx)
	}
}

func defaultDataDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ".trace-portal"
	}
	return filepath.Join(home, ".trace-portal")
}

// route splits one port three ways so the tool stays a single process behind a
// single URL:
//
//	/api/collect  authenticated ingest from collectors
//	/api/…   the query API
//	/auth/…  sign-in, when an identity provider is configured
//	/v1/…    proxied upstream — the Anthropic API surface lives entirely here
//	/…       the embedded UI
//
// Anchoring the proxy to /v1/ is what lets the UI own the root. Any future
// upstream path outside /v1/ would need adding here, which is the tradeoff for
// not making the user run two ports.
// collectPath is the one write endpoint, named here so the router and the
// handler cannot drift apart.
const collectPath = "/api/collect"

func route(proxyHandler, apiHandler, uiHandler, authHandler http.Handler, proxyEnabled bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/auth/"):
			if authHandler == nil {
				// Say so rather than falling through to the UI, which would
				// answer a sign-in request with an HTML page and look like the
				// route was wrong rather than the feature switched off.
				http.Error(w, "sign-in is not configured on this server", http.StatusNotFound)
				return
			}
			authHandler.ServeHTTP(w, r)
		case r.URL.Path == collectPath:
			// Ingest lives under /api/ because it is an API, but it is served by
			// the authenticated mux rather than the query API: it is the only
			// /api/ path that writes, and the only one that takes a collector
			// credential rather than a browser session.
			if authHandler == nil {
				http.Error(w, "this server does not accept collected traces", http.StatusNotFound)
				return
			}
			authHandler.ServeHTTP(w, r)
		case strings.HasPrefix(r.URL.Path, "/api/"):
			apiHandler.ServeHTTP(w, r)
		case strings.HasPrefix(r.URL.Path, "/v1/"):
			if !proxyEnabled {
				// Refuse clearly rather than proxying by accident: pointing an
				// agent here without -proxy would otherwise look like it worked
				// while recording nothing.
				http.Error(w, "proxying is disabled; start trace-portal with -proxy", http.StatusNotFound)
				return
			}
			proxyHandler.ServeHTTP(w, r)
		default:
			uiHandler.ServeHTTP(w, r)
		}
	})
}

// runCompaction rewrites completed days into Parquet in the background. It runs
// once at startup to pick up days that accumulated while the proxy was down,
// then on a ticker. Failures are logged and retried on the next tick rather
// than taken as fatal: the raw JSONL is still the source of truth, and the read
// path falls back to it for any day without a partition.
func runCompaction(ctx context.Context, registries []*tenant.Registry, every time.Duration, log *slog.Logger, after <-chan struct{}) {
	if every <= 0 {
		log.Info("compaction disabled")
		return
	}

	// rebuild is set for the first sweep after a re-read, and only that one.
	rebuild := after != nil
	if after != nil {
		select {
		case <-after:
		case <-ctx.Done():
			return
		}
	}

	defer func() {
		if r := recover(); r != nil {
			log.Error("compaction stopped after a panic", "panic", r)
		}
	}()

	// One tenant's failure must not stop the others. A bucket that rejects one
	// customer's writes is their problem to fix; leaving every other customer
	// uncompacted while it is investigated would make it everybody's.
	compactTenant := func(reg *tenant.Registry, id string) {
		start := time.Now()
		storage, err := reg.For(id)
		if err != nil {
			log.Warn("could not open tenant storage for compaction", "tenant", id, "err", err)
			return
		}
		compact := storage.Compactor.CompactAll
		if rebuild {
			compact = storage.Compactor.RecompactAll
		}
		n, err := compact()
		if err != nil {
			log.Warn("compaction failed", "tenant", id, "err", err)
			return
		}
		if n > 0 {
			log.Info("compacted days", "tenant", id, "count", n, "took", time.Since(start).Round(time.Millisecond))
		}
	}

	run := func() {
		for _, reg := range registries {
			tenants, err := reg.Tenants()
			if err != nil {
				log.Warn("could not list tenants for compaction", "err", err)
				continue
			}
			for _, id := range tenants {
				compactTenant(reg, id)
			}
		}
	}

	run()
	if rebuild {
		log.Info("rebuilt every partition after re-reading transcripts")
		rebuild = false
	}
	ticker := time.NewTicker(every)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			run()
		}
	}
}
