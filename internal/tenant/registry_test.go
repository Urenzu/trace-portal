package tenant

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ValidID is the check standing between a tenant id and the filesystem.
//
// This repository has already shipped one directory traversal: a blob reference
// was length-checked but not alphabet-checked, and a 64-character string
// carrying separators resolved outside the blob store. The lesson was that the
// alphabet is what matters, so the table below is mostly the ways a path can be
// spelled — and every one of them is unspellable in the accepted alphabet,
// which is the point.
func TestValidIDRejectsAnythingThatCouldEscape(t *testing.T) {
	bad := []string{
		"",
		"..",
		"../../etc",
		"..\\..\\windows",
		"a/b",
		"a\\b",
		"/absolute",
		"C:",
		"c:\\windows",
		".hidden",
		"t_acme/../t_victim",
		"t_acme%2f..%2ft_victim",
		"con", // a Windows reserved device name is at least not a path
		"t acme",
		"t.acme",
		"T_ACME", // ids this system mints are lowercase
		"_leading",
		"trailing_",
		"two_under_scores",
		strings.Repeat("a", idMaxLen+1),
		"t_acme\x00",
		"t_acme\n",
	}
	for _, id := range bad {
		if err := ValidID(id); err == nil {
			t.Errorf("ValidID(%q) accepted it", id)
		}
	}

	good := []string{"local", "t_acme", "t_0f9a3b2c1d4e5f60718293a4b5c6d7e8", "abc123"}
	for _, id := range good {
		if err := ValidID(id); err != nil {
			t.Errorf("ValidID(%q) rejected a legitimate id: %v", id, err)
		}
	}
}

// Even if validation were bypassed, the resolved directory must stay under the
// registry root. Belt and braces on the one boundary where a mistake leaks
// another company's data.
func TestForNeverResolvesOutsideTheRoot(t *testing.T) {
	root := t.TempDir()
	r, err := NewPartitioned(root)
	if err != nil {
		t.Fatalf("registry: %v", err)
	}
	defer r.Close()

	for _, id := range []string{"../escape", "..", "a/../../b", "/etc"} {
		storage, err := r.For(id)
		if err == nil {
			abs, _ := filepath.Abs(storage.Root)
			rootAbs, _ := filepath.Abs(root)
			if !strings.HasPrefix(abs, rootAbs) {
				t.Fatalf("tenant %q resolved to %s, outside %s", id, abs, rootAbs)
			}
			t.Errorf("tenant %q should have been refused", id)
		}
	}
}

// The local tool serves exactly one tenant, and asking for any other must fail
// rather than quietly create a second archive on someone's laptop.
func TestSingleRegistryRefusesOtherTenants(t *testing.T) {
	dir := t.TempDir()
	r, err := NewSingle(dir, "local")
	if err != nil {
		t.Fatalf("registry: %v", err)
	}
	defer r.Close()

	storage, err := r.For("local")
	if err != nil {
		t.Fatalf("local tenant: %v", err)
	}
	// The root is the data directory itself, so an archive written by a build
	// that predates tenants is still the archive this one reads. A layout change
	// here would be a silent migration on somebody's machine.
	if storage.Root != dir {
		t.Fatalf("local root is %s, want %s — existing archives would be orphaned", storage.Root, dir)
	}

	if _, err := r.For("t_acme"); err == nil {
		t.Fatal("a single-tenant registry served a second tenant")
	}
}

// Tenants() is what the background sweeps iterate. A stray file or a
// directory with a name this system would never mint must not become a tenant.
func TestTenantsIgnoresWhatItDidNotWrite(t *testing.T) {
	root := t.TempDir()
	r, err := NewPartitioned(root)
	if err != nil {
		t.Fatalf("registry: %v", err)
	}
	defer r.Close()

	if _, err := r.For("t_acme"); err != nil {
		t.Fatalf("open acme: %v", err)
	}
	tenantsDir := filepath.Join(root, "tenants")
	if err := os.WriteFile(filepath.Join(tenantsDir, "notes.txt"), []byte("x"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(tenantsDir, "Not A Tenant"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	got, err := r.Tenants()
	if err != nil {
		t.Fatalf("tenants: %v", err)
	}
	if len(got) != 1 || got[0] != "t_acme" {
		t.Fatalf("Tenants() = %v, want [t_acme]", got)
	}
}

// Resolving the same tenant twice must give the same storage, not a second
// store with its own open file handle on the same day log.
func TestForIsStableForOneTenant(t *testing.T) {
	root := t.TempDir()
	r, err := NewPartitioned(root)
	if err != nil {
		t.Fatalf("registry: %v", err)
	}
	defer r.Close()

	a, err := r.For("t_acme")
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	b, err := r.For("t_acme")
	if err != nil {
		t.Fatalf("second: %v", err)
	}
	if a != b {
		t.Fatal("two resolutions of one tenant returned different storage")
	}
}
