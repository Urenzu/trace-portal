package auth

import (
	"context"
	"crypto/rand"
	"errors"
	"math/big"
	"strings"
	"sync"
	"time"
)

// The device authorization flow, RFC 8628, served by this system rather than by
// the identity provider.
//
// What a person sees:
//
//	$ trace-portal login
//	Open https://app.example.com/device and enter code  FKDL-8HZQ
//	Waiting…
//	Signed in as dana@example.com. Collector enrolled.
//
// What happens underneath: the CLI asks this server for a code pair, then polls.
// The person opens the URL in a browser they are already signed into — or signs
// in there through the ordinary OIDC browser flow — types the short code, and
// approves. The approval binds the pending request to their account, and the
// next poll returns a collector token.
//
// The CLI never speaks to the identity provider, never opens a listening socket
// on localhost, and never handles a password. That matters beyond tidiness: a
// loopback redirect fails over SSH, which is exactly where a collector is often
// installed, and depending on the provider's own device endpoint would tie the
// CLI to the minority of providers that implement one.

// Device flow errors, named to match the poll responses RFC 8628 defines. The
// CLI branches on them, so they are part of the contract rather than incidental
// text.
var (
	// ErrAuthorizationPending means the person has not approved yet. The CLI
	// keeps polling.
	ErrAuthorizationPending = errors.New("authorization_pending")

	// ErrSlowDown means the CLI polled faster than the interval it was given.
	// It must back off rather than treat this as a failure.
	ErrSlowDown = errors.New("slow_down")

	// ErrExpiredToken means the code pair timed out. The CLI starts over.
	ErrExpiredToken = errors.New("expired_token")

	// ErrAccessDenied means the person refused. The CLI stops.
	ErrAccessDenied = errors.New("access_denied")

	// ErrBadDeviceCode covers an unknown or already-redeemed device code.
	ErrBadDeviceCode = errors.New("invalid_grant")
)

// userCodeAlphabet excludes every character pair a person reads wrong off a
// terminal and types into a browser: no O or 0, no I or 1 or L, no U against V.
// The flow's whole purpose is that a human retypes this by hand, so an
// ambiguous glyph is a support ticket, not a cosmetic issue.
const userCodeAlphabet = "ABCDEFGHJKMNPQRSTWXYZ23456789"

// A user code is short because it is retyped. That makes it guessable in a way
// the device code is not, so three things bound the exposure: it lives for a few
// minutes, it is single-use, and approval requires an authenticated browser
// session — a guessed code cannot be approved by the guesser, only by the person
// who is already signed in and is looking at what they are approving.
const (
	userCodeLength     = 8 // two groups of four
	deviceCodeLifetime = 10 * time.Minute
	devicePollInterval = 5 * time.Second
)

// DeviceRequest is one pending CLI sign-in.
type DeviceRequest struct {
	// DeviceCode is the CLI's secret. Long and random: it is never retyped, so
	// it has no reason to be short.
	DeviceCode string

	// UserCode is what the person types, formatted with a separator.
	UserCode string

	// MachineID is supplied by the CLI at the start, so the token issued at the
	// end is bound to the installation that asked for it. It also lets the
	// approval page say which machine is being enrolled — approving a code
	// without knowing what it enrols is approving nothing meaningful.
	MachineID string

	// MachineLabel is a human hint shown on the approval page, such as a
	// hostname the person recognises. Untrusted: it is typed by whatever ran
	// the CLI and is only ever displayed, never matched against.
	MachineLabel string

	ExpiresAt time.Time
	Interval  time.Duration

	// Approved and Denied are set by the browser side. Both start false, which
	// is the pending state the CLI polls against.
	Approved bool
	Denied   bool

	// Account is filled in at approval, from the browser session. It is
	// deliberately not supplied by the CLI: the CLI is unauthenticated at this
	// point and must not be able to name the account it wants to join.
	Account Account

	// LastPolled enforces the interval.
	LastPolled time.Time

	// Redeemed marks a device code whose token has already been collected, so a
	// replayed poll cannot mint a second credential.
	Redeemed bool
}

// Pending reports the state the CLI should keep waiting on.
func (r DeviceRequest) Pending() bool { return !r.Approved && !r.Denied }

