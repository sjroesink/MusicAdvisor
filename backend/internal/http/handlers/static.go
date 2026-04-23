package handlers

import (
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

// StaticSPA serves the built frontend from fsRoot. On any GET that doesn't
// map to an existing file it falls back to index.html, so client-side
// routing (deep links, refresh on a subroute) works without a 404.
//
// Returns nil if fsRoot is empty — callers should skip mounting in that
// case. If fsRoot is set but the directory is missing, the handler returns
// a visible error so operators notice the misconfiguration instead of
// silently showing an empty page.
func StaticSPA(fsRoot string, logger *slog.Logger) http.Handler {
	if fsRoot == "" {
		return nil
	}
	fsRoot = filepath.Clean(fsRoot)
	if info, err := os.Stat(fsRoot); err != nil || !info.IsDir() {
		logger.Warn("static: frontend path missing", "path", fsRoot, "err", err)
		return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "frontend assets not installed at "+fsRoot, http.StatusInternalServerError)
		})
	}
	fileServer := http.FileServer(http.Dir(fsRoot))
	indexPath := filepath.Join(fsRoot, "index.html")
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Never serve /api or /healthz through here — router mounts those
		// above, but a malformed request could still slip in. Guard anyway.
		if strings.HasPrefix(r.URL.Path, "/api/") || r.URL.Path == "/api" || r.URL.Path == "/healthz" {
			http.NotFound(w, r)
			return
		}
		// If the path matches a real file (e.g. /assets/index-xxx.js), serve it.
		p := filepath.Join(fsRoot, filepath.FromSlash(strings.TrimPrefix(r.URL.Path, "/")))
		if info, err := os.Stat(p); err == nil && !info.IsDir() {
			fileServer.ServeHTTP(w, r)
			return
		}
		// Otherwise fall back to index.html — SPA routing takes over from there.
		http.ServeFile(w, r, indexPath)
	})
}
