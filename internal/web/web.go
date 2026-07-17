// Package web serves the compiled Vue SPA out of the binary.
//
// The assets are baked in with embed, so production runs one process and one file:
// there is no Node runtime, no separate static server, and nothing to keep in sync
// between the API and the UI it ships with.
package web

import (
	"embed"
	"io/fs"
	"net/http"
	"strings"
)

// dist is populated by `pnpm build` in web/ before `go build`. The placeholder file
// keeps the embed directive valid on a fresh checkout where the SPA has not been
// built yet — without it the Go build fails on a missing directory.
//
//go:embed all:dist
var dist embed.FS

// Handler serves the SPA: real files as themselves, everything else as index.html so
// that client-side routes survive a page refresh.
func Handler() http.Handler {
	sub, err := fs.Sub(dist, "dist")
	if err != nil {
		panic("web: embedded dist is malformed: " + err.Error())
	}
	files := http.FileServer(http.FS(sub))

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, "/")
		if path == "" {
			path = "index.html"
		}

		if _, err := fs.Stat(sub, path); err != nil {
			// Not a real asset → a client route (/containers/abc). Hand back the
			// shell and let the router work it out.
			r = r.Clone(r.Context())
			r.URL.Path = "/"
		}

		// Hashed assets are immutable; index.html must never be cached or a deploy
		// leaves people on the old bundle.
		if strings.HasPrefix(path, "assets/") {
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		} else {
			w.Header().Set("Cache-Control", "no-cache")
		}
		files.ServeHTTP(w, r)
	})
}
