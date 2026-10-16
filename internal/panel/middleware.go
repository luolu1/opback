package panel

import (
	"net/http"
	"net/url"
	"strings"
)

const contentSecurityPolicy = "default-src 'self'; " +
	"img-src 'self' data:; " +
	"style-src 'self' 'unsafe-inline'; " +
	"script-src 'self'; " +
	"connect-src 'self'; " +
	"font-src 'self'; " +
	"base-uri 'none'; " +
	"form-action 'self'; " +
	"frame-ancestors 'none'; " +
	"object-src 'none'"

// webMiddleware hardens the front-end listener: security headers, a
// cross-site request guard for state-changing API calls, and request logging.
func (s *Server) webMiddleware(next http.Handler) http.Handler {
	return s.secHeaders(s.originGuard(logMiddleware(next)))
}

// agentMiddleware hardens the agent listener. It only sets the security
// headers; agents are non-browser clients, so no CSRF guard is needed.
func (s *Server) agentMiddleware(next http.Handler) http.Handler {
	return s.secHeaders(logMiddleware(next))
}

func (s *Server) secHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Cross-Origin-Opener-Policy", "same-origin")
		h.Set("Content-Security-Policy", contentSecurityPolicy)
		if s.cookieSecure {
			h.Set("Strict-Transport-Security", "max-age=31536000; includeSubDomains")
		}
		next.ServeHTTP(w, r)
	})
}

func isMutating(method string) bool {
	switch method {
	case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
		return true
	}
	return false
}

// originGuard blocks cross-site state-changing API requests. It relies on the
// browser-provided Sec-Fetch-Site / Origin headers and complements the
// SameSite=Lax session cookie.
func (s *Server) originGuard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !isMutating(r.Method) || !strings.HasPrefix(r.URL.Path, "/api/") {
			next.ServeHTTP(w, r)
			return
		}
		if site := r.Header.Get("Sec-Fetch-Site"); site != "" && site != "same-origin" && site != "none" {
			writeErr(w, http.StatusForbidden, "已拒绝跨站请求")
			return
		}
		if origin := r.Header.Get("Origin"); origin != "" && !s.originAllowed(origin, r.Host) {
			writeErr(w, http.StatusForbidden, "已拒绝跨站请求")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// originAllowed reports whether origin may call the admin API from a page
// served by this panel.
func (s *Server) originAllowed(origin, host string) bool {
	u, err := url.Parse(origin)
	if err != nil || u.Host == "" {
		return false
	}
	origin = strings.TrimRight(origin, "/")
	for _, t := range s.trustedOrigins {
		if strings.EqualFold(t, origin) {
			return true
		}
	}
	return strings.EqualFold(u.Host, host)
}
