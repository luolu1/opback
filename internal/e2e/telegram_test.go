package e2e

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"opback/internal/panel"
	"opback/internal/store"
	"opback/internal/types"
)

func putJSON(t *testing.T, c *http.Client, url string, body any, wantStatus int) []byte {
	t.Helper()
	raw, _ := json.Marshal(body)
	req, err := http.NewRequest(http.MethodPut, url, bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.Do(req)
	if err != nil {
		t.Fatalf("PUT %s: %v", url, err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != wantStatus {
		t.Fatalf("PUT %s: status %d (want %d): %s", url, resp.StatusCode, wantStatus, data)
	}
	return data
}

// TestSyncStatusTelegramAndNotifications covers the panel-side wiring added for
// the Telegram integration: the aggregated sync status the bot and the Tasks
// page share, the Telegram settings round trip, the notification log, and the
// username-only account change.
func TestSyncStatusTelegramAndNotifications(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "store.json"))
	if err != nil {
		t.Fatal(err)
	}
	panelTS := httptest.NewServer(panel.New(st).Handler())
	defer panelTS.Close()

	node, err := st.CreateNode("web-1", "web-1", "linux", "amd64", "test")
	if err != nil {
		t.Fatal(err)
	}
	// Touch so the node counts as online.
	if err := st.TouchNode(node.ID, "web-1", "linux", "amd64", "test", "10.0.0.9"); err != nil {
		t.Fatal(err)
	}
	job, err := st.CreateJob(types.Job{
		Name: "docs", NodeID: node.ID, Source: "/srv/docs", Dest: "/backups/docs",
		Interval: 3600, Enabled: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	// A second, never-run job exercises the "never" bucket.
	if _, err := st.CreateJob(types.Job{
		Name: "media", NodeID: node.ID, Source: "/srv/media", Dest: "/backups/media",
		Interval: 3600, Enabled: true,
	}); err != nil {
		t.Fatal(err)
	}
	start := time.Now().Add(-3 * time.Minute).UTC()
	if err := st.UpsertRun(types.Run{
		ID: "run_ok", JobID: job.ID, JobName: job.Name, NodeID: node.ID, NodeName: node.Name,
		Status: types.RunSuccess, Trigger: types.TriggerSchedule,
		StartedAt: start, FinishedAt: start.Add(45 * time.Second),
		FilesDone: 5, BytesDone: 4096,
	}); err != nil {
		t.Fatal(err)
	}

	client := newCookieClient(t)
	postJSON(t, client, panelTS.URL+"/api/auth/login",
		map[string]string{"username": "admin", "password": "admin"}, http.StatusOK)

	// --- sync status --------------------------------------------------------
	var ss struct {
		NodesTotal  int `json:"nodes_total"`
		NodesOnline int `json:"nodes_online"`
		JobsTotal   int `json:"jobs_total"`
		Success     int `json:"success"`
		Failed      int `json:"failed"`
		Never       int `json:"never"`
		Items       []struct {
			JobID     string `json:"job_id"`
			HasRun    bool   `json:"has_run"`
			RunID     string `json:"run_id"`
			Status    string `json:"status"`
			BytesDone int64  `json:"bytes_done"`
		} `json:"items"`
	}
	decodeInto(t, getJSON(t, client, panelTS.URL+"/api/sync-status", http.StatusOK), &ss)
	if ss.NodesTotal != 1 || ss.NodesOnline != 1 {
		t.Fatalf("unexpected node counts: %+v", ss)
	}
	if ss.JobsTotal != 2 || ss.Success != 1 || ss.Failed != 0 || ss.Never != 1 {
		t.Fatalf("unexpected sync summary: %+v", ss)
	}
	if len(ss.Items) != 2 {
		t.Fatalf("want 2 sync items, got %d", len(ss.Items))
	}
	var found bool
	for _, it := range ss.Items {
		if it.JobID == job.ID {
			found = true
			if !it.HasRun || it.RunID != "run_ok" || it.Status != types.RunSuccess || it.BytesDone != 4096 {
				t.Fatalf("unexpected sync item for job: %+v", it)
			}
		}
	}
	if !found {
		t.Fatalf("job %s missing from sync status", job.ID)
	}

	// --- telegram test endpoint rejects a missing config --------------------
	postJSON(t, client, panelTS.URL+"/api/settings/telegram/test",
		map[string]string{}, http.StatusBadRequest)
	// --- chat discovery also needs a token ---------------------------------
	getJSON(t, client, panelTS.URL+"/api/settings/telegram/chats", http.StatusBadRequest)

	// --- telegram settings round trip --------------------------------------
	putJSON(t, client, panelTS.URL+"/api/settings", map[string]any{
		"telegram": map[string]any{
			"enabled":        true,
			"bot_token":      "123:ABC",
			"chat_id":        "999",
			"notify_success": true,
			"notify_failure": false,
		},
	}, http.StatusOK)

	var view struct {
		Telegram types.TelegramSettings `json:"telegram"`
	}
	decodeInto(t, getJSON(t, client, panelTS.URL+"/api/settings", http.StatusOK), &view)
	if !view.Telegram.Enabled || view.Telegram.BotToken != "123:ABC" || view.Telegram.ChatID != "999" {
		t.Fatalf("telegram settings not persisted in view: %+v", view.Telegram)
	}
	if !view.Telegram.NotifySuccess || view.Telegram.NotifyFailure {
		t.Fatalf("unexpected notify toggles: %+v", view.Telegram)
	}
	if got := st.Settings().Telegram; !got.Enabled || got.BotToken != "123:ABC" {
		t.Fatalf("telegram settings not persisted in store: %+v", got)
	}

	// --- notification log ---------------------------------------------------
	if err := st.AddNotification(types.Notification{
		Level: "success", Title: "备份成功", Text: "hello <b>world</b>",
		Channel: "telegram", OK: true,
	}); err != nil {
		t.Fatal(err)
	}
	var ntf []types.Notification
	decodeInto(t, getJSON(t, client, panelTS.URL+"/api/notifications?limit=10", http.StatusOK), &ntf)
	if len(ntf) != 1 || ntf[0].Title != "备份成功" || !ntf[0].OK {
		t.Fatalf("unexpected notification log: %+v", ntf)
	}

	// --- username-only change keeps the session valid ----------------------
	postJSON(t, client, panelTS.URL+"/api/settings/password",
		map[string]string{"username": "operator", "password": ""}, http.StatusOK)
	if got := st.Settings().AdminUser; got != "operator" {
		t.Fatalf("admin user = %q, want operator", got)
	}
	// The existing cookie must still authenticate after the rename.
	getJSON(t, client, panelTS.URL+"/api/me", http.StatusOK)
}
