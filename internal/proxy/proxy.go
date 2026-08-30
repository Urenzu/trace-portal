// Package proxy implements the transparent reverse proxy that sits between an
// instrumented app and the Anthropic API. It must be invisible: every failure
// in the capture path is logged and swallowed, never surfaced to the caller.
package proxy

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"time"

	"github.com/Urenzu/trace-portal/internal/store"
	"github.com/Urenzu/trace-portal/internal/trace"
)

// DefaultUpstream is the API the proxy forwards to.
const DefaultUpstream = "https://api.anthropic.com"

// maxCapture bounds how much of a body we hold in memory per exchange. Beyond
// it we keep the parsed summary and drop the blob rather than risk the proxy's
// footprint tracking the largest response it has ever seen.
const maxCapture = 32 << 20

// SessionHeader lets a client name its own session instead of relying on the
// conversation-prefix heuristic.
const SessionHeader = "X-Trace-Session"

// Config configures a Proxy.
type Config struct {
	Upstream string       // defaults to DefaultUpstream
	Store    *store.Store // required
	Logger   *slog.Logger // defaults to slog.Default()
}

// Proxy is an http.Handler that forwards to the Anthropic API while recording
// a trace event for each exchange.
type Proxy struct {
	rp     *httputil.ReverseProxy
	store  *store.Store
	log    *slog.Logger
	target *url.URL
}

// New builds a Proxy from cfg.
func New(cfg Config) (*Proxy, error) {
	if cfg.Store == nil {
		return nil, fmt.Errorf("proxy: Store is required")
	}
	upstream := cfg.Upstream
	if upstream == "" {
		upstream = DefaultUpstream
	}
	target, err := url.Parse(upstream)
	if err != nil {
		return nil, fmt.Errorf("proxy: parse upstream %q: %w", upstream, err)
	}
	log := cfg.Logger
	if log == nil {
		log = slog.Default()
	}

	p := &Proxy{store: cfg.Store, log: log, target: target}
	p.rp = &httputil.ReverseProxy{
		Rewrite: func(r *httputil.ProxyRequest) {
			r.SetURL(target)
			r.Out.Host = target.Host
			// The capture headers are ours; they must not reach the API.
			r.Out.Header.Del(SessionHeader)
		},
		// Stream SSE through immediately instead of batching flushes.
		FlushInterval:  -1,
		ModifyResponse: p.modifyResponse,
		ErrorHandler:   p.handleError,
	}
	return p, nil
}

// exchange carries per-request capture state between ServeHTTP, the response
// wrapper, and the error handler.
type exchange struct {
	event   trace.Event
	started time.Time
}

type ctxKey struct{}

func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	now := time.Now().UTC()
	ex := &exchange{
		started: now,
		event: trace.Event{
			Type:      trace.EventRequest,
			Timestamp: now,
			TurnID:    newID(),
			Method:    r.Method,
			Path:      r.URL.Path,
		},
	}

	// Only Messages API traffic is worth parsing; everything else is proxied
	// untouched so token counting, model listing, and files keep working.
	if r.Body != nil && isMessages(r.URL.Path) {
		body, err := readCapped(r.Body, maxCapture)
		r.Body.Close()
		if err != nil {
			p.log.Warn("read request body", "err", err)
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
		r.ContentLength = int64(len(body))

		decoded, _ := decodeBody(body, r.Header.Get("Content-Encoding"))
		summarizeRequest(decoded, &ex.event)
		ex.event.SessionID = p.resolveSession(r, decoded)
		ex.event.RequestBlob = p.putBlob(decoded)

		if err := p.store.Append(ex.event); err != nil {
			p.log.Warn("append request event", "err", err)
		}
	}

	p.rp.ServeHTTP(w, r.WithContext(withExchange(r.Context(), ex)))
}

func (p *Proxy) resolveSession(r *http.Request, body []byte) string {
	if s := r.Header.Get(SessionHeader); s != "" {
		return s
	}
	return sessionKey(body)
}

