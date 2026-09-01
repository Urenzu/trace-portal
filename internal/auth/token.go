package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"strings"
	"sync"
	"time"
)

// base64URL is the encoding used for cookie payloads and bearer secrets: URL
// safe and unpadded, so a value survives a query string and a header untouched.
var base64URL = base64.RawURLEncoding

// NewID mints an opaque identifier with a short type prefix, e.g. u_3f9a…
//
// The prefix is for humans reading a log or a support ticket, not for parsing:
// nothing branches on it. 128 bits of randomness, so ids can be minted
// independently on any machine without coordination — which matters once
// tenants live in more than one database.
func NewID(prefix string) string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand failing means the process cannot generate a secret and
		// must not continue pretending it can.
		panic("auth: cannot read random bytes: " + err.Error())
	}
	return prefix + "_" + hex.EncodeToString(b[:])
}

// newSecret mints a URL-safe bearer secret. 256 bits: these are never rotated
// on a schedule, so they are sized to be safe if one lives for years in a
// config file.
func newSecret() string {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("auth: cannot read random bytes: " + err.Error())
	}
	return base64.RawURLEncoding.EncodeToString(b[:])
}

// hashSecret is how a bearer secret is stored.
//
// Only the digest is kept, so a leaked database yields nothing usable — the
// same reason password hashes exist. A fast hash is correct here and would be
// wrong for a password: these secrets are 256 random bits, so there is no
// dictionary to run and nothing for a slow KDF to defend against.
func hashSecret(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(sum[:])
}

// TokenKind separates the two credentials this system issues. They are not
// interchangeable: a browser session may read the UI, and a collector token may
// only ship events. A single credential type would mean a token sitting in a
// config file on a laptop could also read the whole archive.
type TokenKind string

const (
	// KindSession is a browser session, established by the authorization code
	// flow and carried in a cookie.
	KindSession TokenKind = "session"

	// KindCollector is a machine credential, established by the device flow and
	// carried in an Authorization header. Scoped to ingest.
	KindCollector TokenKind = "collector"
)

// Token is an issued credential. The secret itself is returned once, at
// issue, and never stored.
type Token struct {
	ID       string
	Kind     TokenKind
	TenantID string
	UserID   string

	// MachineID ties a collector token to one installation, so revoking a lost
	// laptop does not sign the owner out everywhere else.
	MachineID string

	// Label is what a person sees in a list of active sessions and machines.
	Label string

	IssuedAt  time.Time
	ExpiresAt time.Time
	LastUsed  time.Time
}

// Expired reports whether the token may no longer be used. A zero ExpiresAt
// means no expiry, which is deliberate for collector tokens: a background agent
// that silently stops shipping at 3am because a token lapsed produces a gap in
// an archive whose source is pruned after a month, and that gap is
// unrecoverable. Sessions always expire.
func (t Token) Expired(now time.Time) bool {
	return !t.ExpiresAt.IsZero() && now.After(t.ExpiresAt)
}

// TokenStore holds issued credentials by digest.
type TokenStore interface {
	Issue(ctx context.Context, t Token, secretHash string) error
	// Find resolves a presented secret. Implementations must look the digest up
	// directly rather than scanning and comparing, so lookup cost does not
	// depend on how many tokens exist.
	Find(ctx context.Context, secretHash string) (Token, bool, error)
	Revoke(ctx context.Context, id string) error
	Touch(ctx context.Context, id string, at time.Time) error
}

// ErrTokenRejected is returned for every failed presentation: unknown, revoked,
// expired, or the wrong kind. One error for all of them on purpose — telling a
// caller which of those it was tells an attacker which of their guesses landed.
var ErrTokenRejected = errors.New("token rejected")

// MemoryTokens is an in-memory TokenStore.
type MemoryTokens struct {
	mu     sync.Mutex
	byHash map[string]Token
	byID   map[string]string // id -> hash, so revocation does not scan
}

// NewMemoryTokens builds an empty token store.
func NewMemoryTokens() *MemoryTokens {
	return &MemoryTokens{byHash: map[string]Token{}, byID: map[string]string{}}
}

// Issue implements TokenStore.
func (m *MemoryTokens) Issue(_ context.Context, t Token, secretHash string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.byHash[secretHash] = t
	m.byID[t.ID] = secretHash
	return nil
}

