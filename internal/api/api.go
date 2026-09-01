// Package api serves the read side of trace-portal: sessions, turns, aggregate
// stats, and on-demand payload fetches.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/Urenzu/trace-portal/internal/compact"
	"github.com/Urenzu/trace-portal/internal/identity"
	"github.com/Urenzu/trace-portal/internal/source"
	"github.com/Urenzu/trace-portal/internal/trace"
)

// defaultWindowDays bounds an unqualified query so a long-lived trace directory
// does not turn every page load into a full-history scan.
const defaultWindowDays = 7

// Page sizes for the session list.
const (
	defaultPageSize = 50
	maxPageSize     = 500

	// maxWindowDays bounds how far back any query may reach.
	//
	// Every read path walks the window a day at a time, and a day with no data
	// still costs the file opens that establish that. An unbounded `days` is
	// therefore an unbounded amount of work from a single URL: `?days=200000`
	// measured at fifteen seconds of pure syscalls on an archive holding one
	// month. Ten years is far more history than this tool can hold — the
	// transcripts it reads are pruned after about one — so the ceiling costs
	// nothing real and turns a hang into an ordinary answer.
	maxWindowDays = 3650
)

// CoverageReporter supplies what ingestion understood, so the UI can say what
// it does not know instead of presenting gaps as measurements.
type CoverageReporter interface {
	Coverage() map[string]*source.Coverage
}

// Archive is what the read API needs from the hot window: which days exist, the
// identity it stamps, and the payloads behind a turn. Everything analytical
// comes from the compactor instead.
type Archive interface {
	Days(ctx context.Context) ([]time.Time, error)
	Identity() trace.Identity
	GetBlob(ctx context.Context, ref string) ([]byte, error)
}

// Scope is the storage that answers one request: one tenant's hot window, its
// compactor, and -- only for the archive this process captures into -- what
// ingestion understood about the transcripts it read.
//
// Coverage is per-process, not per-tenant: it describes a tailer reading this
// machine's disk. A server holding somebody else's shipped turns has no tailer
// for them, so it is nil there and the health endpoint reports none.
type Scope struct {
	Store    Archive
	Compact  *compact.Compactor
	Coverage CoverageReporter
}

// ErrNoSession means the request carried no session on a server that requires
// one. It is separated from other refusals because it is the one a browser can
// act on: it is answered with a 401, which is the UI's cue to offer sign-in.
var ErrNoSession = errors.New("not signed in")

// Resolver decides whose data answers a request.
//
// This is the read side of the rule the ingest endpoint already follows: the
// tenant comes from a credential and from nothing else. Handlers below receive
// a Scope and hold no reference to any other, which is why no query in this
// package carries a tenant predicate -- there is nothing for one to filter.
type Resolver interface {
	Scope(r *http.Request) (Scope, error)
}

// Fixed is the local tool's resolver: one archive, no sign-in, the same answer
// to every request.
//
// The local tool goes through the resolver rather than around it so the
// multi-tenant path is the path every request takes, including the ones run on
// a laptop. A branch that only executes in production is a branch nobody tests.
func Fixed(sc Scope) Resolver { return fixed{sc} }

type fixed struct{ sc Scope }

func (f fixed) Scope(*http.Request) (Scope, error) { return f.sc, nil }

// Server exposes the query API. Reads go through the compactor, which serves
// Parquet partitions where they exist and falls back to raw JSONL otherwise;
// blob fetches go straight to the store.
type Server struct {
	tenants Resolver
	log     *slog.Logger
}

// New builds a Server over a resolver.
func New(tenants Resolver, log *slog.Logger) *Server {
	if log == nil {
		log = slog.Default()
	}
	return &Server{tenants: tenants, log: log}
}

// scope resolves the tenant this request may read, and writes the refusal
// itself when there is none.
//
// Every handler starts here, and one that forgot would not compile: there is no
// store and no compactor on the Server to reach past it.
func (s *Server) scope(w http.ResponseWriter, r *http.Request) (Scope, bool) {
	sc, err := s.tenants.Scope(r)
	if err != nil {
		if errors.Is(err, ErrNoSession) {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "not signed in"})
			return Scope{}, false
		}
		// Everything else is one answer. A caller learning that a tenant exists
		// but is not theirs has learned something about another tenant.
		s.log.Warn("refused a request that resolved to no tenant", "err", err)
		s.fail(w, http.StatusForbidden, errors.New("not permitted"))
		return Scope{}, false
	}
	return sc, true
}

