package collect

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/Urenzu/trace-portal/internal/trace"
)

// Shipper sends locally captured events to a server.
//
// It ships from the local archive rather than from the tailer, which is what
// makes shipping failures cost nothing: the archive is written first and is
// authoritative, so a server that is down, slow, or mid-upgrade delays delivery
// and never loses a turn. It also means an engineer keeps a complete local copy
// of their own history, which is the honest version of "your data is yours".
//
// This is the half of the split that runs on a developer's machine. It never
// holds a password and never talks to the identity provider — it presents the
// collector token that `trace-portal login` obtained, and nothing else.
type Shipper struct {
	server string
	token  string
	client *http.Client
	log    *slog.Logger

	// Version is reported with each batch so a bad collector release can be
	// identified from the server side.
	Version string

	// BatchSize bounds one request. Smaller than the server's ceiling on
	// purpose: a collector that discovers the limit by being refused has
	// already spent the upload.
	BatchSize int
}

// NewShipper builds a Shipper. A nil client gets one with a timeout, because
// the default http.Client has none and a hung connection would stall shipping
// indefinitely.
func NewShipper(server, token string, client *http.Client, log *slog.Logger) (*Shipper, error) {
	if server == "" || token == "" {
		return nil, errors.New("shipping needs a server and a collector token")
	}
	if client == nil {
		client = &http.Client{Timeout: 60 * time.Second}
	}
	if log == nil {
		log = slog.Default()
	}
	return &Shipper{server: server, token: token, client: client, log: log, BatchSize: 2000}, nil
}

// Send ships one batch and reports what the server accepted.
//
// Events are sent with whatever identity they carry locally, and the server
// replaces it with what the credential authorises. That is not redundant: the
// local identity is what makes the archive on this machine attributable, and
// the server's is what makes the archive on that machine trustworthy. Neither
// can stand in for the other.
func (s *Shipper) Send(ctx context.Context, events []trace.Event) (Result, error) {
	if len(events) == 0 {
		return Result{}, nil
	}

	body, err := encodeBatch(Batch{Events: events, CollectorVersion: s.Version})
	if err != nil {
		return Result{}, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.server+"/api/collect", bytes.NewReader(body))
	if err != nil {
		return Result{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Content-Encoding", "gzip")
	req.Header.Set("Authorization", "Bearer "+s.token)

	resp, err := s.client.Do(req)
	if err != nil {
		return Result{}, fmt.Errorf("ship batch: %w", err)
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusOK:
		var res Result
		if err := json.NewDecoder(resp.Body).Decode(&res); err != nil {
			// The batch may well have landed. Reporting an error means it is
			// resent, and message-id keying makes the duplicate harmless —
			// which is why "resend on any doubt" is the correct policy here.
			return Result{}, fmt.Errorf("decode ingest response: %w", err)
		}
		return res, nil

	case http.StatusUnauthorized:
		drain(resp.Body)
		// Distinguished because it is the one failure retrying cannot fix: the
		// token was revoked or the account removed, and the person has to run
		// login again.
		return Result{}, ErrCredentialRejected

	case http.StatusRequestEntityTooLarge:
		drain(resp.Body)
		return Result{}, ErrBatchTooLarge

	default:
		drain(resp.Body)
		return Result{}, fmt.Errorf("server refused batch: %s", resp.Status)
	}
}

// Shipping errors a caller branches on.
var (
	// ErrCredentialRejected means retrying will not help; the person must sign
	// in again.
	ErrCredentialRejected = errors.New("collector credential rejected")

	// ErrBatchTooLarge means the same events may succeed in smaller groups.
	ErrBatchTooLarge = errors.New("batch too large")
)

// SendAll ships events in batches, halving on refusal, and returns the totals.
//
// Halving rather than failing: a batch is refused for its size, and the same
// events in two requests are the same data. Giving up would strand a backlog
// permanently, which for an archive whose source is pruned after a month means
// losing it.
func (s *Shipper) SendAll(ctx context.Context, events []trace.Event) (Result, error) {
	var total Result
	size := s.BatchSize
	if size <= 0 {
		size = 2000
	}

	for len(events) > 0 {
		if err := ctx.Err(); err != nil {
			return total, err
		}
		n := min(size, len(events))
		res, err := s.Send(ctx, events[:n])
		if errors.Is(err, ErrBatchTooLarge) && n > 1 {
			size = n / 2
			continue
		}
		if err != nil {
			return total, err
		}
		total.Accepted += res.Accepted
		total.Rejected += res.Rejected
		events = events[n:]
	}
	return total, nil
}

// encodeBatch marshals and gzips a batch.
//
// Gzip is worth the CPU here in a way it is not on the local hot path. These
// events are narrow JSON with enormously repetitive keys and dictionary-like
// values — the same session id, model and project on every row — so they
// compress to a fraction of their size, and the cost is paid on a developer's
// idle machine rather than on the request path of anything that matters.
func encodeBatch(b Batch) ([]byte, error) {
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if err := json.NewEncoder(zw).Encode(b); err != nil {
		return nil, fmt.Errorf("encode batch: %w", err)
	}
	if err := zw.Close(); err != nil {
		return nil, fmt.Errorf("compress batch: %w", err)
	}
	return buf.Bytes(), nil
}