// Find implements TokenStore.
func (m *MemoryTokens) Find(_ context.Context, secretHash string) (Token, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	t, ok := m.byHash[secretHash]
	return t, ok, nil
}

// Revoke implements TokenStore.
func (m *MemoryTokens) Revoke(_ context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if h, ok := m.byID[id]; ok {
		delete(m.byHash, h)
		delete(m.byID, id)
	}
	return nil
}

// Touch implements TokenStore.
func (m *MemoryTokens) Touch(_ context.Context, id string, at time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	h, ok := m.byID[id]
	if !ok {
		return nil
	}
	t := m.byHash[h]
	t.LastUsed = at
	m.byHash[h] = t
	return nil
}

// Issuer mints and checks the credentials both flows hand out.
type Issuer struct {
	store TokenStore
	now   func() time.Time

	// SessionTTL bounds a browser session. Collector tokens do not expire; see
	// Token.Expired for why.
	SessionTTL time.Duration
}

// NewIssuer builds an Issuer over store. A nil now uses the wall clock.
func NewIssuer(store TokenStore, now func() time.Time) *Issuer {
	if now == nil {
		now = time.Now
	}
	return &Issuer{store: store, now: now, SessionTTL: 14 * 24 * time.Hour}
}

// IssueSession mints a browser session credential, returning the secret once.
func (i *Issuer) IssueSession(ctx context.Context, acct Account) (Token, string, error) {
	now := i.now()
	t := Token{
		ID:        NewID("tok"),
		Kind:      KindSession,
		TenantID:  acct.TenantID,
		UserID:    acct.UserID,
		Label:     acct.Email,
		IssuedAt:  now,
		ExpiresAt: now.Add(i.SessionTTL),
	}
	return i.issue(ctx, t)
}

// IssueCollector mints a machine credential for one installation.
func (i *Issuer) IssueCollector(ctx context.Context, acct Account, machineID, label string) (Token, string, error) {
	if machineID == "" {
		return Token{}, "", errors.New("a collector token must name the machine it is for")
	}
	t := Token{
		ID:        NewID("tok"),
		Kind:      KindCollector,
		TenantID:  acct.TenantID,
		UserID:    acct.UserID,
		MachineID: machineID,
		Label:     label,
		IssuedAt:  i.now(),
	}
	return i.issue(ctx, t)
}

func (i *Issuer) issue(ctx context.Context, t Token) (Token, string, error) {
	secret := newSecret()
	if err := i.store.Issue(ctx, t, hashSecret(secret)); err != nil {
		return Token{}, "", err
	}
	return t, secret, nil
}

// Verify resolves a presented secret to its token, checking kind and expiry.
//
// Every rejection returns the same error. The kind check is not a formality: it
// is what stops a collector token, which lives in a file on a laptop, from
// being replayed at the UI to read the archive it only had permission to write
// to.
func (i *Issuer) Verify(ctx context.Context, secret string, want TokenKind) (Token, error) {
	secret = strings.TrimSpace(secret)
	if secret == "" {
		return Token{}, ErrTokenRejected
	}
	t, ok, err := i.store.Find(ctx, hashSecret(secret))
	if err != nil {
		return Token{}, err
	}
	if !ok {
		return Token{}, ErrTokenRejected
	}
	// Constant time on the kind comparison is pointless, but the value is
	// compared rather than trusted, and expiry is checked before the token is
	// handed back to anything that would act on it.
	if t.Kind != want || t.Expired(i.now()) {
		return Token{}, ErrTokenRejected
	}
	_ = i.store.Touch(ctx, t.ID, i.now())
	return t, nil
}

// Revoke invalidates one issued credential.
func (i *Issuer) Revoke(ctx context.Context, id string) error { return i.store.Revoke(ctx, id) }

// BearerToken pulls a credential out of an Authorization header.
func BearerToken(header string) string {
	const prefix = "Bearer "
	if len(header) <= len(prefix) || !strings.EqualFold(header[:len(prefix)], prefix) {
		return ""
	}
	return strings.TrimSpace(header[len(prefix):])
}

// constantTimeEqual compares two secrets without leaking their length
// relationship through timing. Used where a value is compared to a stored
// plaintext rather than to a digest — the device flow's user code, which is
// short enough that a digest would be guessable anyway.
func constantTimeEqual(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}
