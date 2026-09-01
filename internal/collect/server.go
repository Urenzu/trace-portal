// Package collect receives event batches from collectors.
//
// This is the seam the cloud product is built on: everything that needs a
// developer's local disk — tailing transcripts, resolving a project by walking
// up for .git, reducing paths to a name plus a digest — stays on their machine,
// and what crosses the wire is already-narrow measurements. That is also the
// entire privacy story, so it is worth stating plainly: the redaction happens
// before the request, not after it.
//
// One rule governs this package, and it is the reason the code is shaped the
// way it is: identity comes from the credential, never from the payload.
package collect

import (
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/Urenzu/trace-portal/internal/auth"
	"github.com/Urenzu/trace-portal/internal/tenant"
	"github.com/Urenzu/trace-portal/internal/trace"
)

// Limits on one request. A collector batches, so these are sized for a laptop
// that has been offline for a while catching up, not for a single turn.
const (
	// MaxBatchBytes bounds the compressed-on-the-wire body after decompression.
	MaxBatchBytes = 8 << 20 // 8 MiB

	// MaxBatchEvents bounds the count independently of the size, because a
	// batch of many tiny events costs per-event work rather than bytes.
	MaxBatchEvents = 20000
)

// Authenticator resolves a request's collector credential. It is an interface
// so this package does not depend on how sign-in is configured — and so a test
// can hand it a fixed answer.
type Authenticator interface {
	Collector(r *http.Request) (auth.Token, error)
}

// Server accepts batches and writes them into the right tenant's store.
type Server struct {
	registry *tenant.Registry
	auth     Authenticator
	log      *slog.Logger
	now      func() time.Time
}

// NewServer builds the ingest endpoint.
func NewServer(registry *tenant.Registry, a Authenticator, log *slog.Logger, now func() time.Time) (*Server, error) {
	if registry == nil || a == nil {
		return nil, errors.New("collect needs a registry and an authenticator")
	}
	if log == nil {
		log = slog.Default()
	}
	if now == nil {
		now = time.Now
	}
	return &Server{registry: registry, auth: a, log: log, now: now}, nil
}

// Routes registers the ingest endpoint.
func (s *Server) Routes(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/collect", s.handleCollect)
}

// Batch is what a collector sends.
type Batch struct {
	// Events are the measurements. Any identity they carry is ignored; see
	// handleCollect.
	Events []trace.Event `json:"events"`

	// CollectorVersion is recorded in logs so a bad release can be identified
	// from the server side rather than by asking users what they are running.
	CollectorVersion string `json:"collector_version,omitempty"`
}

// Result is what the collector gets back.
type Result struct {
	// Accepted is how many events were written. A collector advances its
	// checkpoint on this, so it must count only what is durable.
	Accepted int `json:"accepted"`

	// Rejected counts events dropped for being unusable — no timestamp, or a
	// timestamp so far outside plausible range that storing it would create a
	// partition for the year 1478. Reported rather than silently dropped,
	// because a collector that is producing garbage should be able to tell.
	Rejected int `json:"rejected"`
}

