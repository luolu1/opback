package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"opback/internal/agent"
	"opback/internal/panel"
	"opback/internal/store"
	"opback/internal/types"
)

// TestHTTPRegisterAndManualRun exercises the full network path: the agent
// registers over HTTP, an admin logs in, creates a job, triggers it, and the
// run result flows back to the panel.
func TestHTTPRegisterAndManualRun(t *testing.T) {
	mock := newMockOpenList()
	ol := httptest.NewServer(mock.handler())
	defer ol.Close()

	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "store.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := st.UpdateSettings(func(s *store.Settings) {
		s.OpenList = types.AgentConfig{BaseURL: ol.URL, Username: "admin", Password: "pw"}
		s.EnrollToken = "secret"
		s.SyncInterval = 1
	}); err != nil {
		t.Fatal(err)
	}
	panelTS := httptest.NewServer(panel.New(st).Handler())
	defer panelTS.Close()

	src := t.TempDir()
	mustWrite(t, filepath.Join(src, "index.html"), "<h1>hi</h1>")

	// Start the agent with a matching enroll token; it registers itself.
	ag, err := agent.New(agent.Config{
		PanelURL:    panelTS.URL,
		Name:        "web-1",
		EnrollToken: "secret",
		StateDir:    t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = ag.Run(ctx) }()

	// Admin session.
	client := newCookieClient(t)
	postJSON(t, client, panelTS.URL+"/api/auth/login", map[string]string{"username": "admin", "password": "admin"}, http.StatusOK)

	// Wait for the agent to appear.
	nodeID := waitForNode(t, client, panelTS.URL, "web-1")

	// Create a manual-only job for that node.
	jobBody := map[string]any{
		"name":     "web backup",
		"node_id":  nodeID,
		"source":   src,
		"dest":     "/srv/web-1",
		"interval": 0,
		"enabled":  true,
		"options":  map[string]any{"incremental": true, "overwrite": true, "concurrency": 2},
	}
	var job types.Job
	decodeInto(t, postJSON(t, client, panelTS.URL+"/api/jobs", jobBody, http.StatusOK), &job)

	// Trigger it.
	postJSON(t, client, panelTS.URL+"/api/jobs/"+job.ID+"/run", map[string]any{}, http.StatusOK)

	// Wait for a terminal run.
	run := waitForRun(t, client, panelTS.URL, job.ID)
	if run.Status != types.RunSuccess || run.FilesDone != 1 {
		t.Fatalf("run status=%s done=%d, want success/1", run.Status, run.FilesDone)
	}
	if got := string(mock.uploaded()["/srv/web-1/index.html"]); got != "<h1>hi</h1>" {
		t.Fatalf("unexpected uploaded content: %q", got)
	}
}

// ---- helpers -------------------------------------------------------------

func newCookieClient(t *testing.T) *http.Client {
	t.Helper()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	return &http.Client{Jar: jar, Timeout: 10 * time.Second}
}

func postJSON(t *testing.T, c *http.Client, url string, body any, wantStatus int) []byte {
	t.Helper()
	raw, _ := json.Marshal(body)
	resp, err := c.Post(url, "application/json", bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("POST %s: %v", url, err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != wantStatus {
		t.Fatalf("POST %s: status %d (want %d): %s", url, resp.StatusCode, wantStatus, data)
	}
	return data
}

func getJSON(t *testing.T, c *http.Client, url string, wantStatus int) []byte {
	t.Helper()
	resp, err := c.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != wantStatus {
		t.Fatalf("GET %s: status %d (want %d): %s", url, resp.StatusCode, wantStatus, data)
	}
	return data
}

func decodeInto(t *testing.T, raw []byte, v any) {
	t.Helper()
	if err := json.Unmarshal(raw, v); err != nil {
		t.Fatalf("decode: %v (%s)", err, raw)
	}
}

func waitForNode(t *testing.T, c *http.Client, base, name string) string {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		var nodes []types.Node
		decodeInto(t, getJSON(t, c, base+"/api/nodes", http.StatusOK), &nodes)
		for _, n := range nodes {
			if n.Name == name {
				return n.ID
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("node %q never registered", name)
	return ""
}

func waitForRun(t *testing.T, c *http.Client, base, jobID string) types.Run {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		var runs []types.Run
		decodeInto(t, getJSON(t, c, base+"/api/runs?job_id="+jobID, http.StatusOK), &runs)
		if len(runs) > 0 && runs[0].Status != types.RunRunning {
			return runs[0]
		}
		time.Sleep(300 * time.Millisecond)
	}
	t.Fatalf("no terminal run for job %s", jobID)
	return types.Run{}
}