// Handler returns the API routes, all rooted at /api/.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/health", s.handleHealth)
	mux.HandleFunc("GET /api/sessions", s.handleSessions)
	mux.HandleFunc("GET /api/sessions/{id}", s.handleSession)
	mux.HandleFunc("GET /api/stats", s.handleStats)
	mux.HandleFunc("GET /api/projects/{id}", s.handleProject)
	mux.HandleFunc("GET /api/blobs/{ref}", s.handleBlob)
	return mux
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	// Health answers before sign-in does. A container probe and the UI's first
	// request both hit this, and neither has a session: making it 401 would
	// mean an orchestrator restarting a healthy server in a loop, and a UI with
	// no way to discover that it should offer a sign-in button.
	sc, err := s.tenants.Scope(r)
	if errors.Is(err, ErrNoSession) {
		writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "signed_in": false})
		return
	}
	if err != nil {
		s.fail(w, http.StatusForbidden, errors.New("not permitted"))
		return
	}

	// From the compactor rather than the store, because on a server the hot
	// window no longer holds compacted days -- asking it would report an
	// archive that begins at the last compaction.
	days, err := sc.Compact.Days(r.Context())
	if err != nil {
		s.fail(w, http.StatusInternalServerError, err)
		return
	}
	// Identity tells a client which of two products it is talking to: an
	// unenrolled local install, which can offer to sign in, or an enrolled one,
	// which can say whose data is on screen. Only the ids are exposed; the
	// collector token never leaves the enrollment file.
	id := sc.Store.Identity()
	resp := map[string]any{
		"status":        "ok",
		"signed_in":     true,
		"days_captured": len(days),
		"identity": map[string]any{
			"tenant_id": id.TenantID,
			"user_id":   id.UserID,
			"local":     id.TenantID == identity.LocalTenant && id.UserID == identity.LocalUser,
		},
	}
	if len(days) > 0 {
		resp["first_day"] = days[0].Format("2006-01-02")
		resp["last_day"] = days[len(days)-1].Format("2006-01-02")

		// Per-day volume, not just the span. A first and last date read as
		// continuous coverage; they are not, and the days with nothing in them
		// are the shape of how the agent was actually used.
		series, err := sc.Compact.DailyRange(days[0], days[len(days)-1])
		if err != nil {
			s.fail(w, http.StatusInternalServerError, err)
			return
		}
		resp["days"] = series
	}
	if sc.Coverage != nil {
		resp["coverage"] = sc.Coverage.Coverage()
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handleSessions(w http.ResponseWriter, r *http.Request) {
	sc, ok := s.scope(w, r)
	if !ok {
		return
	}
	from, to := s.window(r)
	limit, ok := intParam(r, "limit")
	if !ok {
		limit = defaultPageSize
	}
	if limit > maxPageSize {
		limit = maxPageSize
	}

	// Listing walks days backwards and stops once the page is full, so the
	// common case reads one partition rather than the whole window. A search
	// narrows what qualifies, so it runs here rather than in the browser: the
	// alternative would only ever search the page already loaded.
	filter := compact.ParseFilter(r.URL.Query().Get("q"))
	cursor := r.URL.Query().Get("cursor")
	order := compact.ParseOrder(r.URL.Query().Get("sort"))

	// Recency is the only order the backwards walk already produces. Any other
	// has to see the whole window before it knows what comes first, so it takes
	// the ranked path — deliberately, and only when asked for.
	var page compact.SessionPage
	var err error
	if order.Lazy() {
		page, err = sc.Compact.SessionsPage(from, to, limit, cursor, filter)
	} else {
		page, err = sc.Compact.SessionsRanked(from, to, limit, cursor, filter, order)
	}
	if err != nil {
		s.fail(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, page)
}

func (s *Server) handleSession(w http.ResponseWriter, r *http.Request) {
	sc, ok := s.scope(w, r)
	if !ok {
		return
	}
	from, to := s.window(r)
	id := r.PathValue("id")

	detail, found, err := sc.Compact.SessionDetail(from, to, id)
	if err != nil {
		s.fail(w, http.StatusInternalServerError, err)
		return
	}
	if !found {
		s.fail(w, http.StatusNotFound, fmt.Errorf("session %q not found in this window", id))
		return
	}
	writeJSON(w, http.StatusOK, detail)
}

// ProjectStat is one project's share of the window.
type ProjectStat struct {
	Project      string  `json:"project"`
	ProjectID    string  `json:"project_id"`
	InRepo       bool    `json:"in_repo"`
	Turns        int     `json:"turns"`
	Sessions     int     `json:"sessions"`
	CostUSD      float64 `json:"cost_usd"`
	CacheHitRate float64 `json:"cache_hit_rate"`
	Errors       int     `json:"errors,omitempty"`
	InputTokens  int     `json:"input_tokens"`
	OutputTokens int     `json:"output_tokens"`
}

// Stats is the aggregate dashboard payload.
type Stats struct {
	From         time.Time   `json:"from"`
	To           time.Time   `json:"to"`
	Sessions     int         `json:"sessions"`
	Turns        int         `json:"turns"`
	Errors       int         `json:"errors"`
	Usage        trace.Usage `json:"usage"`
	CostUSD      float64     `json:"cost_usd"`
	CacheHitRate float64     `json:"cache_hit_rate"`
	// SavingsUSD is what the cached input would have cost at full input price
	// minus what it actually cost — the money prompt caching saved.
	SavingsUSD  float64        `json:"savings_usd"`
	ByProject   []ProjectStat  `json:"projects,omitempty"`
	ByModel     map[string]int `json:"turns_by_model,omitempty"`
	ToolCalls   map[string]int `json:"tool_calls_by_name,omitempty"`
	UnpricedRun int            `json:"unpriced_turns,omitempty"`

	// ByDay is the window as a series. Totals say what something cost; only a
	// series says whether that is rising, and the day rollups already hold it.
	ByDay []compact.DayPoint `json:"by_day,omitempty"`

	// SessionsExact is false when part of the window was answered from day
	// rollups, which count a session once per day it touched. Totals stay
	// exact; the session count becomes an upper bound.
	SessionsExact bool `json:"sessions_exact"`
}

func (s *Server) handleStats(w http.ResponseWriter, r *http.Request) {
	sc, ok := s.scope(w, r)
	if !ok {
		return
	}
	from, to := s.window(r)
	agg, err := sc.Compact.AggregateRange(from, to)
	if err != nil {
		s.fail(w, http.StatusInternalServerError, err)
		return
	}
	stats := statsFromAggregate(agg, from, to)
	if stats.ByDay, err = sc.Compact.DailyRange(from, to); err != nil {
		s.fail(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, stats)
}

func (s *Server) handleBlob(w http.ResponseWriter, r *http.Request) {
	sc, ok := s.scope(w, r)
	if !ok {
		return
	}
	payload, err := sc.Store.GetBlob(r.Context(), r.PathValue("ref"))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			s.fail(w, http.StatusNotFound, fmt.Errorf("blob not found"))
			return
		}
		s.fail(w, http.StatusBadRequest, err)
		return
	}
	// Blobs are stored verbatim as captured, so they are already JSON (or an
	// SSE transcript); serve the bytes untouched.
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	w.Write(payload)
}

// window resolves ?from= and ?to= (RFC3339 or a plain date), defaulting to the
// last defaultWindowDays.
func (s *Server) window(r *http.Request) (time.Time, time.Time) {
	to := parseTime(r.URL.Query().Get("to"), time.Now().UTC())
	from := parseTime(r.URL.Query().Get("from"), to.AddDate(0, 0, -defaultWindowDays))
	if days, ok := intParam(r, "days"); ok && days > 0 {
		if days > maxWindowDays {
			days = maxWindowDays
		}
		from = to.AddDate(0, 0, -days)
	}
	// An explicit `from` is clamped too, and a reversed range is put back in
	// order rather than being walked backwards to no result.
	if earliest := to.AddDate(0, 0, -maxWindowDays); from.Before(earliest) {
		from = earliest
	}
	if from.After(to) {
		from, to = to, from
	}
	return from, to
}

func parseTime(raw string, fallback time.Time) time.Time {
	if raw == "" {
		return fallback
	}
	for _, layout := range []string{time.RFC3339, "2006-01-02"} {
		if t, err := time.ParseInLocation(layout, raw, time.UTC); err == nil {
			return t
		}
	}
	return fallback
}

func intParam(r *http.Request, name string) (int, bool) {
	raw := r.URL.Query().Get(name)
	if raw == "" {
		return 0, false
	}
	n, err := strconv.Atoi(raw)
	return n, err == nil
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(body); err != nil {
		// The status line is already sent; nothing useful is left to do.
		slog.Default().Warn("encode response", "err", err)
	}
}

func (s *Server) fail(w http.ResponseWriter, status int, err error) {
	if status >= 500 {
		s.log.Error("api request failed", "status", status, "err", err)
	}
	writeJSON(w, status, map[string]string{"error": err.Error()})
}
