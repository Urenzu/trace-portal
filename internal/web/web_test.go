package web

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The embedded build must actually be present, or the binary silently ships
// without a UI.
func TestFrontendIsEmbedded(t *testing.T) {
	if !Available() {
		t.Fatal("no frontend embedded; run `npm run build` in web/")
	}
}

func TestServesIndexAndAssets(t *testing.T) {
	srv := httptest.NewServer(Handler())
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("index status = %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "text/html") {
		t.Errorf("index content-type = %q", ct)
	}
}

// Deep links and reloads must land on index.html, since the UI keeps its state
// in the URL hash and unknown paths are not real files.
func TestUnknownPathFallsBackToIndex(t *testing.T) {
	srv := httptest.NewServer(Handler())
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/some/deep/link")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "text/html") {
		t.Errorf("content-type = %q, want html", ct)
	}
}

// Hashed assets are immutable and cached hard; index.html must not be, or a
// rebuilt UI never reaches an already-open browser.
func TestCacheHeaders(t *testing.T) {
	srv := httptest.NewServer(Handler())
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if got := resp.Header.Get("Cache-Control"); got != "no-cache" {
		t.Errorf("index Cache-Control = %q, want no-cache", got)
	}
}
