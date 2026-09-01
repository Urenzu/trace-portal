package objectstore

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// A prefixed store is how one bucket holds many tenants, so it has to behave
// exactly like an unprefixed one from the caller's side.
func TestPrefixedStore(t *testing.T) {
	runStoreSuite(t, "prefixed", func(t *testing.T) Store {
		inner, err := NewLocal(t.TempDir())
		if err != nil {
			t.Fatalf("new local: %v", err)
		}
		return Prefixed(inner, "tenants/t_acme/compact")
	})
}

// Two tenants over one bucket must not see each other. This is the object-store
// half of the same property the filesystem gives by putting the tenant in the
// path.
func TestPrefixedStoresAreIsolated(t *testing.T) {
	inner, err := NewLocal(t.TempDir())
	if err != nil {
		t.Fatalf("new local: %v", err)
	}
	ctx := context.Background()

	acme := Prefixed(inner, "tenants/t_acme/compact")
	beta := Prefixed(inner, "tenants/t_beta/compact")

	if err := acme.Put(ctx, "2026-08-28/turns.parquet", []byte("acme turns")); err != nil {
		t.Fatalf("put: %v", err)
	}

	// The same key names a different object for the other tenant.
	if _, err := beta.Get(ctx, "2026-08-28/turns.parquet"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("one tenant read another's partition: %v", err)
	}
	keys, err := beta.List(ctx, "")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(keys) != 0 {
		t.Fatalf("a tenant listed another's keys: %v", keys)
	}

	// And the underlying bucket really does hold it under the scoped key.
	all, err := inner.List(ctx, "")
	if err != nil {
		t.Fatalf("list inner: %v", err)
	}
	if len(all) != 1 || all[0] != "tenants/t_acme/compact/2026-08-28/turns.parquet" {
		t.Fatalf("unexpected underlying keys: %v", all)
	}
}

// A listing has to come back without the prefix, or its results could not be
// passed straight back to Get — which would prefix them a second time.
func TestPrefixedListStripsThePrefix(t *testing.T) {
	inner, err := NewLocal(t.TempDir())
	if err != nil {
		t.Fatalf("new local: %v", err)
	}
	ctx := context.Background()
	s := Prefixed(inner, "tenants/t_acme/compact")

	if err := s.Put(ctx, "2026-08-28/turns.parquet", []byte("x")); err != nil {
		t.Fatalf("put: %v", err)
	}
	keys, err := s.List(ctx, "")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(keys) != 1 || keys[0] != "2026-08-28/turns.parquet" {
		t.Fatalf("List returned %v; keys must round-trip into Get", keys)
	}
	if strings.Contains(keys[0], "tenants/") {
		t.Fatal("List leaked the prefix")
	}
	if _, err := s.Get(ctx, keys[0]); err != nil {
		t.Fatalf("a listed key did not resolve: %v", err)
	}
}