func (s *Server) handleCollect(w http.ResponseWriter, r *http.Request) {
	tok, err := s.auth.Collector(r)
	if err != nil {
		// One response for every authentication failure. Distinguishing
		// "unknown token" from "revoked token" tells a caller which of their
		// guesses landed.
		w.Header().Set("WWW-Authenticate", `Bearer realm="trace-portal"`)
		http.Error(w, "collector credential rejected", http.StatusUnauthorized)
		return
	}

	reader, err := batchReader(w, r)
	if err != nil {
		http.Error(w, "could not read batch", http.StatusBadRequest)
		return
	}
	defer reader.Close()

	var batch Batch
	if err := json.NewDecoder(reader).Decode(&batch); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) || errors.Is(err, errBatchTooLarge) {
			// 413 rather than 400: a collector that gets this should halve its
			// batch and retry, which is a different response from "your data is
			// malformed and retrying will not help".
			http.Error(w, "batch too large", http.StatusRequestEntityTooLarge)
			return
		}
		http.Error(w, "could not read batch", http.StatusBadRequest)
		return
	}
	if len(batch.Events) > MaxBatchEvents {
		http.Error(w, fmt.Sprintf("batch holds %d events; the limit is %d", len(batch.Events), MaxBatchEvents),
			http.StatusRequestEntityTooLarge)
		return
	}

	// The tenant is resolved from the token and from nothing else. This call is
	// the isolation boundary: the storage it returns is rooted at a path derived
	// from an authenticated id, so nothing downstream needs a tenant predicate
	// and nothing downstream can omit one.
	storage, err := s.registry.For(tok.TenantID)
	if err != nil {
		s.log.Error("could not open tenant storage", "tenant", tok.TenantID, "err", err)
		http.Error(w, "could not accept batch", http.StatusInternalServerError)
		return
	}

	// Identity is overwritten, not merged.
	//
	// This is the single most important line in the package. A collector's
	// stamped identity is advisory: it is written by software on a machine we do
	// not control, and a modified one could claim any tenant it liked. The
	// credential is the only authority, so whatever the payload said is
	// discarded and replaced. Merging here — filling only what was absent —
	// would mean a body that supplied a tenant_id kept it, which is precisely
	// the bug.
	stamp := trace.Identity{
		TenantID:  tok.TenantID,
		UserID:    tok.UserID,
		MachineID: tok.MachineID,
	}

	var res Result
	for _, ev := range batch.Events {
		if !plausible(ev, s.now()) {
			res.Rejected++
			continue
		}
		ev.Identity = stamp
		if err := storage.Store.Append(r.Context(), ev); err != nil {
			s.log.Error("could not append a collected event", "tenant", tok.TenantID, "err", err)
			// Report what was durable so far. The collector will resend the
			// rest, and message-id keying makes the overlap harmless — turns
			// observed twice collapse into one, which is the property that lets
			// a retry be the whole recovery story.
			writeJSON(w, http.StatusInternalServerError, res)
			return
		}
		res.Accepted++
	}

	if res.Rejected > 0 {
		s.log.Warn("dropped implausible events from a batch",
			"tenant", tok.TenantID, "machine", tok.MachineID,
			"rejected", res.Rejected, "accepted", res.Accepted,
			"collector", batch.CollectorVersion)
	}
	writeJSON(w, http.StatusOK, res)
}

var errBatchTooLarge = errors.New("batch exceeds the decompressed limit")

// batchReader gives back the request body, decompressed if the collector
// compressed it, and bounded either way.
//
// The bound is applied twice on purpose. MaxBytesReader caps what arrives on
// the wire; the limit inside the gzip reader caps what it expands to. Capping
// only the wire would accept a few kilobytes of zeros that decompress to
// gigabytes — the whole point of a compression bomb is that the compressed size
// tells you nothing about the cost of reading it.
func batchReader(w http.ResponseWriter, r *http.Request) (io.ReadCloser, error) {
	wire := http.MaxBytesReader(w, r.Body, MaxBatchBytes)
	if !strings.EqualFold(r.Header.Get("Content-Encoding"), "gzip") {
		return wire, nil
	}

	zr, err := gzip.NewReader(wire)
	if err != nil {
		wire.Close()
		return nil, err
	}
	return &boundedReader{
		Reader:  io.LimitReader(zr, MaxBatchBytes+1),
		closers: []io.Closer{zr, wire},
		limit:   MaxBatchBytes,
	}, nil
}

// boundedReader turns the limit into an error rather than a silent truncation.
// A truncated JSON body would fail to decode, which reports as malformed input
// and sends a collector into a retry loop over a batch that can never land.
type boundedReader struct {
	io.Reader
	closers []io.Closer
	limit   int64
	read    int64
}

func (b *boundedReader) Read(p []byte) (int, error) {
	n, err := b.Reader.Read(p)
	b.read += int64(n)
	if b.read > b.limit {
		return n, errBatchTooLarge
	}
	return n, err
}

func (b *boundedReader) Close() error {
	var first error
	for _, c := range b.closers {
		if err := c.Close(); err != nil && first == nil {
			first = err
		}
	}
	return first
}

// plausibleWindow bounds how far a timestamp may sit from now.
//
// Not a validation formality. Every read path walks the window a day at a time,
// so one event dated 1478 makes every subsequent query walk five centuries of
// empty days — the same shape as the unbounded-window defect this repository
// already fixed from the query side. Fixing it there and not here would leave
// the identical denial of service reachable by writing rather than reading.
//
// The future bound is tighter than the past one because a clock ahead by an
// hour is a misconfigured machine, whereas a laptop that was closed for a month
// has genuinely old data worth keeping.
const (
	plausiblePast   = 5 * 365 * 24 * time.Hour
	plausibleFuture = 24 * time.Hour
)

func plausible(ev trace.Event, now time.Time) bool {
	if ev.Timestamp.IsZero() {
		return false
	}
	if ev.Timestamp.Before(now.Add(-plausiblePast)) || ev.Timestamp.After(now.Add(plausibleFuture)) {
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// drain is used by tests and by the client; it keeps a response body from
// leaking a connection when the body is not otherwise read.
func drain(rc io.ReadCloser) {
	_, _ = io.Copy(io.Discard, io.LimitReader(rc, 4<<10))
	rc.Close()
}
