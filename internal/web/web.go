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

// dist/app is populated by `pnpm build` in web/ before `go build`. dist/ itself holds a
// tracked .gitkeep so the embed directive stays valid on a fresh checkout where the SPA has
// not been built yet — without a file to match, the Go build fails. The .gitkeep lives in
// dist/, NOT in dist/app/, because vite wipes its output dir on every build (emptyOutDir):
// keeping the placeholder one level up is what stops each build from deleting it.
//
//go:embed all:dist
var dist embed.FS

// Handler serves the SPA: real files as themselves, everything else as index.html so
// that client-side routes survive a page refresh.
func Handler() http.Handler {
	sub, err := fs.Sub(dist, "dist/app")
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