// DeviceStore holds pending device requests.
//
// An interface for the same reason the directory is: this is memory today and a
// table tomorrow. Note that a memory implementation is not merely a convenience
// here — it also means pending sign-ins do not survive a restart, which is
// correct behaviour rather than a limitation. A restart mid-flow should make the
// person run the command again, not silently approve later.
type DeviceStore interface {
	Create(ctx context.Context, r DeviceRequest) error
	ByDeviceCode(ctx context.Context, code string) (DeviceRequest, bool, error)
	ByUserCode(ctx context.Context, code string) (DeviceRequest, bool, error)
	Update(ctx context.Context, r DeviceRequest) error
	Delete(ctx context.Context, deviceCode string) error
}

// MemoryDevices is an in-memory DeviceStore.
type MemoryDevices struct {
	mu       sync.Mutex
	byDevice map[string]DeviceRequest
	byUser   map[string]string // user code -> device code
}

// NewMemoryDevices builds an empty device store.
func NewMemoryDevices() *MemoryDevices {
	return &MemoryDevices{byDevice: map[string]DeviceRequest{}, byUser: map[string]string{}}
}

// Create implements DeviceStore.
func (m *MemoryDevices) Create(_ context.Context, r DeviceRequest) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, clash := m.byUser[r.UserCode]; clash {
		// Two live requests sharing a user code would make the short code
		// ambiguous, and approving one would approve an arbitrary one of them.
		return errors.New("user code collision")
	}
	m.byDevice[r.DeviceCode] = r
	m.byUser[r.UserCode] = r.DeviceCode
	return nil
}

// ByDeviceCode implements DeviceStore.
func (m *MemoryDevices) ByDeviceCode(_ context.Context, code string) (DeviceRequest, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.byDevice[code]
	return r, ok, nil
}

// ByUserCode implements DeviceStore.
func (m *MemoryDevices) ByUserCode(_ context.Context, code string) (DeviceRequest, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	device, ok := m.byUser[code]
	if !ok {
		return DeviceRequest{}, false, nil
	}
	r, ok := m.byDevice[device]
	return r, ok, nil
}

// Update implements DeviceStore.
func (m *MemoryDevices) Update(_ context.Context, r DeviceRequest) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.byDevice[r.DeviceCode]; !ok {
		return ErrBadDeviceCode
	}
	m.byDevice[r.DeviceCode] = r
	return nil
}

// Delete implements DeviceStore.
func (m *MemoryDevices) Delete(_ context.Context, deviceCode string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if r, ok := m.byDevice[deviceCode]; ok {
		delete(m.byUser, r.UserCode)
		delete(m.byDevice, deviceCode)
	}
	return nil
}

// DeviceFlow runs the CLI half of sign-in.
type DeviceFlow struct {
	store  DeviceStore
	issuer *Issuer
	now    func() time.Time

	// VerificationURI is where the person is told to go. It is absolute because
	// it is printed into a terminal and pasted into a browser that may be on
	// another machine entirely.
	VerificationURI string

	Lifetime time.Duration
	Interval time.Duration
}

// NewDeviceFlow builds a DeviceFlow. verificationURI is the browser page that
// accepts a user code.
func NewDeviceFlow(store DeviceStore, issuer *Issuer, verificationURI string, now func() time.Time) *DeviceFlow {
	if now == nil {
		now = time.Now
	}
	return &DeviceFlow{
		store:           store,
		issuer:          issuer,
		now:             now,
		VerificationURI: strings.TrimRight(verificationURI, "/"),
		Lifetime:        deviceCodeLifetime,
		Interval:        devicePollInterval,
	}
}

// Start creates a pending request for one machine.
func (f *DeviceFlow) Start(ctx context.Context, machineID, machineLabel string) (DeviceRequest, error) {
	if machineID == "" {
		return DeviceRequest{}, errors.New("device authorization needs a machine id")
	}
	now := f.now()

	// Retry on the vanishingly rare user-code collision rather than failing a
	// sign-in for it.
	for attempt := 0; attempt < 5; attempt++ {
		code, err := newUserCode()
		if err != nil {
			return DeviceRequest{}, err
		}
		r := DeviceRequest{
			DeviceCode:   newSecret(),
			UserCode:     code,
			MachineID:    machineID,
			MachineLabel: machineLabel,
			ExpiresAt:    now.Add(f.Lifetime),
			Interval:     f.Interval,
		}
		if err := f.store.Create(ctx, r); err == nil {
			return r, nil
		}
	}
	return DeviceRequest{}, errors.New("could not allocate a user code")
}

// Approve binds a pending request to the account of the person approving it.
//
// The account comes from the caller's authenticated browser session, never from
// the request itself. That is the property that makes a short, guessable user
// code safe: guessing one lets an attacker learn that a request exists, and
// nothing else — approving it still requires being signed in as somebody, and
// what gets enrolled is that somebody's own machine.
func (f *DeviceFlow) Approve(ctx context.Context, userCode string, acct Account) error {
	r, err := f.pendingByUserCode(ctx, userCode)
	if err != nil {
		return err
	}
	r.Approved, r.Account = true, acct
	return f.store.Update(ctx, r)
}

