package auth

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"
)

// Account is what a successful sign-in resolves to: ids minted by this system,
// never the provider's.
//
// This indirection is the whole reason the archive can outlive a vendor choice.
// A turn is stamped with a UserID, and a UserID is a row in this directory that
// happens to be linked to a provider subject. Change providers and the link
// changes; the turns do not. Write the provider's subject onto the turn instead
// and every Parquet partition ever written becomes a hostage to that vendor.
type Account struct {
	TenantID string
	UserID   string

	// Email and Name are cached from the last sign-in for display. They are
	// deliberately not identifiers — see Claims.Federated — and nothing may
	// look a user up by them.
	Email string
	Name  string
}

// Directory resolves a verified provider identity into an account, and answers
// which accounts exist.
//
// It is an interface because the backing store is an open question: today it is
// memory, next it is Postgres, and the choice should not reach into the flows.
// Every method takes a context so a slow database cannot outlive the request
// that needed it.
type Directory interface {
	// Resolve finds the account linked to these claims, creating one on first
	// sign-in. Implementations must key on Claims.Federated and must not treat
	// two identities sharing an email as the same person.
	Resolve(ctx context.Context, c Claims) (Account, error)

	// Lookup returns an account by our own user id. It is what a session cookie
	// or a collector token is resolved through on each request.
	Lookup(ctx context.Context, userID string) (Account, bool, error)
}

// ErrUnknownUser is returned when a credential names a user that no longer
// exists — a revoked account whose token is still in someone's config file.
var ErrUnknownUser = errors.New("unknown user")

// TenantPolicy decides which tenant a first-time user joins.
//
// It exists because that decision is a product decision with no defensible
// default. Giving every new sign-in its own tenant is right for self-serve and
// wrong for a company, where it would scatter one team across many tenants and
// make the org rollup — the thing a manager is buying — permanently empty.
// Domain matching is right for a company and wrong for consumer domains, where
// it would merge unrelated people who happen to use the same mail host.
//
// So it is a seam rather than a rule. What must not happen is a default chosen
// by accident: tenancy is the isolation boundary, and a user placed in the
// wrong one can read another company's spend.
type TenantPolicy interface {
	TenantFor(ctx context.Context, c Claims) (string, error)
}

// TenantPerUser gives every new sign-in a tenant of its own. It is the correct
// starting policy: it can never over-share, and merging two tenants later is a
// migration, whereas splitting one that wrongly merged two companies means
// deciding row by row who owned what.
type TenantPerUser struct{}

// TenantFor implements TenantPolicy.
func (TenantPerUser) TenantFor(context.Context, Claims) (string, error) {
	return NewID("t"), nil
}

// TenantPerEmailDomain groups sign-ins by the domain of a verified email.
//
// Only a verified address is grouped: an unverified one is self-asserted, so
// honouring it would let anyone join a company's tenant by typing its domain
// into a sign-up form. Unverified addresses fall back to their own tenant.
//
// Consumer domains are refused outright rather than grouped, because every
// gmail.com user is not one organisation. The list is short and deliberately
// not exhaustive — this policy is for deployments that know their domains.
type TenantPerEmailDomain struct {
	// Consumer domains that must never form a shared tenant. If nil, a small
	// built-in list is used.
	ConsumerDomains map[string]bool

	mu       sync.Mutex
	byDomain map[string]string
}

var defaultConsumerDomains = map[string]bool{
	"gmail.com": true, "googlemail.com": true, "outlook.com": true,
	"hotmail.com": true, "live.com": true, "yahoo.com": true,
	"icloud.com": true, "me.com": true, "proton.me": true, "protonmail.com": true,
}

// TenantFor implements TenantPolicy.
func (p *TenantPerEmailDomain) TenantFor(_ context.Context, c Claims) (string, error) {
	domain := emailDomain(c.Email)
	consumer := p.ConsumerDomains
	if consumer == nil {
		consumer = defaultConsumerDomains
	}
	if domain == "" || !c.EmailVerified || consumer[domain] {
		return NewID("t"), nil
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	if p.byDomain == nil {
		p.byDomain = map[string]string{}
	}
	if id, ok := p.byDomain[domain]; ok {
		return id, nil
	}
	id := NewID("t")
	p.byDomain[domain] = id
	return id, nil
}

func emailDomain(email string) string {
	at := strings.LastIndex(email, "@")
	if at < 0 || at == len(email)-1 {
		return ""
	}
	return strings.ToLower(email[at+1:])
}

// MemoryDirectory is an in-memory Directory.
//
// It is not a placeholder to be deleted but the reference implementation the
// Postgres one has to match, and it is what the tests run against. Its data
// does not survive a restart, which is correct for a single-user local install
// — where nobody signs in at all — and obviously wrong for a server, which is
// why the server will not start with it.
type MemoryDirectory struct {
	policy TenantPolicy

	mu           sync.Mutex
	byFederated  map[string]string // issuer|subject -> user id
	byUser       map[string]Account
	lastSeen     map[string]time.Time
	tenantOfUser map[string]string
}

// NewMemoryDirectory builds an empty directory. A nil policy means one tenant
// per user, the policy that cannot over-share.
func NewMemoryDirectory(policy TenantPolicy) *MemoryDirectory {
	if policy == nil {
		policy = TenantPerUser{}
	}
	return &MemoryDirectory{
		policy:       policy,
		byFederated:  map[string]string{},
		byUser:       map[string]Account{},
		lastSeen:     map[string]time.Time{},
		tenantOfUser: map[string]string{},
	}
}

// Resolve implements Directory.
func (d *MemoryDirectory) Resolve(ctx context.Context, c Claims) (Account, error) {
	if c.Issuer == "" || c.Subject == "" {
		return Account{}, errors.New("claims carry no federated identity")
	}
	key := c.Federated()

	d.mu.Lock()
	userID, known := d.byFederated[key]
	d.mu.Unlock()

	if known {
		d.mu.Lock()
		defer d.mu.Unlock()
		acct := d.byUser[userID]
		// Refresh the display fields; they change without the identity
		// changing, which is exactly why they are not the key.
		acct.Email, acct.Name = c.Email, c.Name
		d.byUser[userID] = acct
		d.lastSeen[userID] = time.Now()
		return acct, nil
	}

	// The policy may reach a database, so it runs outside the lock.
	tenantID, err := d.policy.TenantFor(ctx, c)
	if err != nil {
		return Account{}, err
	}

	d.mu.Lock()
	defer d.mu.Unlock()
	// Re-check: two sign-ins for the same new user can race here, and the
	// second must join the first rather than mint a second account.
	if userID, known := d.byFederated[key]; known {
		return d.byUser[userID], nil
	}
	acct := Account{
		TenantID: tenantID,
		UserID:   NewID("u"),
		Email:    c.Email,
		Name:     c.Name,
	}
	d.byFederated[key] = acct.UserID
	d.byUser[acct.UserID] = acct
	d.tenantOfUser[acct.UserID] = tenantID
	d.lastSeen[acct.UserID] = time.Now()
	return acct, nil
}

// Lookup implements Directory.
func (d *MemoryDirectory) Lookup(_ context.Context, userID string) (Account, bool, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	acct, ok := d.byUser[userID]
	return acct, ok, nil
}
