// Package panel implements the central management panel: an HTTP API used by
// both the web UI and the agents, plus the embedded single-page UI.
package panel

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"opback/internal/store"
	"opback/internal/types"
)

// Server wires the HTTP routes to the store.
type Server struct {
	store *store.Store

	// agentDir is where prebuilt agent binaries live, served by the installer.
	agentDir string

	cmdMu    sync.Mutex
	commands map[string][]types.Command // nodeID -> pending commands

	// chatMu guards the recent-chat cache the Telegram bot fills in, which
	// backs the "获取 Chat ID" picker.
	chatMu   sync.Mutex
	chats    map[int64]tgChatInfo
	chatSeen []int64

	// Hardening / deployment knobs.
	cookieSecure   bool
	agentPublicURL string
	agentPort      int
	trustedOrigins []string
	version        string

	// login throttles password and 2FA attempts.
	login *rateLimiter
}

// New builds a panel server backed by st.
func New(st *store.Store) *Server {
	return &Server{
		store:    st,
		agentDir: "bin",
		commands: make(map[string][]types.Command),
		chats:    make(map[int64]tgChatInfo),
		login:    newRateLimiter(8, 5*time.Minute, 10*time.Minute),
	}
}

// SetAgentDir overrides the directory that holds prebuilt agent binaries
// (files named opback-agent-<os>-<arch>).
func (s *Server) SetAgentDir(dir string) {
	if strings.TrimSpace(dir) != "" {
		s.agentDir = dir
	}
}

// SetCookieSecure forces the Secure flag on the session cookie.
func (s *Server) SetCookieSecure(v bool) { s.cookieSecure = v }

// SetAgentPublicURL sets the externally reachable base URL of the agent
// listener, used when handing install commands to the operator.
func (s *Server) SetAgentPublicURL(u string) {
	s.agentPublicURL = strings.TrimRight(strings.TrimSpace(u), "/")
}

// SetAgentAddr records the agent listener address so the panel can derive a
// sensible default agent URL from the request host when no public URL is set.
func (s *Server) SetAgentAddr(addr string) {
	if _, port, err := net.SplitHostPort(addr); err == nil {
		if p, err := strconv.Atoi(port); err == nil {
			s.agentPort = p
		}
	}
}

// SetTrustedOrigins configures extra origins allowed to call the admin API
// (e.g. behind a reverse proxy whose public host differs from the request
// Host).
func (s *Server) SetTrustedOrigins(origins []string) {
	for _, o := range origins {
		if o = strings.TrimRight(strings.TrimSpace(o), "/"); o != "" {
			s.trustedOrigins = append(s.trustedOrigins, o)
		}
	}
}

// SetVersion records the build version shown in the UI.
func (s *Server) SetVersion(v string) { s.version = v }

// agentBaseURL returns the base URL agents should use, honouring the explicit
// public URL and otherwise deriving one from the request host plus the agent
// port.
func (s *Server) agentBaseURL(r *http.Request) string {
	if s.agentPublicURL != "" {
		return s.agentPublicURL
	}
	host := r.Host
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	scheme := "http"
	if s.cookieSecure {
		scheme = "https"
	}
	if s.agentPort > 0 {
		host = net.JoinHostPort(host, strconv.Itoa(s.agentPort))
	}
	return scheme + "://" + host
}

// Handler returns a single handler exposing every route. It is mainly useful
// for tests; production uses WebHandler + AgentHandler on separate ports.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	s.registerWeb(mux)
	s.registerAgent(mux)
	mux.HandleFunc("GET /healthz", s.handleHealth)
	return s.webMiddleware(mux)
}

// WebHandler serves the front-end: the embedded UI and the session-authenticated
// admin API.
func (s *Server) WebHandler() http.Handler {
	mux := http.NewServeMux()
	s.registerWeb(mux)
	mux.HandleFunc("GET /healthz", s.handleHealth)
	return s.webMiddleware(mux)
}

// AgentHandler serves the back-end: the agent registration/sync API and the
// installer used to bootstrap new agents.
func (s *Server) AgentHandler() http.Handler {
	mux := http.NewServeMux()
	s.registerAgent(mux)
	mux.HandleFunc("GET /healthz", s.handleHealth)
	return s.agentMiddleware(mux)
}

