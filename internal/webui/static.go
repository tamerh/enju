package webui

import (
	"io/fs"
	"net/http"
)

// staticHandler serves files from the static FS. In production
// the FS is embedded (//go:embed); in dev mode the caller
// passes os.DirFS so edits show on save.
//
// Cache headers:
//
//   - dev → no-cache (forces every reload to re-fetch)
//   - prod → public, max-age=1y, immutable (paired with build-
//     sha-versioned URLs in a follow-up; for v1 the URLs are
//     un-versioned and the immutable header is fine because
//     content is embedded — a binary upgrade replaces both
//     URL and content)
func (s *Server) staticHandler(staticFS fs.FS) http.Handler {
	fileServer := http.FileServer(http.FS(staticFS))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.dev {
			w.Header().Set("Cache-Control", "no-cache")
		} else {
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		}
		fileServer.ServeHTTP(w, r)
	})
}

// pickStaticFS returns the static FS to serve from, picking
// embed in production and os.DirFS in dev (when the caller
// passed a non-nil dev FS via Config.Static).
//
// In production, sub-rooting strips the "static/" prefix so
// /static/app.css maps to app.css inside the FS.
func pickStaticFS(dev bool, override fs.FS) (fs.FS, error) {
	if dev && override != nil {
		return override, nil
	}
	sub, err := fs.Sub(embeddedStatic, "static")
	if err != nil {
		return nil, err
	}
	return sub, nil
}

// pickViewsFS does the same for templates. Same prod/dev
// split: embedded vs disk.
func pickViewsFS(dev bool, override fs.FS) (fs.FS, error) {
	if dev && override != nil {
		return override, nil
	}
	sub, err := fs.Sub(embeddedViews, "views")
	if err != nil {
		return nil, err
	}
	return sub, nil
}
