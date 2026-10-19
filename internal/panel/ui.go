package panel

import (
	"embed"
	"io/fs"
	"net/http"
	"strings"
)

//go:embed web
var webFS embed.FS

// uiHandler serves the embedded single-page UI. Unknown paths fall back to
// index.html so client-side routing works.
func (s *Server) uiHandler() http.Handler {
	sub, err := fs.Sub(webFS, "web")
	if err != nil {
		panic(err)
	}
	fileServer := http.FileServer(http.FS(sub))

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The UI is embedded in the panel binary, but browsers otherwise cache
		// app.js/style.css heuristically and can keep showing the previous
		// release after an upgrade. Revalidate on every use.
		w.Header().Set("Cache-Control", "no-cache")
		p := strings.TrimPrefix(r.URL.Path, "/")
		if p == "" {
			p = "index.html"
		}
		if _, err := fs.Stat(sub, p); err != nil {
			// Not a real file: hand off to the SPA entry point.
			r.URL.Path = "/"
		}
		fileServer.ServeHTTP(w, r)
	})
}
