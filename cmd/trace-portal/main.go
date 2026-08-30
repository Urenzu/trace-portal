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

	"github.com/Urenzu/trace-portal/internal/api"
	"github.com/Urenzu/trace-portal/internal/compact"
	"github.com/Urenzu/trace-portal/internal/ingest"
	"github.com/Urenzu/trace-portal/internal/proxy"
	"github.com/Urenzu/trace-portal/internal/source"
	"github.com/Urenzu/trace-portal/internal/store"
	"github.com/Urenzu/trace-portal/internal/web"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "trace-portal:", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		addr         = flag.String("addr", "127.0.0.1:8317", "address to listen on")
		dataDir      = flag.String("data", defaultDataDir(), "directory for traces and blobs")
		upstream     = flag.String("upstream", proxy.DefaultUpstream, "upstream API base URL")
		verbose      = flag.Bool("v", false, "verbose logging")
		compactEvery = flag.Duration("compact-every", time.Hour, "how often to compact completed days into Parquet (0 disables)")
		pollEvery    = flag.Duration("poll", 2*time.Second, "how often to check agent logs for new turns (0 disables tailing)")
		enableProxy  = flag.Bool("proxy", false, "also accept proxied API traffic (only needed for tools that keep no local log)")
		claudeDir    = flag.String("claude-dir", "", "Claude Code transcript directory (default ~/.claude/projects)")
	)
	flag.Parse()

	level := slog.LevelInfo
	if *verbose {
		level = slog.LevelDebug
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))

	st, err := store.Open(*dataDir)
	if err != nil {
		return err
	}
	defer st.Close()

	p, err := proxy.New(proxy.Config{Upstream: *upstream, Store: st, Logger: log})
	if err != nil {
		return err
	}

	compactor, err := compact.New(st, *dataDir)
	if err != nil {
		return err
	}

	// Tailing agent logs is the default way traces are collected: it reads what
	// the tools already write, so nothing sits in an agent's request path and
	// a failure here can never stop an agent from working.
	sources := []source.Source{
		source.NewClaudeCode(*claudeDir),
	}
	for _, src := range sources {
		if files, err := src.Files(); err == nil && len(files) > 0 {
			log.Info("watching agent logs", "source", src.Name(), "files", len(files), "dir", src.Root())
		}
	}
	ingester := ingest.New(st, *dataDir, log, sources...)
	// Persist offsets and coverage on the way out, so a clean shutdown keeps
	// what this run learned.
	defer ingester.Close()

	srv := &http.Server{
		Addr:              *addr,
		Handler:           route(p, api.New(st, compactor, ingester, log).Handler(), web.Handler(), *enableProxy),
		ReadHeaderTimeout: 10 * time.Second,
		// No write timeout: streaming responses are long-lived by design.
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	go runCompaction(ctx, compactor, *compactEvery, log)

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
//	/api/…   the query API
//	/v1/…    proxied upstream — the Anthropic API surface lives entirely here
//	/…       the embedded UI
//
// Anchoring the proxy to /v1/ is what lets the UI own the root. Any future
// upstream path outside /v1/ would need adding here, which is the tradeoff for
// not making the user run two ports.
func route(proxyHandler, apiHandler, uiHandler http.Handler, proxyEnabled bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
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
func runCompaction(ctx context.Context, c *compact.Compactor, every time.Duration, log *slog.Logger) {
	if every <= 0 {
		log.Info("compaction disabled")
		return
	}

	defer func() {
		if r := recover(); r != nil {
			log.Error("compaction stopped after a panic", "panic", r)
		}
	}()

	run := func() {
		start := time.Now()
		n, err := c.CompactAll()
		if err != nil {
			log.Warn("compaction failed", "err", err)
			return
		}
		if n > 0 {
			log.Info("compacted days", "count", n, "took", time.Since(start).Round(time.Millisecond))
		}
	}

	run()
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