func (s *Server) registerWeb(mux *http.ServeMux) {
	// ---- public / auth ----
	mux.HandleFunc("POST /api/auth/login", s.handleLogin)
	mux.HandleFunc("POST /api/auth/login/2fa", s.handleLoginTwoFA)
	mux.HandleFunc("POST /api/auth/logout", s.handleLogout)

	// ---- admin API (session auth) ----
	mux.HandleFunc("GET /api/me", s.admin(s.handleMe))
	mux.HandleFunc("GET /api/overview", s.admin(s.handleOverview))
	mux.HandleFunc("GET /api/nodes", s.admin(s.handleListNodes))
	mux.HandleFunc("GET /api/nodes/{id}", s.admin(s.handleGetNode))
	mux.HandleFunc("DELETE /api/nodes/{id}", s.admin(s.handleDeleteNode))
	mux.HandleFunc("POST /api/nodes", s.admin(s.handleCreateNode))
	mux.HandleFunc("GET /api/jobs", s.admin(s.handleListJobs))
	mux.HandleFunc("POST /api/jobs", s.admin(s.handleCreateJob))
	mux.HandleFunc("GET /api/jobs/{id}", s.admin(s.handleGetJob))
	mux.HandleFunc("PUT /api/jobs/{id}", s.admin(s.handleUpdateJob))
	mux.HandleFunc("DELETE /api/jobs/{id}", s.admin(s.handleDeleteJob))
	mux.HandleFunc("POST /api/jobs/{id}/run", s.admin(s.handleRunJob))
	mux.HandleFunc("GET /api/runs", s.admin(s.handleListRuns))
	mux.HandleFunc("GET /api/runs/{id}", s.admin(s.handleGetRun))
	mux.HandleFunc("GET /api/settings", s.admin(s.handleGetSettings))
	mux.HandleFunc("PUT /api/settings", s.admin(s.handleUpdateSettings))
	mux.HandleFunc("POST /api/settings/password", s.admin(s.handleChangePassword))
	mux.HandleFunc("POST /api/settings/2fa/setup", s.admin(s.handleTwoFASetup))
	mux.HandleFunc("POST /api/settings/2fa/enable", s.admin(s.handleTwoFAEnable))
	mux.HandleFunc("POST /api/settings/2fa/disable", s.admin(s.handleTwoFADisable))
	mux.HandleFunc("POST /api/settings/2fa/recovery", s.admin(s.handleTwoFARecovery))
	mux.HandleFunc("POST /api/settings/telegram/test", s.admin(s.handleTestTelegram))
	mux.HandleFunc("GET /api/settings/telegram/chats", s.admin(s.handleListTelegramChats))

	// Task sync status + Telegram notification log.
	mux.HandleFunc("GET /api/sync-status", s.admin(s.handleSyncStatus))
	mux.HandleFunc("GET /api/notifications", s.admin(s.handleListNotifications))
	mux.HandleFunc("GET /api/openlists", s.admin(s.handleListOpenLists))
	mux.HandleFunc("POST /api/openlists", s.admin(s.handleCreateOpenList))
	mux.HandleFunc("GET /api/openlists/{id}", s.admin(s.handleGetOpenList))
	mux.HandleFunc("PUT /api/openlists/{id}", s.admin(s.handleUpdateOpenList))
	mux.HandleFunc("DELETE /api/openlists/{id}", s.admin(s.handleDeleteOpenList))
	mux.HandleFunc("POST /api/openlists/{id}/test", s.admin(s.handleTestOpenList))
	mux.HandleFunc("GET /api/openlist/list", s.admin(s.handleBrowseOpenList))

	// ---- embedded UI ----
	// The agent-only namespaces are explicitly refused here so exposing the
	// front-end port never leaks the installer or agent binaries (and so a
	// typo in an agent URL fails loudly instead of returning the SPA).
	mux.HandleFunc("/install.sh", http.NotFound)
	mux.HandleFunc("/downloads/", http.NotFound)
	mux.Handle("/", s.uiHandler())
}

func (s *Server) registerAgent(mux *http.ServeMux) {
	// ---- public agent bootstrap (installer + binary download) ----
	mux.HandleFunc("GET /install.sh", s.handleInstallScript)
	mux.HandleFunc("GET /downloads/agent/{os}/{arch}", s.handleAgentDownload)

	// ---- agent API (node-token auth) ----
	mux.HandleFunc("POST /api/agent/register", s.handleAgentRegister)
	mux.HandleFunc("POST /api/agent/sync", s.handleAgentSync)
	mux.HandleFunc("POST /api/agent/uninstall", s.handleAgentUninstall)
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeOK(w, map[string]any{"ok": true, "version": s.version})
}

