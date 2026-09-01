// Package objectstore is where compacted history lives.
//
// Partitions and rollups are written once and then only read, which is exactly
// what object storage is for and exactly what a database is not for. Keeping
// them here rather than in Postgres is what lets a year of history cost
// object-storage prices while the database holds only the days still being
// written to.
//
// Two implementations, one interface:
//
//   - Local, a directory. What the single-machine tool uses, and what makes
//     "one binary, nothing to install" true.
//   - S3, anything speaking the S3 API — MinIO in development, R2 or S3 in
//     production. Moving between those is a configuration change, because the
//     protocol is the same.
//
// # Keys are not paths
//
// An object store has no directories. A key is one opaque string, and the
// slashes in it are a naming convention that consoles render as folders. Two
// consequences are load-bearing here.
//
// Keys always use forward slashes, including on Windows. A key assembled with
// filepath.Join on Windows would contain backslashes and name a *different
// object* from the same key assembled on Linux — an archive written by a
// developer's laptop would be unreadable by the server, and nothing would
// report an error, because both stores would be behaving correctly.
//
// And there is no rename. The local compactor publishes a partition by writing
// a temporary directory and renaming it into place, so a reader never sees a
// half-written partition. That trick does not exist here, so completeness has
// to be published explicitly — see the marker convention in the compactor.
package objectstore

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// ErrNotFound is returned for a key that does not exist. It wraps fs.ErrNotExist
// so callers can use errors.Is against either, and so the API layer's existing
// "missing means 404" check keeps working whichever store is behind it.
var ErrNotFound = fmt.Errorf("object not found: %w", fs.ErrNotExist)

// Store is the whole contract. Deliberately small: everything compaction does
// is write an object, read an object, ask whether one exists, and list a
// prefix. A larger interface would be a larger thing to reimplement for every
// backend, and there is no operation missing that Parquet needs.
type Store interface {
	// Get returns an object's bytes, or ErrNotFound.
	Get(ctx context.Context, key string) ([]byte, error)

	// Put writes an object, replacing any existing one. Writes are whole
	// objects rather than streams: the largest thing written here is a day's
	// turns, which is tens of kilobytes.
	Put(ctx context.Context, key string, data []byte) error

	// Exists reports whether a key is present, without transferring it.
	Exists(ctx context.Context, key string) (bool, error)

	// List returns the keys under a prefix, sorted. Prefixes are literal string
	// prefixes, not directory paths.
	List(ctx context.Context, prefix string) ([]string, error)

	// Delete removes an object. Absent keys are not an error, so cleaning up
	// twice is safe.
	Delete(ctx context.Context, key string) error
}

