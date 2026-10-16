package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"opback/internal/agent"
	"opback/internal/panel"
	"opback/internal/store"
	"opback/internal/types"
)

// TestMultiOpenListRouting verifies that two jobs for the same node upload to
// two different OpenList servers according to their OpenListID, and that run
// records carry the enriched node/connection details.
func TestMultiOpenListRouting(t *testing.T) {
	mockA := newMockOpenList()
	olA := httptest.NewServer(mockA.handler())
	defer olA.Close()
	mockB := newMockOpenList()
	olB := httptest.NewServer(mockB.handler())
	defer olB.Close()

	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "store.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := st.UpdateSettings(func(s *store.Settings) { s.SyncInterval = 5 }); err != nil {
		t.Fatal(err)
	}
	panelTS := httptest.NewServer(panel.New(st).Handler())
	defer panelTS.Close()

	connA, err := st.CreateOpenList(types.OpenList{Name: "A", BaseURL: olA.URL, Username: "admin", Password: "pw"})
	if err != nil {
		t.Fatal(err)
	}
	connB, err := st.CreateOpenList(types.OpenList{Name: "B", BaseURL: olB.URL, Username: "admin", Password: "pw"})
	if err != nil {
		t.Fatal(err)
	}

	node, err := st.CreateNode("multi-node", "host", "linux", "amd64", "test")
	if err != nil {
		t.Fatal(err)
	}
	agentDir := t.TempDir()
	writeAgentState(t, agentDir, node.ID, node.Token)

	srcA := t.TempDir()
	mustWrite(t, filepath.Join(srcA, "a.txt"), "aaa")
	srcB := t.TempDir()
	mustWrite(t, filepath.Join(srcB, "b.txt"), "bbb")

	base := types.JobOptions{Incremental: true, Overwrite: true, Concurrency: 2}
	if _, err := st.CreateJob(types.Job{
		Name: "jobA", NodeID: node.ID, OpenListID: connA.ID, Source: srcA, Dest: "/dataA",
		Enabled: true, Interval: 0, Options: base,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateJob(types.Job{
		Name: "jobB", NodeID: node.ID, OpenListID: connB.ID, Source: srcB, Dest: "/dataB",
		Enabled: true, Interval: 0, Options: base,
	}); err != nil {
		t.Fatal(err)
	}

	ag, err := agent.New(agent.Config{PanelURL: panelTS.URL, Name: node.Name, StateDir: agentDir, Once: true})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := ag.Run(ctx); err != nil {
		t.Fatalf("run: %v", err)
	}

	if got := string(mockA.uploaded()["/dataA/a.txt"]); got != "aaa" {
		t.Fatalf("connection A missing /dataA/a.txt: %v", mockA.paths())
	}
	if got := string(mockB.uploaded()["/dataB/b.txt"]); got != "bbb" {
		t.Fatalf("connection B missing /dataB/b.txt: %v", mockB.paths())
	}
	if _, ok := mockA.uploaded()["/dataB/b.txt"]; ok {
		t.Fatalf("job B leaked onto connection A: %v", mockA.paths())
	}
	if _, ok := mockB.uploaded()["/dataA/a.txt"]; ok {
		t.Fatalf("job A leaked onto connection B: %v", mockB.paths())
	}

	// Every run should be enriched with the node and connection it belongs to.
	var runA, runB types.Run
	foundA, foundB := false, false
	for _, r := range st.ListRuns("", 0) {
		switch r.JobName {
		case "jobA":
			runA, foundA = r, true
		case "jobB":
			runB, foundB = r, true
		}
	}
	if !foundA || !foundB {
		t.Fatalf("missing run records: %+v", st.ListRuns("", 0))
	}
	if runA.NodeName != node.Name || runB.NodeName != node.Name {
		t.Fatalf("run node names = %q/%q, want %q", runA.NodeName, runB.NodeName, node.Name)
	}
	if runA.OpenListName != "A" || runB.OpenListName != "B" {
		t.Fatalf("run connection names = %q/%q, want A/B", runA.OpenListName, runB.OpenListName)
	}
	if runB.Source != srcB || runB.Dest != "/dataB" {
		t.Fatalf("run B source/dest = %q/%q, want %q//dataB", runB.Source, runB.Dest, srcB)
	}
	if runA.Source != srcA || runA.Dest != "/dataA" {
		t.Fatalf("run A source/dest = %q/%q, want %q//dataA", runA.Source, runA.Dest, srcA)
	}
}

// TestNodeUninstallFlow exercises the graceful "smooth uninstall" path: the
// panel drops the node's jobs, queues an uninstall command, keeps the node
// marked as uninstalling, and finally removes it when the agent confirms.
func TestNodeUninstallFlow(t *testing.T) {
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
		s.SyncInterval = 5
	}); err != nil {
		t.Fatal(err)
	}
	panelTS := httptest.NewServer(panel.New(st).Handler())
	defer panelTS.Close()

	node, err := st.CreateNode("victim", "host", "linux", "amd64", "test")
	if err != nil {
		t.Fatal(err)
	}
	src := t.TempDir()
	mustWrite(t, filepath.Join(src, "a.txt"), "a")
	job, err := st.CreateJob(types.Job{
		Name: "job", NodeID: node.ID, Source: src, Dest: "/backup",
		Enabled: true, Interval: 0,
		Options: types.JobOptions{Incremental: true, Overwrite: true, Concurrency: 2},
	})
	if err != nil {
		t.Fatal(err)
	}

	// Admin requests the graceful delete.
	client := newCookieClient(t)
	postJSON(t, client, panelTS.URL+"/api/auth/login", map[string]string{"username": "admin", "password": "admin"}, http.StatusOK)
	raw := doRequest(t, client, http.MethodDelete, panelTS.URL+"/api/nodes/"+node.ID, nil, http.StatusOK)
	var resp map[string]any
	decodeInto(t, raw, &resp)
	if resp["uninstalling"] != true {
		t.Fatalf("expected uninstalling=true, got %v", resp)
	}

	if got := len(st.ListJobs(node.ID)); got != 0 {
		t.Fatalf("node jobs not removed: %d remain", got)
	}
	if _, err := st.GetJob(job.ID); err == nil {
		t.Fatalf("job %s should have been deleted", job.ID)
	}
	kept, err := st.GetNode(node.ID)
	if err != nil {
		t.Fatalf("node should stay until confirmed: %v", err)
	}
	if !kept.Uninstalling {
		t.Fatalf("node should be marked uninstalling")
	}

	// The agent's next sync must carry the uninstall command.
	syncResp := agentSync(t, panelTS.URL, node.Token)
	found := false
	for _, c := range syncResp.Commands {
		if c.Action == "uninstall" {
			found = true
		}
	}
	if !found {
		t.Fatalf("agent did not receive uninstall command: %+v", syncResp.Commands)
	}

	// The agent confirms by calling the uninstall endpoint; the node disappears.
	req, _ := http.NewRequest(http.MethodPost, panelTS.URL+"/api/agent/uninstall", bytes.NewReader([]byte("{}")))
	req.Header.Set("X-Node-Token", node.Token)
	confirm, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	confirm.Body.Close()
	if confirm.StatusCode != http.StatusOK {
		t.Fatalf("uninstall confirm status = %d", confirm.StatusCode)
	}
	if _, err := st.GetNode(node.ID); err == nil {
		t.Fatalf("node should be gone after confirmation")
	}

	// Idempotent: calling again still succeeds.
	req2, _ := http.NewRequest(http.MethodPost, panelTS.URL+"/api/agent/uninstall", bytes.NewReader([]byte("{}")))
	req2.Header.Set("X-Node-Token", node.Token)
	again, err := http.DefaultClient.Do(req2)
	if err != nil {
		t.Fatal(err)
	}
	again.Body.Close()
	if again.StatusCode != http.StatusOK {
		t.Fatalf("repeat uninstall confirm status = %d", again.StatusCode)
	}
}