// ---- middleware ----------------------------------------------------------

// admin wraps a handler with session authentication.
func (s *Server) admin(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.checkSession(r) {
			writeErr(w, http.StatusUnauthorized, "authentication required")
			return
		}
		next(w, r)
	}
}

func logMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Agent heartbeats are noisy; only log non-sync requests.
		if !strings.HasPrefix(r.URL.Path, "/api/agent/sync") && r.URL.Path != "/healthz" {
			fmt.Printf("%s %s %s\n", time.Now().Format("2006-01-02 15:04:05"), r.Method, r.URL.Path)
		}
		next.ServeHTTP(w, r)
	})
}

// ---- session cookies -----------------------------------------------------

const sessionCookie = "opback_session"

// token kinds embedded in signed values.
const (
	tokenSession   = "s"
	tokenChallenge = "c"
)

// signToken produces an opaque, tamper-evident value bound to the current
// session epoch. kind distinguishes session cookies from 2FA challenges.
func (s *Server) signToken(kind, subject string, exp time.Time) string {
	epoch := s.store.Settings().SessionEpoch
	payload := fmt.Sprintf("%s|%s|%d|%d", kind,
		base64.RawURLEncoding.EncodeToString([]byte(subject)), epoch, exp.Unix())
	mac := hmac.New(sha256.New, []byte(s.store.Settings().SessionSecret))
	mac.Write([]byte(payload))
	sig := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	return base64.RawURLEncoding.EncodeToString([]byte(payload)) + "." + sig
}

// verifyToken validates a signed value and returns its subject. It rejects
// values signed for a different kind or a stale session epoch.
func (s *Server) verifyToken(value, kind string) (string, bool) {
	parts := strings.SplitN(value, ".", 2)
	if len(parts) != 2 {
		return "", false
	}
	payloadRaw, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return "", false
	}
	set := s.store.Settings()
	mac := hmac.New(sha256.New, []byte(set.SessionSecret))
	mac.Write(payloadRaw)
	want := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	if !hmac.Equal([]byte(want), []byte(parts[1])) {
		return "", false
	}
	fields := strings.Split(string(payloadRaw), "|")
	if len(fields) != 4 || fields[0] != kind {
		return "", false
	}
	subjRaw, err := base64.RawURLEncoding.DecodeString(fields[1])
	if err != nil {
		return "", false
	}
	epoch, err := strconv.ParseInt(fields[2], 10, 64)
	if err != nil || epoch != set.SessionEpoch {
		return "", false
	}
	exp, err := strconv.ParseInt(fields[3], 10, 64)
	if err != nil || time.Now().Unix() >= exp {
		return "", false
	}
	return string(subjRaw), true
}

func (s *Server) signSession(user string, exp time.Time) string {
	return s.signToken(tokenSession, user, exp)
}

func (s *Server) verifySession(value string) bool {
	_, ok := s.verifyToken(value, tokenSession)
	return ok
}

// signChallenge issues a short-lived token that only permits completing the
// 2FA step of a login.
func (s *Server) signChallenge(exp time.Time) string {
	return s.signToken(tokenChallenge, "", exp)
}

func (s *Server) verifyChallenge(value string) bool {
	_, ok := s.verifyToken(value, tokenChallenge)
	return ok
}

func (s *Server) checkSession(r *http.Request) bool {
	// Allow either a cookie (web UI) or a bearer session token (scripts).
	if c, err := r.Cookie(sessionCookie); err == nil && s.verifySession(c.Value) {
		return true
	}
	if auth := r.Header.Get("Authorization"); auth != "" {
		return s.verifySession(auth)
	}
	return false
}

// ---- HTTP helpers --------------------------------------------------------

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeOK(w http.ResponseWriter, v any) {
	writeJSON(w, http.StatusOK, v)
}

func writeErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

// decodeJSON reads a JSON body into v, guarding against oversized payloads.
func decodeJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	defer r.Body.Close()
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	if err := dec.Decode(v); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return false
	}
	return true
}

// nodeOnline reports whether a node has heartbeated recently enough.
func nodeOnline(n types.Node, syncInterval int, now time.Time) bool {
	window := time.Duration(syncInterval) * time.Second * 4
	if window < 60*time.Second {
		window = 60 * time.Second
	}
	return now.Sub(n.LastSeen) < window
}
