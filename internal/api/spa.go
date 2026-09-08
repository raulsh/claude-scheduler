package api

import (
	"io/fs"
	"net/http"
	"path"
	"strings"
)

// spaHandler serves the embedded single-page app. Unknown paths fall back to
// index.html so client-side routes survive a page reload, while unknown
// paths under /api and /assets return a real 404 instead of HTML.
func (s *Server) spaHandler() http.Handler {
	dist, err := webFS()
	if err != nil {
		s.log.Error("embedded UI unavailable", "error", err)
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			writeError(w, http.StatusInternalServerError, "embedded UI unavailable")
		})
	}

	files := http.FileServerFS(dist)

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		clean := strings.TrimPrefix(path.Clean("/"+r.URL.Path), "/")
		if clean == "" {
			clean = "index.html"
		}

		if _, err := fs.Stat(dist, clean); err == nil {
			// Hashed build assets are immutable; the HTML shell must not be
			// cached or a redeploy would keep serving stale asset references.
			if strings.HasPrefix(clean, "assets/") {
				w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
			} else {
				w.Header().Set("Cache-Control", "no-cache")
			}
			files.ServeHTTP(w, r)
			return
		}

		// A miss under these prefixes is a genuine 404, not a client route.
		if strings.HasPrefix(clean, "api/") || strings.HasPrefix(clean, "assets/") {
			writeError(w, http.StatusNotFound, "not found")
			return
		}

		serveIndex(w, r, dist)
	})
}

func serveIndex(w http.ResponseWriter, r *http.Request, dist fs.FS) {
	index, err := fs.ReadFile(dist, "index.html")
	if err != nil {
		writeError(w, http.StatusInternalServerError, "index.html missing from the embedded UI")
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(index)
}