// TestOpenListDeleteGuard verifies that a connection in use cannot be deleted
// without force, and that a forced delete clears the references.
func TestOpenListDeleteGuard(t *testing.T) {
	mock := newMockOpenList()
	ol := httptest.NewServer(mock.handler())
	defer ol.Close()

	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "store.json"))
	if err != nil {
		t.Fatal(err)
	}
	panelTS := httptest.NewServer(panel.New(st).Handler())
	defer panelTS.Close()

	conn, err := st.CreateOpenList(types.OpenList{Name: "conn", BaseURL: ol.URL, Username: "admin", Password: "pw"})
	if err != nil {
		t.Fatal(err)
	}
	node, err := st.CreateNode("n", "host", "linux", "amd64", "test")
	if err != nil {
		t.Fatal(err)
	}
	job, err := st.CreateJob(types.Job{
		Name: "uses-conn", NodeID: node.ID, OpenListID: conn.ID, Source: "/tmp/x", Dest: "/b",
		Enabled: true, Options: types.JobOptions{Overwrite: true, Concurrency: 1},
	})
	if err != nil {
		t.Fatal(err)
	}

	client := newCookieClient(t)
	postJSON(t, client, panelTS.URL+"/api/auth/login", map[string]string{"username": "admin", "password": "admin"}, http.StatusOK)

	// Without force the delete is rejected with 409.
	doRequest(t, client, http.MethodDelete, panelTS.URL+"/api/openlists/"+conn.ID, nil, http.StatusConflict)

	// With force it succeeds and the job loses its reference.
	doRequest(t, client, http.MethodDelete, panelTS.URL+"/api/openlists/"+conn.ID+"?force=1", nil, http.StatusOK)
	if _, err := st.GetOpenList(conn.ID); err == nil {
		t.Fatalf("connection should be deleted")
	}
	updated, err := st.GetJob(job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if updated.OpenListID != "" {
		t.Fatalf("job openlist_id = %q, want empty", updated.OpenListID)
	}
}

