// Package web serves the built frontend from inside the binary, so the whole
// tool ships as a single file with no assets to install alongside it.
package web

import (
	"embed"
	"io/fs"
	"net/http"
	"strings"
)

// dist holds the Vite build output. It is committed so that `go build` works
// without Node installed; rebuild it with `npm run build` in web/.
//
//go:embed all:dist
var dist embed.FS

// Available reports whether a real frontend was embedded. A checkout with only
// the placeholder still serves something useful rather than a blank page.
func Available() bool {
	entries, err := fs.ReadDir(dist, "dist/assets")
	return err == nil && len(entries) > 0
}

// Handler serves the frontend, falling back to index.html for any path that is
// not a real file. The UI keeps its state in the URL hash, so this only has to
// cover deep links and reloads, not client-side routes.
func Handler() http.Handler {
	sub, err := fs.Sub(dist, "dist")
	if err != nil {
		return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "frontend not embedded", http.StatusInternalServerError)
		})
	}
	files := http.FS(sub)
	fileServer := http.FileServer(files)

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, "/")
		if path == "" {
			path = "index.html"
		}

		if f, err := files.Open(path); err == nil {
			f.Close()
			// Hashed asset filenames are immutable, so they can be cached hard;
			// index.html must not be, or a rebuild never reaches the browser.
			if strings.HasPrefix(path, "assets/") {
				w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
			} else {
				w.Header().Set("Cache-Control", "no-cache")
			}
			fileServer.ServeHTTP(w, r)
			return
		}

		index, err := fs.ReadFile(sub, "index.html")
		if err != nil {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache")
		w.Write(index)
	})
}
