// Package tenant resolves a tenant id to the storage that holds its data.
//
// This is where isolation lives. Every read path in this system takes a window
// and walks partitions, and none of them takes a tenant predicate — because a
// predicate can be forgotten, and the failure mode of forgetting one is that a
// query serves another company's spend. Instead the tenant is resolved to a
// root directory here, once, from an authenticated credential, and every path
// below it is derived from that root. A handler that never learns the tenant id
// physically cannot reach another tenant's bytes.
//
// The registry has two shapes, and both go through the same API so no caller
// can tell them apart:
//
//   - Single, for the local tool. One tenant, one fixed root — the layout that
//     already exists on people's machines, so a build that learns about tenants
//     does not move anybody's archive.
//   - Partitioned, for the server. <root>/tenants/<id>/, one subtree each.
//
// Local mode is trivially isolated: there is one tenant and its root is fixed,
// so the resolution step cannot go wrong. That is the point of routing it
// through the same call — the server path gets exercised by every request the
// local tool makes, rather than being a separate branch nobody runs.
package tenant

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Urenzu/trace-portal/internal/compact"
	"github.com/Urenzu/trace-portal/internal/eventstore"
	"github.com/Urenzu/trace-portal/internal/objectstore"
	"github.com/Urenzu/trace-portal/internal/postgres"
	"github.com/Urenzu/trace-portal/internal/store"
	"github.com/Urenzu/trace-portal/internal/trace"
)

// Backend is one tenant's hot window plus the blobs behind its turns.
//
// Both implementations satisfy it — files locally, Postgres on a server — which
// is what makes "local development is the same architecture as production" true
// rather than aspirational: the difference between the two is which constructor
// ran, and nothing above this line can tell.
type Backend interface {
	eventstore.Store
	GetBlob(ctx context.Context, ref string) ([]byte, error)
	PutBlob(ctx context.Context, payload []byte) (string, error)
}

// ErrUnknownTenant is returned for a tenant that is not permitted here. It is
// deliberately the same error whether the id was malformed, unknown, or simply
// not this registry's — a caller learning which would learn something about
// tenants it has no business knowing.
var ErrUnknownTenant = errors.New("unknown tenant")

// Storage is everything one tenant's data is reached through.
type Storage struct {
	Store     Backend
	Compactor *compact.Compactor

	// Root is the directory this tenant's data lives under. Exported for
	// diagnostics and for the test that asserts two tenants never share one.
	Root string
}

// Registry hands out per-tenant storage.
type Registry struct {
	root   string
	single string // non-empty in local mode: the only tenant permitted

	// pool is non-nil when the hot window lives in Postgres. Compaction still
	// writes Parquet under root either way: the database holds the days still
	// being written to, and the partitions hold the history.
	pool *pgxpool.Pool

	// identity is what a Postgres-backed store stamps onto events arriving
	// without one. The file store reads its own from the enrollment beside the
	// archive; a server has no such file, and the ingest endpoint has already
	// stamped from the credential by the time anything reaches here.
	identity trace.Identity

	// objects builds the object store holding one tenant's Parquet. Nil means
	// local directories.
	//
	// A function rather than a store, because each tenant needs its own key
	// space. The prefix is derived from the tenant id here, from an
	// authenticated value, and never from anything a request carried -- which is
	// the same rule the filesystem layout follows by putting the tenant in the
	// path.
	objects func(tenantID string) (objectstore.Store, error)

	mu   sync.Mutex
	open map[string]*Storage
}

// NewSingle builds a registry for the local tool: one tenant, rooted at dataDir
// itself.
//
// The root is dataDir rather than dataDir/tenants/local so that an archive
// written by an earlier build is still the archive this one reads. A layout
// change here would be a silent data migration on somebody's laptop, and the
// only thing it would buy is symmetry with a mode this process is not in.
func NewSingle(dataDir, tenantID string) (*Registry, error) {
	if tenantID == "" {
		return nil, errors.New("a single-tenant registry needs a tenant id")
	}
	return &Registry{root: dataDir, single: tenantID, open: map[string]*Storage{}}, nil
}

// NewPartitioned builds a registry for the server: one subtree per tenant under
// <root>/tenants/.
func NewPartitioned(root string) (*Registry, error) {
	if err := os.MkdirAll(filepath.Join(root, "tenants"), 0o755); err != nil {
		return nil, fmt.Errorf("create tenants dir: %w", err)
	}
	return &Registry{root: root, open: map[string]*Storage{}}, nil
}

// Single reports whether this registry serves exactly one tenant.
func (r *Registry) Single() bool { return r.single != "" }

