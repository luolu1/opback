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
