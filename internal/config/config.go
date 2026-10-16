// Package config loads the panel's file-based configuration. It intentionally
// depends only on the Go standard library so the panel stays a single
// dependency-free binary.
//
// The front-end (web UI + admin API) and the agent-facing API can be bound to
// separate host/port pairs. A minimal config looks like:
//
//	{
//	  "web":   { "host": "0.0.0.0", "port": 8080 },
//	  "agent": { "host": "0.0.0.0", "port": 8081 }
//	}
package config

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
)

// Listen is a host/port pair. An empty Host means "all interfaces".
type Listen struct {
	Host string `json:"host"`
	Port int    `json:"port"`
}

// Addr renders the listen address in the form accepted by net.Listen. It
// returns an empty string when the pair is unset (both host and port zero), so
// callers can fall back to defaults.
func (l Listen) Addr() string {
	host := strings.TrimSpace(l.Host)
	if host == "" && l.Port == 0 {
		return ""
	}
	if l.Port < 0 || l.Port > 65535 {
		return ""
	}
	if host == "" {
		return ":" + strconv.Itoa(l.Port)
	}
	return net.JoinHostPort(host, strconv.Itoa(l.Port))
}

// OpenList carries optional bootstrap OpenList credentials.
type OpenList struct {
	URL      string `json:"url"`
	Username string `json:"username"`
	Password string `json:"password"`
}

// Config is the on-disk panel configuration. Zero values mean "not set" so the
// caller can layer a config file underneath CLI flags and environment
// variables.
type Config struct {
	// Web is the front-end listener (web UI + admin API).
	Web Listen `json:"web"`
	// Agent is the back-end listener (agent register/sync + installer).
	Agent Listen `json:"agent"`

	DataDir  string `json:"data_dir"`
	AgentDir string `json:"agent_dir"`

	// AgentPublicURL is the base URL agents should use to reach the agent
	// listener (e.g. https://agent.example.com). When empty the panel derives
	// it from the incoming request host plus the configured agent port.
	AgentPublicURL string `json:"agent_public_url"`

	// TrustedOrigins lists extra origins allowed to call the admin API. Useful
	// when the panel sits behind a reverse proxy whose Host differs from the
	// public Origin.
	TrustedOrigins []string `json:"trusted_origins"`

	// CookieSecure forces the Secure attribute on the session cookie. It is
	// enabled automatically when TLS is configured.
	CookieSecure bool `json:"cookie_secure"`

	// TLS, when both fields are set, serves the web listener over HTTPS.
	TLSCert string `json:"tls_cert"`
	TLSKey  string `json:"tls_key"`

	// EnrollToken is the shared secret agents must present when registering.
	// An empty value allows open registration (the panel logs a warning).
	EnrollToken string `json:"enroll_token"`

	// OpenList carries optional bootstrap OpenList credentials.
	OpenList OpenList `json:"openlist"`
}

// Load reads a JSON config file on top of the supplied base (typically the
// built-in defaults). A missing or empty file is not an error: the base is
// returned unchanged. Unknown fields are rejected so typos fail loudly.
func Load(path string, base Config) (Config, error) {
	cfg := base
	if strings.TrimSpace(path) == "" {
		return cfg, nil
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return cfg, nil
		}
		return cfg, err
	}
	if len(strings.TrimSpace(string(raw))) == 0 {
		return cfg, nil
	}
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&cfg); err != nil {
		return cfg, fmt.Errorf("解析配置文件 %s 失败: %w", path, err)
	}
	return cfg, nil
}

// Default returns an empty configuration. Callers apply their own defaults for
// any field left unset.
func Default() Config { return Config{} }