// For resolves a tenant to its storage, opening it on first use.
//
// The id must have come from an authenticated credential. Nothing here checks
// that a tenant exists in a directory — this is the storage layer, and a tenant
// with no data yet is an ordinary state — so passing an id taken from a request
// body would create a subtree for whatever the caller typed.
func (r *Registry) For(tenantID string) (*Storage, error) {
	if err := ValidID(tenantID); err != nil {
		return nil, err
	}
	if r.single != "" && tenantID != r.single {
		return nil, ErrUnknownTenant
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if s, ok := r.open[tenantID]; ok {
		return s, nil
	}

	dir := r.root
	if r.single == "" {
		dir = filepath.Join(r.root, "tenants", tenantID)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("create tenant dir: %w", err)
		}
	}

	var backend Backend
	if r.pool != nil {
		pg, err := postgres.Open(r.pool, tenantID, r.identity)
		if err != nil {
			return nil, err
		}
		backend = pgBackend{pg}
	} else {
		st, err := store.Open(dir)
		if err != nil {
			return nil, err
		}
		backend = st
	}

	var c *compact.Compactor
	if r.objects != nil {
		objects, err := r.objects(tenantID)
		if err != nil {
			backend.Close()
			return nil, err
		}
		c = compact.NewWithObjects(backend, objects)
	} else {
		local, err := compact.New(backend, dir)
		if err != nil {
			backend.Close()
			return nil, err
		}
		c = local
	}
	s := &Storage{Store: backend, Compactor: c, Root: dir}
	r.open[tenantID] = s
	return s, nil
}

// Tenants lists the tenants with data on disk, for the background jobs that
// have to sweep all of them.
func (r *Registry) Tenants() ([]string, error) {
	if r.single != "" {
		return []string{r.single}, nil
	}
	entries, err := os.ReadDir(filepath.Join(r.root, "tenants"))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		// A directory whose name is not a valid id was not written by this
		// code. Skipping it rather than serving it means a file dropped into
		// the tenants directory cannot become a tenant.
		if ValidID(e.Name()) != nil {
			continue
		}
		out = append(out, e.Name())
	}
	return out, nil
}

// WithPostgres points the registry's hot window at a database.
//
// The Parquet root is unchanged: a server keeps its partitions on disk (and,
// once object storage lands, in a bucket) regardless of where the window lives.
func (r *Registry) WithPostgres(pool *pgxpool.Pool, id trace.Identity) *Registry {
	r.pool, r.identity = pool, id
	return r
}

// WithObjectStorage points compacted history at a bucket.
//
// Each tenant gets its own key prefix, so isolation is in the key the same way
// it is in the path: a query that never learns another tenant id cannot name
// another tenant object. One bucket rather than one per tenant, because buckets
// are a provisioning operation with account-level limits and prefixes are free.
func (r *Registry) WithObjectStorage(build func(prefix string) (objectstore.Store, error)) *Registry {
	r.objects = func(tenantID string) (objectstore.Store, error) {
		if err := ValidID(tenantID); err != nil {
			return nil, err
		}
		return build(objectstore.Key("tenants", tenantID, "compact"))
	}
	return r
}

// pgBackend adapts the Postgres store's blob methods to the names the file
// store uses. Two names for one operation is a small cost for not renaming a
// method that reads well at every existing call site.
type pgBackend struct{ *postgres.Store }

func (b pgBackend) PutBlob(ctx context.Context, payload []byte) (string, error) {
	return b.Put(ctx, payload)
}

func (b pgBackend) GetBlob(ctx context.Context, ref string) ([]byte, error) {
	return b.Get(ctx, ref)
}

// Close releases every open tenant.
func (r *Registry) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	var first error
	for _, s := range r.open {
		if err := s.Store.Close(); err != nil && first == nil {
			first = err
		}
	}
	r.open = map[string]*Storage{}
	return first
}

// reservedNames are the Windows device names. They are spellable in the
// alphabet below, and a directory cannot be created with one on Windows — the
// platform the local tool is most often run on. Nothing this system mints could
// collide, so this is defence in depth rather than a live case; it costs one map
// lookup and turns a baffling filesystem error into a plain refusal.
var reservedNames = map[string]bool{
	"con": true, "prn": true, "aux": true, "nul": true,
	"com1": true, "com2": true, "com3": true, "com4": true,
	"com5": true, "com6": true, "com7": true, "com8": true, "com9": true,
	"lpt1": true, "lpt2": true, "lpt3": true, "lpt4": true,
	"lpt5": true, "lpt6": true, "lpt7": true, "lpt8": true, "lpt9": true,
}

// idMaxLen bounds an id well above anything this system mints, so a long string
// cannot be used to probe path-length behaviour.
const idMaxLen = 64

// ValidID checks that a tenant id is safe to use as a path segment.
//
// This is stricter than it needs to be for ids this system mints, and
// deliberately so: it is the check that stands between an id and the filesystem.
// This repository has already shipped one directory traversal — a blob
// reference was length-checked but not alphabet-checked, and a 64-character
// string carrying separators resolved outside the blob store. The lesson from
// that one is that validating the alphabet is what matters, not validating the
// shape, so this accepts only lowercase hex, digits and the single underscore
// the id prefix uses.
//
// Rejected by construction: "..", "a/b", "a\\b", an absolute path, a drive
// letter and a leading dot, since none of them can be spelled in this alphabet.
// Windows device names can be, so they are rejected explicitly.
func ValidID(id string) error {
	if id == "" || len(id) > idMaxLen {
		return ErrUnknownTenant
	}
	if reservedNames[id] {
		return ErrUnknownTenant
	}
	underscores := 0
	for i := 0; i < len(id); i++ {
		c := id[i]
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9':
		case c == '_':
			underscores++
			// A leading or trailing underscore, or more than the one separating
			// a prefix from its body, is not something this system mints.
			if i == 0 || i == len(id)-1 || underscores > 1 {
				return ErrUnknownTenant
			}
		default:
			return ErrUnknownTenant
		}
	}
	return nil
}