// TestOpenListListMasksPassword ensures the list endpoint never returns the
// stored password but still reports whether one is set.
func TestOpenListListMasksPassword(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "store.json"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateOpenList(types.OpenList{Name: "c", BaseURL: "http://x", Username: "u", Password: "supersecret"}); err != nil {
		t.Fatal(err)
	}
	panelTS := httptest.NewServer(panel.New(st).Handler())
	defer panelTS.Close()

	client := newCookieClient(t)
	postJSON(t, client, panelTS.URL+"/api/auth/login", map[string]string{"username": "admin", "password": "admin"}, http.StatusOK)

	var list []map[string]any
	decodeInto(t, getJSON(t, client, panelTS.URL+"/api/openlists", http.StatusOK), &list)
	if len(list) != 1 {
		t.Fatalf("want 1 connection, got %d", len(list))
	}
	if pw, _ := list[0]["password"].(string); pw != "" {
		t.Fatalf("password leaked in list response: %q", pw)
	}
	if list[0]["password_set"] != true {
		t.Fatalf("password_set should be true")
	}
}

// ---- helpers -------------------------------------------------------------

func doRequest(t *testing.T, c *http.Client, method, url string, body []byte, wantStatus int) []byte {
	t.Helper()
	var rdr *bytes.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	} else {
		rdr = bytes.NewReader(nil)
	}
	req, err := http.NewRequest(method, url, rdr)
	if err != nil {
		t.Fatal(err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	defer resp.Body.Close()
	var buf bytes.Buffer
	if _, err := buf.ReadFrom(resp.Body); err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != wantStatus {
		t.Fatalf("%s %s: status %d (want %d): %s", method, url, resp.StatusCode, wantStatus, buf.String())
	}
	return buf.Bytes()
}

// agentSync performs a raw agent heartbeat with the given node token.
func agentSync(t *testing.T, base, token string) types.SyncResponse {
	t.Helper()
	raw, _ := json.Marshal(types.SyncRequest{Hostname: "host", OS: "linux", Arch: "amd64", AgentVersion: "test"})
	req, err := http.NewRequest(http.MethodPost, base+"/api/agent/sync", bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Node-Token", token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("agent sync status = %d", resp.StatusCode)
	}
	var out types.SyncResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	return out
}
