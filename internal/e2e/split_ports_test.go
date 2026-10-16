package e2e

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"opback/internal/panel"
	"opback/internal/store"
)

// TestHandlersAreSeparated verifies that the front-end (web UI + admin API) and
// the back-end (agent API + installer) live on distinct listeners, so exposing
// the agent port does not expose the admin API and vice versa.
func TestHandlersAreSeparated(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "store.json"))
	if err != nil {
		t.Fatal(err)
	}
	srv := panel.New(st)
	srv.SetAgentPublicURL("http://agent.example.com:8081")

	web := httptest.NewServer(srv.WebHandler())
	defer web.Close()
	agent := httptest.NewServer(srv.AgentHandler())
	defer agent.Close()

	// Admin API is only reachable on the web listener.
	doRequest(t, http.DefaultClient, http.MethodGet, web.URL+"/api/me", nil, http.StatusUnauthorized)
	doRequest(t, http.DefaultClient, http.MethodGet, agent.URL+"/api/me", nil, http.StatusNotFound)

	// The installer and binary downloads are only on the agent listener.
	body := doRequest(t, http.DefaultClient, http.MethodGet, agent.URL+"/install.sh", nil, http.StatusOK)
	if !strings.Contains(string(body), "http://agent.example.com:8081") {
		t.Fatalf("install script did not embed the agent URL:\n%s", body)
	}
	doRequest(t, http.DefaultClient, http.MethodGet, web.URL+"/install.sh", nil, http.StatusNotFound)

	// Health is available on both.
	doRequest(t, http.DefaultClient, http.MethodGet, web.URL+"/healthz", nil, http.StatusOK)
	doRequest(t, http.DefaultClient, http.MethodGet, agent.URL+"/healthz", nil, http.StatusOK)
}

// TestInstallScriptSupportsInPlaceUpdate verifies the served installer can be
// re-run to upgrade the agent in place: it must always restart the service, and
// recover the node name from the existing unit when --name is omitted, so a bare
// `curl .../install.sh | sudo bash` acts as an update command.
func TestInstallScriptSupportsInPlaceUpdate(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "store.json"))
	if err != nil {
		t.Fatal(err)
	}
	srv := panel.New(st)
	srv.SetAgentPublicURL("http://agent.example.com:8081")

	agent := httptest.NewServer(srv.AgentHandler())
	defer agent.Close()

	body := string(doRequest(t, http.DefaultClient, http.MethodGet, agent.URL+"/install.sh", nil, http.StatusOK))
	if !strings.Contains(body, "systemctl restart") {
		t.Fatal("installer must restart the service so a re-install applies the new args")
	}
	if strings.Contains(body, "systemctl enable --now") {
		t.Fatal("installer must not rely on `systemctl enable --now` (it is a no-op on a running service)")
	}
	if !strings.Contains(body, `sed -n 's/.* -name`) {
		t.Fatal("installer must recover the node name from the existing unit for updates")
	}
	if !strings.Contains(body, "TMP_BIN") {
		t.Fatal("installer must replace the binary atomically to survive a running service")
	}
}

// TestMeReportsAgentURL checks that the front-end learns the agent endpoint so
// it can hand operators the correct install command.
func TestMeReportsAgentURL(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "store.json"))
	if err != nil {
		t.Fatal(err)
	}
	srv := panel.New(st)
	srv.SetAgentPublicURL("https://agent.example.com")

	ts := httptest.NewServer(srv.WebHandler())
	defer ts.Close()

	client := newCookieClient(t)
	postJSON(t, client, ts.URL+"/api/auth/login", map[string]string{"username": "admin", "password": "admin"}, http.StatusOK)

	var me struct {
		User     string `json:"user"`
		AgentURL string `json:"agent_url"`
		TwoFA    bool   `json:"twofa"`
	}
	if err := json.Unmarshal(getJSON(t, client, ts.URL+"/api/me", http.StatusOK), &me); err != nil {
		t.Fatal(err)
	}
	if me.User != "admin" {
		t.Fatalf("user = %q", me.User)
	}
	if me.AgentURL != "https://agent.example.com" {
		t.Fatalf("agent_url = %q", me.AgentURL)
	}
	if me.TwoFA {
		t.Fatal("twofa should be off by default")
	}
}
