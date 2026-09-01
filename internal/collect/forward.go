package collect

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/Urenzu/trace-portal/internal/eventstore"
	"github.com/Urenzu/trace-portal/internal/trace"
)

// Forwarder ships the local archive to a server, in the background.
//
// It reads from the store rather than from the tailer, which is what makes a
// shipping failure cost nothing: the local archive is written first and is
// authoritative, so a server that is down, slow or mid-upgrade delays delivery
// and never loses a turn. The engineer also keeps a complete copy of their own
// history, which is the honest version of "your data is yours".
type Forwarder struct {
	store   eventstore.Store
	shipper *Shipper
	log     *slog.Logger
	path    string

	// Overlap is how far back before the checkpoint each pass re-reads.
	//
	// Re-reading is necessary because events do not arrive in timestamp order:
	// a backfill pass writes old records after new ones, so a checkpoint that
	// only ever moved forward in time would step over them permanently. A
	// timestamp is a bookmark in the data, not in the file.
	//
	// What is re-read is not re-sent — see the record of already-shipped keys
	// below. If it were, a quiet collector would re-upload the same hour on
	// every pass forever, which is bandwidth spent to discover nothing.
	Overlap time.Duration
}

// forwardCheckpoint records how far shipping has got. It sits beside the ingest
// checkpoint and has the same job: survive a restart without re-sending months.
const forwardCheckpoint = "forward.json"

type forwardState struct {
	// Shipped is the timestamp of the newest event known to be durable on the
	// server. Advanced only on a successful response, so a crash mid-batch
	// re-sends rather than skips.
	Shipped time.Time `json:"shipped_through"`

	// Server is recorded so that pointing a collector at a different server
	// resets the checkpoint rather than silently shipping only new turns to it.
	Server string `json:"server"`

	// RecentKeys identifies the events already shipped from inside the overlap
	// window, so re-reading that window does not mean re-sending it.
	//
	// Bounded by how many events fall in one overlap rather than by how long
	// the collector has run: everything older than the window is excluded by
	// the timestamp, and everything inside it is listed here. An hour of heavy
	// use is a few hundred short strings.
	RecentKeys []string `json:"recent_keys,omitempty"`
}

// NewForwarder builds a Forwarder over a local store.
func NewForwarder(st eventstore.Store, shipper *Shipper, dataDir string, log *slog.Logger) (*Forwarder, error) {
	if st == nil || shipper == nil {
		return nil, errors.New("forwarding needs a store and a shipper")
	}
	if log == nil {
		log = slog.Default()
	}
	return &Forwarder{
		store:   st,
		shipper: shipper,
		log:     log,
		path:    filepath.Join(dataDir, forwardCheckpoint),
		Overlap: time.Hour,
	}, nil
}

// Run ships once, then on a ticker until the context is cancelled.
func (f *Forwarder) Run(ctx context.Context, every time.Duration) error {
	if n, err := f.Pass(ctx); err != nil {
		f.log.Warn("first shipping pass failed", "err", err)
	} else if n > 0 {
		f.log.Info("shipped backlog", "events", n, "server", f.shipper.server)
	}
	if every <= 0 {
		return nil
	}

	ticker := time.NewTicker(every)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			n, err := f.Pass(ctx)
			switch {
			case errors.Is(err, ErrCredentialRejected):
				// The one failure retrying cannot fix. Stop rather than
				// hammering the endpoint with a credential that will never be
				// accepted; capture continues locally either way.
				f.log.Error("collector credential rejected; stopping shipping until you sign in again",
					"hint", "trace-portal login -server "+f.shipper.server)
				return nil
			case err != nil:
				f.log.Warn("shipping pass failed; will retry", "err", err)
			case n > 0:
				f.log.Debug("shipped events", "events", n)
			}
		}
	}
}

