package objectstore

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// One suite, run against every backend.
//
// Written this way because the failure worth preventing is not "a backend is
// broken" but "two backends disagree" — a key that resolves locally and not in
// S3, or a missing object that reports as a read error on one and a miss on the
// other. Those only show up in the deployment that uses the backend nobody
// developed against, which is the worst place to find them.
func runStoreSuite(t *testing.T, name string, open func(t *testing.T) Store) {
	t.Run(name+"/put and get round-trips", func(t *testing.T) {
		s, ctx := open(t), context.Background()
		want := []byte("some parquet bytes")
		if err := s.Put(ctx, "compact/2026-08-28/turns.parquet", want); err != nil {
			t.Fatalf("put: %v", err)
		}
		got, err := s.Get(ctx, "compact/2026-08-28/turns.parquet")
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		if string(got) != string(want) {
			t.Fatalf("got %q, want %q", got, want)
		}
	})

	t.Run(name+"/missing key is ErrNotFound", func(t *testing.T) {
		s, ctx := open(t), context.Background()
		_, err := s.Get(ctx, "compact/nope/turns.parquet")
		if !errors.Is(err, ErrNotFound) {
			t.Fatalf("want ErrNotFound, got %v", err)
		}
		// The compactor treats an absent partition as an empty result by
		// checking fs.ErrNotExist, and the API turns it into a 404. Both must
		// keep working whichever store is behind the interface.
		if !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("ErrNotFound must wrap fs.ErrNotExist, got %v", err)
		}
	})

	t.Run(name+"/exists does not lie", func(t *testing.T) {
		s, ctx := open(t), context.Background()
		ok, err := s.Exists(ctx, "compact/2026-08-28/turns.parquet")
		if err != nil {
			t.Fatalf("exists: %v", err)
		}
		if ok {
			t.Fatal("reported an object that was never written")
		}
		if err := s.Put(ctx, "compact/2026-08-28/turns.parquet", []byte("x")); err != nil {
			t.Fatalf("put: %v", err)
		}
		ok, err = s.Exists(ctx, "compact/2026-08-28/turns.parquet")
		if err != nil {
			t.Fatalf("exists: %v", err)
		}
		if !ok {
			t.Fatal("did not report an object that was written")
		}
	})

	t.Run(name+"/put replaces", func(t *testing.T) {
		s, ctx := open(t), context.Background()
		key := "compact/rollup/day.parquet"
		if err := s.Put(ctx, key, []byte("first")); err != nil {
			t.Fatalf("put: %v", err)
		}
		// Rebuilding the rollup overwrites it on every compaction, so this is
		// the common case rather than an edge one.
		if err := s.Put(ctx, key, []byte("second")); err != nil {
			t.Fatalf("overwrite: %v", err)
		}
		got, err := s.Get(ctx, key)
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		if string(got) != "second" {
			t.Fatalf("got %q after overwrite", got)
		}
	})

	t.Run(name+"/list is prefix-scoped and sorted", func(t *testing.T) {
		s, ctx := open(t), context.Background()
		keys := []string{
			"compact/2026-08-28/turns.parquet",
			"compact/2026-08-28/day.parquet",
			"compact/2026-08-29/turns.parquet",
			"compact/rollup/day.parquet",
			"events/2026-08-28.jsonl",
		}
		for _, k := range keys {
			if err := s.Put(ctx, k, []byte("x")); err != nil {
				t.Fatalf("put %s: %v", k, err)
			}
		}

		got, err := s.List(ctx, "compact/2026-08-28/")
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		if len(got) != 2 {
			t.Fatalf("prefix listing returned %d keys: %v", len(got), got)
		}
		if got[0] != "compact/2026-08-28/day.parquet" || got[1] != "compact/2026-08-28/turns.parquet" {
			t.Fatalf("listing is not sorted or not scoped: %v", got)
		}

		all, err := s.List(ctx, "compact/")
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		if len(all) != 4 {
			t.Fatalf("compact/ listing returned %d keys: %v", len(all), all)
		}
		// Keys come back as keys, with forward slashes, on every platform.
		for _, k := range all {
			if strings.Contains(k, `\`) {
				t.Fatalf("listing returned a filesystem path, not a key: %q", k)
			}
		}
	})

	t.Run(name+"/delete is idempotent", func(t *testing.T) {
		s, ctx := open(t), context.Background()
		key := "compact/2026-08-28/turns.parquet"
		if err := s.Put(ctx, key, []byte("x")); err != nil {
			t.Fatalf("put: %v", err)
		}
		if err := s.Delete(ctx, key); err != nil {
			t.Fatalf("delete: %v", err)
		}
		// Cleaning up twice must be safe: compaction deletes a superseded
		// partition and may be interrupted between the delete and the record of
		// it having happened.
		if err := s.Delete(ctx, key); err != nil {
			t.Fatalf("second delete: %v", err)
		}
		if _, err := s.Get(ctx, key); !errors.Is(err, ErrNotFound) {
			t.Fatalf("object survived delete: %v", err)
		}
	})

	t.Run(name+"/empty object round-trips", func(t *testing.T) {
		s, ctx := open(t), context.Background()
		// A day with no tools invoked produces an empty rollup. Zero bytes must
		// be a stored object rather than an absent one, or that day would fall
		// back to re-reading the raw log forever.
		if err := s.Put(ctx, "compact/2026-08-28/by_tool.parquet", []byte{}); err != nil {
			t.Fatalf("put empty: %v", err)
		}
		ok, err := s.Exists(ctx, "compact/2026-08-28/by_tool.parquet")
		if err != nil || !ok {
			t.Fatalf("empty object does not exist: ok=%v err=%v", ok, err)
		}
		got, err := s.Get(ctx, "compact/2026-08-28/by_tool.parquet")
		if err != nil {
			t.Fatalf("get empty: %v", err)
		}
		if len(got) != 0 {
			t.Fatalf("empty object came back with %d bytes", len(got))
		}
	})
}

func TestLocalStore(t *testing.T) {
	runStoreSuite(t, "local", func(t *testing.T) Store {
		s, err := NewLocal(t.TempDir())
		if err != nil {
			t.Fatalf("new local: %v", err)
		}
		return s
	})
}

// The S3 suite runs against MinIO when one is reachable.
//
//	docker compose up -d minio
//	TRACE_PORTAL_TEST_S3=localhost:9000 go test ./internal/objectstore/
func TestS3Store(t *testing.T) {
	endpoint := os.Getenv("TRACE_PORTAL_TEST_S3")
	if endpoint == "" {
		t.Skip("set TRACE_PORTAL_TEST_S3 to run these (docker compose up -d minio)")
	}
	runStoreSuite(t, "s3", func(t *testing.T) Store {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		// A bucket per subtest, so the cases cannot see each other's keys and
		// can run against a MinIO somebody else is also using.
		bucket := "tp-test-" + strings.ToLower(strings.NewReplacer("/", "-", " ", "-", "_", "-").Replace(t.Name()))
		if len(bucket) > 63 {
			bucket = bucket[:63]
		}
		bucket = strings.Trim(bucket, "-")

		s, err := NewS3(ctx, S3Config{
			Endpoint:  endpoint,
			Bucket:    bucket,
			AccessKey: envOr("TRACE_PORTAL_TEST_S3_KEY", "trace"),
			SecretKey: envOr("TRACE_PORTAL_TEST_S3_SECRET", "tracetrace"),
			Region:    "us-east-1",
		})
		if err != nil {
			t.Fatalf("new s3: %v", err)
		}
		// Buckets outlive the process, so a previous run leaves objects behind
		// and the next one fails as a real-looking assertion -- "reported an
		// object that was never written" -- rather than as an obvious leftover.
		// That is the worst kind of flake to debug, so the bucket is emptied on
		// the way in as well as on the way out: cleaning up afterwards is a
		// courtesy, and starting from a known state is the part that makes the
		// test mean anything.
		purge := func() {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			keys, err := s.List(ctx, "")
			if err != nil {
				return
			}
			for _, k := range keys {
				_ = s.Delete(ctx, k)
			}
		}
		purge()
		t.Cleanup(purge)
		return s
	})
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// Keys are assembled by this codebase, not typed by a user — but that was also
// true of the blob reference that turned out to be a directory traversal here,
// and the fix then was to validate rather than to trust the caller.
func TestLocalRefusesKeysThatEscapeTheRoot(t *testing.T) {
	dir := t.TempDir()
	s, err := NewLocal(filepath.Join(dir, "store"))
	if err != nil {
		t.Fatalf("new local: %v", err)
	}
	ctx := context.Background()

	for _, key := range []string{
		"../escaped.parquet",
		"compact/../../escaped.parquet",
		`..\escaped.parquet`,
		"",
	} {
		if err := s.Put(ctx, key, []byte("x")); err == nil {
			t.Errorf("Put(%q) was allowed", key)
		}
		if _, err := s.Get(ctx, key); err == nil {
			t.Errorf("Get(%q) was allowed", key)
		}
	}

	// Nothing may have been written outside the store.
	if _, err := os.Stat(filepath.Join(dir, "escaped.parquet")); !os.IsNotExist(err) {
		t.Fatal("a key escaped the object store root")
	}
}

// A key built with filepath.Join on Windows carries backslashes and names a
// different object from the same key built on Linux. Nothing would report an
// error — both stores would be behaving correctly — and an archive written by a
// laptop would be unreadable by the server.
func TestKeyAlwaysUsesForwardSlashes(t *testing.T) {
	got := Key("compact", "2026-08-28", "turns.parquet")
	if got != "compact/2026-08-28/turns.parquet" {
		t.Fatalf("Key() = %q", got)
	}
	if strings.Contains(got, `\`) {
		t.Fatalf("Key() produced a backslash on this platform: %q", got)
	}
	if got := Key(`compact\2026-08-28`, "turns.parquet"); got != "compact/2026-08-28/turns.parquet" {
		t.Fatalf("Key() did not normalise a backslash: %q", got)
	}
	if got := Key("compact", "", "turns.parquet"); got != "compact/turns.parquet" {
		t.Fatalf("Key() did not drop an empty part: %q", got)
	}
}

// A half-written local object must never appear in a listing, or the compactor
// would try to read a temporary file as a partition.
func TestLocalListIgnoresPartialWrites(t *testing.T) {
	dir := t.TempDir()
	s, err := NewLocal(dir)
	if err != nil {
		t.Fatalf("new local: %v", err)
	}
	ctx := context.Background()

	if err := s.Put(ctx, "compact/2026-08-28/turns.parquet", []byte("x")); err != nil {
		t.Fatalf("put: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "compact", "2026-08-28", "turns.parquet.tmp"), []byte("half"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	keys, err := s.List(ctx, "compact/")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(keys) != 1 || keys[0] != "compact/2026-08-28/turns.parquet" {
		t.Fatalf("listing included a partial write: %v", keys)
	}
}

// An endpoint pasted out of a dashboard arrives as a URL; minio.New wants a
// host. Dropping the scheme silently is how a correct-looking endpoint produces
// a connection refused against the wrong port.
func TestNormalizeEndpoint(t *testing.T) {
	cases := []struct {
		in         string
		useSSL     bool
		wantHost   string
		wantSecure bool
	}{
		{"localhost:9000", false, "localhost:9000", false},
		{"localhost:9000", true, "localhost:9000", true},
		{"https://acct.r2.cloudflarestorage.com", false, "acct.r2.cloudflarestorage.com", true},
		{"http://minio:9000", true, "minio:9000", false},
		{"minio:9000/", false, "minio:9000", false},
	}
	for _, tc := range cases {
		host, secure, err := normalizeEndpoint(tc.in, tc.useSSL)
		if err != nil {
			t.Errorf("normalizeEndpoint(%q): %v", tc.in, err)
			continue
		}
		if host != tc.wantHost || secure != tc.wantSecure {
			t.Errorf("normalizeEndpoint(%q, ssl=%v) = %q/%v, want %q/%v",
				tc.in, tc.useSSL, host, secure, tc.wantHost, tc.wantSecure)
		}
	}
	if _, _, err := normalizeEndpoint("", false); err == nil {
		t.Error("an empty endpoint should be refused")
	}
}