// Deny records a refusal, so the CLI stops immediately rather than waiting out
// the expiry.
func (f *DeviceFlow) Deny(ctx context.Context, userCode string) error {
	r, err := f.pendingByUserCode(ctx, userCode)
	if err != nil {
		return err
	}
	r.Denied = true
	return f.store.Update(ctx, r)
}

// Lookup returns a pending request for display on the approval page, so the
// person can see which machine they are about to enrol.
func (f *DeviceFlow) Lookup(ctx context.Context, userCode string) (DeviceRequest, error) {
	return f.pendingByUserCode(ctx, userCode)
}

func (f *DeviceFlow) pendingByUserCode(ctx context.Context, userCode string) (DeviceRequest, error) {
	r, ok, err := f.store.ByUserCode(ctx, NormalizeUserCode(userCode))
	if err != nil {
		return DeviceRequest{}, err
	}
	if !ok {
		return DeviceRequest{}, ErrBadDeviceCode
	}
	if f.now().After(r.ExpiresAt) {
		return DeviceRequest{}, ErrExpiredToken
	}
	if !r.Pending() {
		return DeviceRequest{}, ErrBadDeviceCode
	}
	return r, nil
}

// Poll is the CLI's side. It returns the collector token once, on the first
// poll after approval.
func (f *DeviceFlow) Poll(ctx context.Context, deviceCode string) (Token, string, error) {
	r, ok, err := f.store.ByDeviceCode(ctx, deviceCode)
	if err != nil {
		return Token{}, "", err
	}
	if !ok || r.Redeemed {
		return Token{}, "", ErrBadDeviceCode
	}

	now := f.now()
	if now.After(r.ExpiresAt) {
		return Token{}, "", ErrExpiredToken
	}

	// Rate limiting, per RFC 8628. A client polling in a tight loop is told to
	// slow down rather than served, and the timestamp is recorded even on the
	// rejected poll so a hot loop cannot escape the interval by ignoring it.
	if !r.LastPolled.IsZero() && now.Sub(r.LastPolled) < r.Interval {
		r.LastPolled = now
		_ = f.store.Update(ctx, r)
		return Token{}, "", ErrSlowDown
	}
	r.LastPolled = now

	switch {
	case r.Denied:
		_ = f.store.Delete(ctx, deviceCode)
		return Token{}, "", ErrAccessDenied
	case !r.Approved:
		_ = f.store.Update(ctx, r)
		return Token{}, "", ErrAuthorizationPending
	}

	// Approved. Mark redeemed before minting, so a concurrent poll cannot
	// collect a second token for one approval.
	r.Redeemed = true
	if err := f.store.Update(ctx, r); err != nil {
		return Token{}, "", err
	}

	label := r.MachineLabel
	if label == "" {
		label = r.MachineID
	}
	tok, secret, err := f.issuer.IssueCollector(ctx, r.Account, r.MachineID, label)
	if err != nil {
		return Token{}, "", err
	}
	_ = f.store.Delete(ctx, deviceCode)
	return tok, secret, nil
}

// newUserCode mints a code in the form ABCD-EFGH.
func newUserCode() (string, error) {
	limit := big.NewInt(int64(len(userCodeAlphabet)))
	out := make([]byte, 0, userCodeLength+1)
	for i := 0; i < userCodeLength; i++ {
		if i == userCodeLength/2 {
			out = append(out, '-')
		}
		n, err := rand.Int(rand.Reader, limit)
		if err != nil {
			return "", err
		}
		out = append(out, userCodeAlphabet[n.Int64()])
	}
	return string(out), nil
}

// NormalizeUserCode makes what a person typed match what was minted.
//
// People type lower case, omit the dash, or paste it with spaces around it.
// Rejecting any of those would be rejecting a correct code for a formatting
// reason, on the one screen in the product where the person is already doing
// something tedious by hand.
func NormalizeUserCode(s string) string {
	var b strings.Builder
	for _, r := range strings.ToUpper(strings.TrimSpace(s)) {
		if r == '-' || r == ' ' {
			continue
		}
		b.WriteRune(r)
	}
	code := b.String()
	if len(code) != userCodeLength {
		return code
	}
	return code[:userCodeLength/2] + "-" + code[userCodeLength/2:]
}