// Pass ships everything captured since the checkpoint, and reports how many
// events the server accepted.
func (f *Forwarder) Pass(ctx context.Context) (int, error) {
	state, err := f.load()
	if err != nil {
		return 0, err
	}
	if state.Server != "" && state.Server != f.shipper.server {
		// A different server has none of this archive. Starting from the
		// checkpoint would ship only recent turns and leave a permanent hole in
		// the history it holds, so the checkpoint is reset instead.
		f.log.Info("server changed; shipping the whole archive", "from", state.Server, "to", f.shipper.server)
		// Both halves of the bookmark, not just the timestamp. The key set says
		// "already sent", and it is only true of the server it was recorded
		// against — leaving it would filter out every event whose timestamp the
		// reset had just made eligible again.
		state.Shipped, state.RecentKeys = time.Time{}, nil
	}

	from := state.Shipped.Add(-f.Overlap)
	if state.Shipped.IsZero() {
		days, err := f.store.Days(ctx)
		if err != nil {
			return 0, err
		}
		if len(days) == 0 {
			return 0, nil
		}
		from = days[0]
	}
	to := time.Now().UTC()

	events, err := f.store.EventsRange(ctx, from, to)
	if err != nil {
		return 0, err
	}
	if len(events) == 0 {
		return 0, nil
	}

	already := make(map[string]bool, len(state.RecentKeys))
	for _, k := range state.RecentKeys {
		already[k] = true
	}

	pending := events[:0:0]
	for _, ev := range events {
		if !already[forwardKey(ev)] {
			pending = append(pending, ev)
		}
	}
	if len(pending) == 0 {
		return 0, nil
	}

	res, err := f.shipper.SendAll(ctx, pending)
	if err != nil {
		// Whatever was accepted before the failure stays unrecorded, so the
		// next pass sends it again. Message-id keying makes that harmless, and
		// the alternative — recording a partial success — risks recording more
		// than actually landed.
		return res.Accepted, err
	}

	newest := newestTimestamp(events)
	if newest.After(state.Shipped) {
		state.Shipped = newest
	}
	state.RecentKeys = keysWithin(events, state.Shipped.Add(-f.Overlap))
	state.Server = f.shipper.server
	if err := f.save(state); err != nil {
		// The data is durable on the server; only the bookmark failed. Say so
		// and carry on — the next pass re-ships an overlap, which deduplicates.
		f.log.Warn("could not save the shipping checkpoint", "err", err)
	}
	return res.Accepted, nil
}

// forwardKey identifies one record for the purpose of "have I sent this".
//
// It is not the turn key the query layer uses. That one deliberately collapses
// a request and its response into a single turn, which is right for reading and
// wrong here: both records have to reach the server, and treating them as one
// would ship the request and never the response.
func forwardKey(ev trace.Event) string {
	return string(ev.Type) + "|" + ev.TurnID + "|" + ev.MessageID + "|" +
		strconv.FormatInt(ev.Timestamp.UnixNano(), 36)
}

// keysWithin lists the shipped records still inside the overlap window, which
// is exactly the set the next pass will re-read and must not re-send.
func keysWithin(events []trace.Event, cutoff time.Time) []string {
	keys := make([]string, 0, len(events))
	for _, ev := range events {
		if ev.Timestamp.Before(cutoff) {
			continue
		}
		keys = append(keys, forwardKey(ev))
	}
	return keys
}

func newestTimestamp(events []trace.Event) time.Time {
	var newest time.Time
	for _, ev := range events {
		if ev.Timestamp.After(newest) {
			newest = ev.Timestamp
		}
	}
	return newest
}

func (f *Forwarder) load() (forwardState, error) {
	raw, err := os.ReadFile(f.path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return forwardState{}, nil
		}
		return forwardState{}, fmt.Errorf("read %s: %w", forwardCheckpoint, err)
	}
	var s forwardState
	if err := json.Unmarshal(raw, &s); err != nil {
		// A corrupt bookmark is recoverable: re-ship everything and let
		// message-id keying absorb the duplicates. Failing instead would stop
		// shipping over a file that carries no data of its own.
		f.log.Warn("shipping checkpoint unreadable; re-shipping the archive", "err", err)
		return forwardState{}, nil
	}
	return s, nil
}

func (f *Forwarder) save(s forwardState) error {
	raw, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	tmp := f.path + ".tmp"
	if err := os.WriteFile(tmp, append(raw, '\n'), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, f.path)
}