// modifyResponse swaps the upstream body for one that tees into a buffer and
// finalizes the response event once the client has drained the stream.
func (p *Proxy) modifyResponse(resp *http.Response) error {
	ex, ok := exchangeFrom(resp.Request.Context())
	if !ok || ex.event.SessionID == "" {
		return nil // not a Messages call; nothing to record
	}

	ev := ex.event
	ev.Type = trace.EventResponse
	ev.StatusCode = resp.StatusCode
	ev.TTFBMS = time.Since(ex.started).Milliseconds()
	ev.RequestBlob = ex.event.RequestBlob
	sse := isSSE(resp.Header.Get("Content-Type"))
	encoding := resp.Header.Get("Content-Encoding")

	resp.Body = &captureReader{
		inner: resp.Body,
		onEOF: func(captured []byte, truncated bool) {
			ev.Timestamp = time.Now().UTC()
			ev.DurationMS = time.Since(ex.started).Milliseconds()

			// The client gets the original bytes; only the copy kept for
			// analysis is decoded.
			decoded, decodedOK := decodeBody(captured, encoding)
			if !decodedOK && encoding != "" {
				p.log.Debug("response body left encoded", "encoding", encoding)
			}

			if sse {
				summarizeSSE(decoded, &ev)
			} else {
				summarizeResponse(decoded, &ev)
			}
			// Store the readable form so the payload viewer shows JSON rather
			// than a compressed blob.
			if !truncated {
				ev.ResponseBlob = p.putBlob(decoded)
			}
			if err := p.store.Append(ev); err != nil {
				p.log.Warn("append response event", "err", err)
			}
		},
	}
	return nil
}

// handleError records upstream transport failures and reports a 502 upstream,
// matching what ReverseProxy would have done on its own.
func (p *Proxy) handleError(w http.ResponseWriter, r *http.Request, err error) {
	if ex, ok := exchangeFrom(r.Context()); ok && ex.event.SessionID != "" {
		ev := ex.event
		ev.Type = trace.EventError
		ev.Timestamp = time.Now().UTC()
		ev.DurationMS = time.Since(ex.started).Milliseconds()
		ev.Error = err.Error()
		if appendErr := p.store.Append(ev); appendErr != nil {
			p.log.Warn("append error event", "err", appendErr)
		}
	}
	p.log.Error("upstream request failed", "path", r.URL.Path, "err", err)
	w.WriteHeader(http.StatusBadGateway)
}

func (p *Proxy) putBlob(payload []byte) string {
	if len(payload) == 0 {
		return ""
	}
	ref, err := p.store.PutBlob(payload)
	if err != nil {
		p.log.Warn("store blob", "err", err)
		return ""
	}
	return ref
}

// captureReader tees everything read through it into a buffer and invokes onEOF
// exactly once, whether the consumer reads to EOF or gives up and closes early.
type captureReader struct {
	inner     io.ReadCloser
	buf       bytes.Buffer
	truncated bool
	done      bool
	onEOF     func(captured []byte, truncated bool)
}

func (c *captureReader) Read(p []byte) (int, error) {
	n, err := c.inner.Read(p)
	if n > 0 {
		if c.buf.Len()+n <= maxCapture {
			c.buf.Write(p[:n])
		} else {
			c.truncated = true
		}
	}
	if err != nil {
		c.finish()
	}
	return n, err
}

func (c *captureReader) Close() error {
	c.finish()
	return c.inner.Close()
}

func (c *captureReader) finish() {
	if c.done {
		return
	}
	c.done = true
	c.onEOF(c.buf.Bytes(), c.truncated)
}

func isMessages(path string) bool {
	return strings.HasSuffix(path, "/messages")
}

func readCapped(r io.Reader, limit int64) ([]byte, error) {
	var buf bytes.Buffer
	_, err := io.Copy(&buf, io.LimitReader(r, limit))
	return buf.Bytes(), err
}

func newID() string {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b[:])
}
