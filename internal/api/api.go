// Package api serves the read side of trace-portal: sessions, turns, aggregate
// stats, and on-demand payload fetches.
package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/Urenzu/trace-portal/internal/compact"
	"github.com/Urenzu/trace-portal/internal/source"
	"github.com/Urenzu/trace-portal/internal/store"
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

// Server exposes the query API. Reads go through the compactor, which serves
// Parquet partitions where they exist and falls back to raw JSONL otherwise;
// blob fetches go straight to the store.
type Server struct {
	store    *store.Store
	compact  *compact.Compactor
	coverage CoverageReporter
	log      *slog.Logger
}

// New builds a Server. coverage may be nil, in which case the health endpoint
// simply reports none.
func New(st *store.Store, c *compact.Compactor, coverage CoverageReporter, log *slog.Logger) *Server {
	if log == nil {
		log = slog.Default()
	}
	return &Server{store: st, compact: c, coverage: coverage, log: log}
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
	days, err := s.store.Days()
	if err != nil {
		s.fail(w, http.StatusInternalServerError, err)
		return
	}
	resp := map[string]any{"status": "ok", "days_captured": len(days)}
	if len(days) > 0 {
		resp["first_day"] = days[0].Format("2006-01-02")
		resp["last_day"] = days[len(days)-1].Format("2006-01-02")

		// Per-day volume, not just the span. A first and last date read as
		// continuous coverage; they are not, and the days with nothing in them
		// are the shape of how the agent was actually used.
		series, err := s.compact.DailyRange(days[0], days[len(days)-1])
		if err != nil {
			s.fail(w, http.StatusInternalServerError, err)
			return
		}
		resp["days"] = series
	}
	if s.coverage != nil {
		resp["coverage"] = s.coverage.Coverage()
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handleSessions(w http.ResponseWriter, r *http.Request) {
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
		page, err = s.compact.SessionsPage(from, to, limit, cursor, filter)
	} else {
		page, err = s.compact.SessionsRanked(from, to, limit, cursor, filter, order)
	}
	if err != nil {
		s.fail(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, page)
}

func (s *Server) handleSession(w http.ResponseWriter, r *http.Request) {
	from, to := s.window(r)
	id := r.PathValue("id")

	detail, ok, err := s.compact.SessionDetail(from, to, id)
	if err != nil {
		s.fail(w, http.StatusInternalServerError, err)
		return
	}
	if !ok {
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
	from, to := s.window(r)
	agg, err := s.compact.AggregateRange(from, to)
	if err != nil {
		s.fail(w, http.StatusInternalServerError, err)
		return
	}
	stats := statsFromAggregate(agg, from, to)
	if stats.ByDay, err = s.compact.DailyRange(from, to); err != nil {
		s.fail(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, stats)
}

func (s *Server) handleBlob(w http.ResponseWriter, r *http.Request) {
	payload, err := s.store.GetBlob(r.PathValue("ref"))
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