// Key joins parts into an object key, always with forward slashes.
//
// Use this rather than filepath.Join anywhere a key is built. See the package
// comment for why a backslash here is a silent cross-platform data split rather
// than an error.
func Key(parts ...string) string {
	clean := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.Trim(strings.ReplaceAll(p, `\`, "/"), "/")
		if p != "" {
			clean = append(clean, p)
		}
	}
	return path.Join(clean...)
}

// Local is an object store backed by a directory.
type Local struct{ root string }

// NewLocal returns a Local rooted at dir, creating it if needed.
func NewLocal(dir string) (*Local, error) {
	if dir == "" {
		return nil, errors.New("local object store needs a directory")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("create object store dir: %w", err)
	}
	return &Local{root: dir}, nil
}

// Root is the directory this store writes into.
func (l *Local) Root() string { return l.root }

// path turns a key into a filesystem path, refusing anything that would escape
// the root.
//
// A key reaching this function has been assembled by this codebase, not typed
// by a user — but that was also true of the blob reference that turned out to
// be a directory traversal in this repository, and the fix there was to
// validate rather than to trust the caller. Any key containing a ".." segment
// is refused outright.
func (l *Local) path(key string) (string, error) {
	if key == "" {
		return "", fmt.Errorf("empty object key")
	}
	key = strings.ReplaceAll(key, `\`, "/")
	for _, seg := range strings.Split(key, "/") {
		if seg == ".." {
			return "", fmt.Errorf("object key %q escapes the store", key)
		}
	}
	return filepath.Join(l.root, filepath.FromSlash(key)), nil
}

// Get implements Store.
//
// Retries briefly on an error that is not a miss, because of how atomic replace
// works on Windows. Put writes a temporary file and renames it over the target;
// on Unix that is invisible to a concurrent reader, but Windows refuses to
// replace or open a file another handle has open, and a reader that happens to
// arrive during the rename gets a sharing violation rather than either version.
//
// It is a real case rather than a theoretical one: the rollup is rewritten by
// compaction while requests are reading it, which is exactly this. Found by the
// concurrency test in the compactor.
//
// Bounded and short. The window is the length of a rename, so a handful of
// milliseconds covers it; anything still failing after that is a genuine error
// and is returned.
func (l *Local) Get(_ context.Context, key string) ([]byte, error) {
	p, err := l.path(key)
	if err != nil {
		return nil, err
	}

	var lastErr error
	for attempt := 0; attempt < 20; attempt++ {
		data, err := os.ReadFile(p)
		if err == nil {
			return data, nil
		}
		if errors.Is(err, fs.ErrNotExist) {
			return nil, ErrNotFound
		}
		lastErr = err
		time.Sleep(time.Millisecond)
	}
	return nil, lastErr
}

// Put implements Store.
//
// Written to a temporary file and renamed, so a reader never observes a
// half-written object. That is free on a filesystem and is what the S3
// implementation gets from the protocol instead: an S3 PUT is atomic, and a
// reader sees either the old object or the new one.
func (l *Local) Put(_ context.Context, key string, data []byte) error {
	p, err := l.path(key)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}

// Exists implements Store.
func (l *Local) Exists(_ context.Context, key string) (bool, error) {
	p, err := l.path(key)
	if err != nil {
		return false, err
	}
	_, err = os.Stat(p)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	return err == nil, err
}

// List implements Store.
func (l *Local) List(_ context.Context, prefix string) ([]string, error) {
	prefix = strings.ReplaceAll(prefix, `\`, "/")

	var keys []string
	err := filepath.WalkDir(l.root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(l.root, p)
		if err != nil {
			return err
		}
		// Back to a key: forward slashes, so a listing on Windows returns the
		// same strings it would on Linux.
		key := filepath.ToSlash(rel)
		if strings.HasSuffix(key, ".tmp") {
			// A partially written object. Never a key anyone asked for.
			return nil
		}
		if strings.HasPrefix(key, prefix) {
			keys = append(keys, key)
		}
		return nil
	})
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	sort.Strings(keys)
	return keys, nil
}

// Delete implements Store.
func (l *Local) Delete(_ context.Context, key string) error {
	p, err := l.path(key)
	if err != nil {
		return err
	}
	if err := os.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

// Prefixed scopes a store to a key prefix.
//
// This is how one bucket holds many tenants. The prefix is fixed when the store
// is built, from an authenticated tenant id, and every key passed in afterwards
// is relative to it — so a caller that never learns another tenant's id cannot
// name another tenant's object, whatever key it constructs.
//
// The same reasoning as putting the tenant in a directory path, and for the same
// reason: a scope that is established once cannot be forgotten later, and a
// predicate applied per query can.
func Prefixed(s Store, prefix string) Store {
	if prefix == "" {
		return s
	}
	return &prefixed{inner: s, prefix: strings.TrimSuffix(prefix, "/") + "/"}
}

type prefixed struct {
	inner  Store
	prefix string
}

func (p *prefixed) key(k string) string { return p.prefix + strings.TrimPrefix(k, "/") }

func (p *prefixed) Get(ctx context.Context, key string) ([]byte, error) {
	return p.inner.Get(ctx, p.key(key))
}

func (p *prefixed) Put(ctx context.Context, key string, data []byte) error {
	return p.inner.Put(ctx, p.key(key), data)
}

func (p *prefixed) Exists(ctx context.Context, key string) (bool, error) {
	return p.inner.Exists(ctx, p.key(key))
}

func (p *prefixed) Delete(ctx context.Context, key string) error {
	return p.inner.Delete(ctx, p.key(key))
}

// List strips the prefix back off, so a caller sees the same keys it would from
// an unscoped store. Leaving it on would make a listing unusable as input to
// Get, since Get would then prefix it a second time.
func (p *prefixed) List(ctx context.Context, prefix string) ([]string, error) {
	keys, err := p.inner.List(ctx, p.key(prefix))
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		out = append(out, strings.TrimPrefix(k, p.prefix))
	}
	return out, nil
}
