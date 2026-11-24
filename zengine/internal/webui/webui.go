// Package webui embeds the browser interface served by every engine process.
// It is plain HTML, CSS and JavaScript with no build step and no external
// requests: it only talks to the engine's control API.
package webui

import (
	"embed"
	"io/fs"
	"net/http"
)

//go:embed assets
var assets embed.FS

// Handler serves the web interface at "/". The page itself needs no token (it
// is static); every API call it makes does.
func Handler() http.Handler {
	sub, err := fs.Sub(assets, "assets")
	if err != nil {
		panic(err) // the embed directive guarantees the directory exists
	}
	files := http.FileServerFS(sub)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Cache-Control", "no-cache")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		// The UI loads nothing from elsewhere: lock the page down accordingly.
		h.Set("Content-Security-Policy", "default-src 'none'; script-src 'self'; style-src 'self'; img-src 'self' blob: data:; connect-src 'self'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'")
		files.ServeHTTP(w, r)
	})
}
