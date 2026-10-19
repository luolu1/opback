package e2e

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"opback/internal/agent"
	"opback/internal/panel"
	"opback/internal/store"
	"opback/internal/types"
)

// mockOpenList is a small in-memory stand-in for the OpenList filesystem. It
// models files and directories so tests can exercise listing, moving and
// removal, not just uploads.
type mockOpenList struct {
	mu       sync.Mutex
	files    map[string][]byte // absolute, normalized file paths
	dirs     map[string]bool   // absolute, normalized directory paths
	putCount int
}

func newMockOpenList() *mockOpenList {
	return &mockOpenList{files: map[string][]byte{}, dirs: map[string]bool{"/": true}}
}

// olPath mirrors the client's path normalization.
func olPath(p string) string {
	if p == "" {
		return "/"
	}
	p = strings.ReplaceAll(p, "\\", "/")
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	return path.Clean(p)
}

func (m *mockOpenList) ensureDir(p string) {
	p = olPath(p)
	if p == "/" {
		return
	}
	cur := ""
	for _, s := range strings.Split(strings.Trim(p, "/"), "/") {
		cur += "/" + s
		m.dirs[cur] = true
	}
}

// children returns the immediate entries of p.
func (m *mockOpenList) children(p string) []map[string]any {
	p = olPath(p)
	var out []map[string]any
	for d := range m.dirs {
		if d != "/" && path.Dir(d) == p {
			out = append(out, map[string]any{"name": path.Base(d), "is_dir": true, "size": 0})
		}
	}
	for f, b := range m.files {
		if path.Dir(f) == p {
			out = append(out, map[string]any{"name": path.Base(f), "is_dir": false, "size": len(b)})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i]["name"].(string) < out[j]["name"].(string) })
	return out
}

func (m *mockOpenList) removeEntry(p string) {
	p = olPath(p)
	for k := range m.files {
		if k == p || strings.HasPrefix(k, p+"/") {
			delete(m.files, k)
		}
	}
	for k := range m.dirs {
		if k == p || strings.HasPrefix(k, p+"/") {
			delete(m.dirs, k)
		}
	}
}

func (m *mockOpenList) moveEntry(src, dst string) {
	src, dst = olPath(src), olPath(dst)
	var fkeys []string
	for k := range m.files {
		if k == src || strings.HasPrefix(k, src+"/") {
			fkeys = append(fkeys, k)
		}
	}
	for _, k := range fkeys {
		v := m.files[k]
		delete(m.files, k)
		m.files[dst+strings.TrimPrefix(k, src)] = v
	}
	var dkeys []string
	for k := range m.dirs {
		if k == src || strings.HasPrefix(k, src+"/") {
			dkeys = append(dkeys, k)
		}
	}
	for _, k := range dkeys {
		delete(m.dirs, k)
		m.dirs[dst+strings.TrimPrefix(k, src)] = true
	}
	m.ensureDir(path.Dir(dst))
}

// pruneEmpty removes empty directories strictly below p, innermost first.
func (m *mockOpenList) pruneEmpty(p string) {
	p = olPath(p)
	for changed := true; changed; {
		changed = false
		var victims []string
		for d := range m.dirs {
			if d == "/" || d == p || !strings.HasPrefix(d, p+"/") {
				continue
			}
			empty := true
			for f := range m.files {
				if strings.HasPrefix(f, d+"/") {
					empty = false
					break
				}
			}
			if empty {
				for sub := range m.dirs {
					if strings.HasPrefix(sub, d+"/") {
						empty = false
						break
					}
				}
			}
			if empty {
				victims = append(victims, d)
			}
		}
		for _, d := range victims {
			delete(m.dirs, d)
			changed = true
		}
	}
}

func (m *mockOpenList) handler() http.Handler {
	mux := http.NewServeMux()
	writeJSON := func(w http.ResponseWriter, v any) {
		_ = json.NewEncoder(w).Encode(v)
	}
	ok := func(w http.ResponseWriter) {
		writeJSON(w, map[string]any{"code": 200, "message": "success", "data": nil})
	}
	okData := func(w http.ResponseWriter, data any) {
		writeJSON(w, map[string]any{"code": 200, "message": "success", "data": data})
	}
	fail := func(w http.ResponseWriter, msg string) {
		w.WriteHeader(http.StatusNotFound)
		writeJSON(w, map[string]any{"code": 404, "message": msg, "data": nil})
	}

	mux.HandleFunc("/api/auth/login", func(w http.ResponseWriter, r *http.Request) {
		okData(w, map[string]string{"token": "test-token"})
	})
	mux.HandleFunc("/api/fs/mkdir", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Path string `json:"path"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		m.mu.Lock()
		m.ensureDir(req.Path)
		m.mu.Unlock()
		ok(w)
	})
	mux.HandleFunc("/api/fs/list", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Path string `json:"path"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		m.mu.Lock()
		p := olPath(req.Path)
		if !m.dirs[p] {
			m.mu.Unlock()
			fail(w, "object not found")
			return
		}
		content := m.children(p)
		m.mu.Unlock()
		okData(w, map[string]any{"content": content, "total": len(content), "write": true, "provider": "Local"})
	})
	mux.HandleFunc("/api/fs/get", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Path string `json:"path"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		m.mu.Lock()
		p := olPath(req.Path)
		isDir := m.dirs[p]
		b, isFile := m.files[p]
		m.mu.Unlock()
		switch {
		case isDir:
			okData(w, map[string]any{"name": path.Base(p), "is_dir": true})
		case isFile:
			okData(w, map[string]any{"name": path.Base(p), "is_dir": false, "size": len(b)})
		default:
			fail(w, "object not found")
		}
	})
	mux.HandleFunc("/api/fs/put", func(w http.ResponseWriter, r *http.Request) {
		raw := r.Header.Get("File-Path")
		decoded, err := url.PathUnescape(raw)
		if err != nil {
			decoded = raw
		}
		body, _ := io.ReadAll(r.Body)
		m.mu.Lock()
		p := olPath(decoded)
		m.ensureDir(path.Dir(p))
		m.files[p] = body
		m.putCount++
		m.mu.Unlock()
		ok(w)
	})
	mux.HandleFunc("/api/fs/move", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			SrcDir string   `json:"src_dir"`
			DstDir string   `json:"dst_dir"`
			Names  []string `json:"names"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		m.mu.Lock()
		for _, n := range req.Names {
			m.moveEntry(olPath(req.SrcDir+"/"+n), olPath(req.DstDir+"/"+n))
		}
		m.mu.Unlock()
		ok(w)
	})
	mux.HandleFunc("/api/fs/remove", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Dir   string   `json:"dir"`
			Names []string `json:"names"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		m.mu.Lock()
		for _, n := range req.Names {
			m.removeEntry(olPath(req.Dir + "/" + n))
		}
		m.mu.Unlock()
		ok(w)
	})
	mux.HandleFunc("/api/fs/remove_empty_directory", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			SrcDir string `json:"src_dir"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		m.mu.Lock()
		m.pruneEmpty(req.SrcDir)
		m.mu.Unlock()
		ok(w)
	})
	return mux
}

func (m *mockOpenList) puts() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.putCount
}

func (m *mockOpenList) uploaded() map[string][]byte {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make(map[string][]byte, len(m.files))
	for k, v := range m.files {
		out[k] = v
	}
	return out
}

// seed injects a file directly, bypassing the HTTP API (for test fixtures).
func (m *mockOpenList) seed(p, content string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	np := olPath(p)
	m.ensureDir(path.Dir(np))
	m.files[np] = []byte(content)
}

func (m *mockOpenList) has(p string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.files[olPath(p)]
	return ok
}

func (m *mockOpenList) hasDir(p string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.dirs[olPath(p)]
}

func (m *mockOpenList) paths() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]string, 0, len(m.files))
	for k := range m.files {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func TestEndToEndBackupAndIncremental(t *testing.T) {
	// 1. Mock OpenList.
	mock := newMockOpenList()
	ol := httptest.NewServer(mock.handler())
	defer ol.Close()

	// 2. Panel store + server.
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
	panel := panel.New(st).Handler()
	panelTS := httptest.NewServer(panel)
	defer panelTS.Close()

	// 3. A pre-provisioned node (so the job can reference it before the agent
	// starts) with matching agent state on disk.
	node, err := st.CreateNode("test-node", "host", "linux", "amd64", "test")
	if err != nil {
		t.Fatal(err)
	}
	agentDir := t.TempDir()
	writeAgentState(t, agentDir, node.ID, node.Token)

	// 4. Source data.
	src := t.TempDir()
	mustWrite(t, filepath.Join(src, "a.txt"), "hello")
	mustWrite(t, filepath.Join(src, "sub", "b.txt"), "world")
	mustWrite(t, filepath.Join(src, "sub", "skip.log"), "ignore me")

	// 5. Job: back up src to /backup on OpenList, excluding *.log.
	if _, err := st.CreateJob(types.Job{
		Name:     "test job",
		NodeID:   node.ID,
		Source:   src,
		Dest:     "/backup",
		Enabled:  true,
		Interval: 0,
		Options:  types.JobOptions{Incremental: true, Overwrite: true, Concurrency: 2, Exclude: []string{"*.log"}},
	}); err != nil {
		t.Fatal(err)
	}

	ag, err := agent.New(agent.Config{
		PanelURL: panelTS.URL,
		Name:     "test-node",
		StateDir: agentDir,
		Once:     true,
	})
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// 6. First run: both eligible files upload, the excluded one does not.
	if err := ag.Run(ctx); err != nil {
		t.Fatalf("first run: %v", err)
	}
	ups := mock.uploaded()
	if _, ok := ups["/backup/a.txt"]; !ok {
		t.Fatalf("expected /backup/a.txt to be uploaded, got %v", keys(ups))
	}
	if got := string(ups["/backup/sub/b.txt"]); got != "world" {
		t.Fatalf("expected /backup/sub/b.txt=world, got %q", got)
	}
	if _, ok := ups["/backup/sub/skip.log"]; ok {
		t.Fatalf("excluded file was uploaded: %v", keys(ups))
	}

	first := latestRun(t, st)
	if first.Status != types.RunSuccess || first.FilesDone != 2 {
		t.Fatalf("first run status=%s done=%d, want success/2", first.Status, first.FilesDone)
	}

	// 7. Second run: nothing changed, everything is skipped.
	before := mock.puts()
	if err := ag.Run(ctx); err != nil {
		t.Fatalf("second run: %v", err)
	}
	if mock.puts() != before {
		t.Fatalf("incremental run re-uploaded files: putCount %d -> %d", before, mock.puts())
	}
	second := latestRun(t, st)
	if second.FilesDone != 0 || second.FilesSkipped != 2 {
		t.Fatalf("second run done=%d skipped=%d, want 0/2", second.FilesDone, second.FilesSkipped)
	}
}

// TestHooksRunAroundBackup verifies that a pre command can materialise a
// consistent dump, that the dump is uploaded, and that the post command runs
// after the upload.
func TestHooksRunAroundBackup(t *testing.T) {
	mock, panelTS, st, node, agentDir := newTestEnv(t)

	src := t.TempDir()
	mustWrite(t, filepath.Join(src, "live.db"), "row-1")
	marker := filepath.Join(t.TempDir(), "post.marker")

	if _, err := st.CreateJob(types.Job{
		Name:     "db dump",
		NodeID:   node.ID,
		Source:   src,
		Dest:     "/backup",
		Enabled:  true,
		Interval: 0,
		Options: types.JobOptions{
			Overwrite:   true,
			Incremental: true,
			Concurrency: 1,
			PreCommand:  "cp live.db snapshot.sql",
			PostCommand: "printf done > " + marker,
			// Short timeout: the test must fail fast if hooks hang.
			HookTimeoutSeconds: 30,
		},
	}); err != nil {
		t.Fatal(err)
	}

	ag, err := agent.New(agent.Config{
		PanelURL: panelTS.URL,
		Name:     "test-node",
		StateDir: agentDir,
		Once:     true,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := ag.Run(ctx); err != nil {
		t.Fatalf("agent run: %v", err)
	}

	ups := mock.uploaded()
	if got := string(ups["/backup/snapshot.sql"]); got != "row-1" {
		t.Fatalf("pre-command dump was not uploaded: paths=%v", keys(ups))
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("post command did not run: %v", err)
	}
	if run := latestRun(t, st); run.Status != types.RunSuccess {
		t.Fatalf("run status = %s (message %q), want success", run.Status, run.Message)
	}
}

// TestPreHookFailureAbortsRun verifies that no files are uploaded when the pre
// command fails, and that the failure is reported to the panel.
func TestPreHookFailureAbortsRun(t *testing.T) {
	mock, panelTS, st, node, agentDir := newTestEnv(t)

	src := t.TempDir()
	mustWrite(t, filepath.Join(src, "a.txt"), "a")

	if _, err := st.CreateJob(types.Job{
		Name:     "broken hook",
		NodeID:   node.ID,
		Source:   src,
		Dest:     "/backup",
		Enabled:  true,
		Interval: 0,
		Options: types.JobOptions{
			Overwrite:          true,
			Incremental:        true,
			Concurrency:        1,
			PreCommand:         "echo nope >&2; exit 9",
			HookTimeoutSeconds: 30,
		},
	}); err != nil {
		t.Fatal(err)
	}

	ag, err := agent.New(agent.Config{
		PanelURL: panelTS.URL,
		Name:     "test-node",
		StateDir: agentDir,
		Once:     true,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := ag.Run(ctx); err != nil {
		t.Fatalf("agent run: %v", err)
	}

	if puts := mock.puts(); puts != 0 {
		t.Fatalf("pre hook failed but %d file(s) were uploaded", puts)
	}
	run := latestRun(t, st)
	if run.Status != types.RunFailed {
		t.Fatalf("run status = %s, want failed", run.Status)
	}
	if !strings.Contains(run.Message, "nope") {
		t.Fatalf("run message %q should include hook output", run.Message)
	}
}

// newTestEnv wires a mock OpenList, a panel store and a pre-provisioned node
// whose agent state is already on disk.
func newTestEnv(t *testing.T) (*mockOpenList, *httptest.Server, *store.Store, types.Node, string) {
	t.Helper()
	mock := newMockOpenList()
	ol := httptest.NewServer(mock.handler())
	t.Cleanup(ol.Close)

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
	t.Cleanup(panelTS.Close)

	node, err := st.CreateNode("test-node", "host", "linux", "amd64", "test")
	if err != nil {
		t.Fatal(err)
	}
	agentDir := t.TempDir()
	writeAgentState(t, agentDir, node.ID, node.Token)
	return mock, panelTS, st, node, agentDir
}

// TestDeleteExtraMovesToTrash verifies that files removed at the source are
// moved into the destination's trash folder and that now-empty directories are
// cleaned up.
func TestDeleteExtraMovesToTrash(t *testing.T) {
	mock, panelTS, st, node, agentDir := newTestEnv(t)

	src := t.TempDir()
	mustWrite(t, filepath.Join(src, "keep.txt"), "keep")
	mustWrite(t, filepath.Join(src, "sub", "gone.txt"), "gone")

	if _, err := st.CreateJob(types.Job{
		Name: "cleanup", NodeID: node.ID, Source: src, Dest: "/backup",
		Enabled: true, Interval: 0,
		Options: types.JobOptions{Incremental: true, Overwrite: true, Concurrency: 2, DeleteExtra: true, RetentionDays: 7},
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
		t.Fatalf("first run: %v", err)
	}
	if !mock.has("/backup/keep.txt") || !mock.has("/backup/sub/gone.txt") {
		t.Fatalf("initial uploads missing: %v", mock.paths())
	}

	// Remove a source file, then run again.
	if err := os.Remove(filepath.Join(src, "sub", "gone.txt")); err != nil {
		t.Fatal(err)
	}
	if err := ag.Run(ctx); err != nil {
		t.Fatalf("second run: %v", err)
	}

	if mock.has("/backup/sub/gone.txt") {
		t.Fatalf("extra file still at destination: %v", mock.paths())
	}
	found := false
	for _, p := range mock.paths() {
		if strings.HasPrefix(p, "/backup/.olb-trash/") && strings.HasSuffix(p, "/sub/gone.txt") {
			found = true
		}
	}
	if !found {
		t.Fatalf("gone.txt was not moved to trash: %v", mock.paths())
	}
	if mock.hasDir("/backup/sub") {
		t.Fatalf("empty directory /backup/sub was not pruned")
	}
}

// TestDeleteExtraWithoutRetention verifies that a zero retention window
// deletes extras outright instead of trashing them.
func TestDeleteExtraWithoutRetention(t *testing.T) {
	mock, panelTS, st, node, agentDir := newTestEnv(t)

	src := t.TempDir()
	mustWrite(t, filepath.Join(src, "a.txt"), "a")
	mustWrite(t, filepath.Join(src, "b.txt"), "b")

	if _, err := st.CreateJob(types.Job{
		Name: "cleanup", NodeID: node.ID, Source: src, Dest: "/backup",
		Enabled: true, Interval: 0,
		Options: types.JobOptions{Incremental: true, Overwrite: true, Concurrency: 2, DeleteExtra: true, RetentionDays: 0},
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
		t.Fatalf("first run: %v", err)
	}
	if err := os.Remove(filepath.Join(src, "b.txt")); err != nil {
		t.Fatal(err)
	}
	if err := ag.Run(ctx); err != nil {
		t.Fatalf("second run: %v", err)
	}

	if mock.has("/backup/b.txt") {
		t.Fatalf("extra file was not deleted: %v", mock.paths())
	}
	for _, p := range mock.paths() {
		if strings.Contains(p, "/.olb-trash/") {
			t.Fatalf("unexpected trash entry with zero retention: %s", p)
		}
	}
}

// TestTrashRetentionPrunesExpiredSnapshots verifies that snapshots older than
// the retention window are purged while fresh ones survive.
func TestTrashRetentionPrunesExpiredSnapshots(t *testing.T) {
	mock, panelTS, st, node, agentDir := newTestEnv(t)

	src := t.TempDir()
	mustWrite(t, filepath.Join(src, "keep.txt"), "keep")

	if _, err := st.CreateJob(types.Job{
		Name: "cleanup", NodeID: node.ID, Source: src, Dest: "/backup",
		Enabled: true, Interval: 0,
		Options: types.JobOptions{Incremental: true, Overwrite: true, Concurrency: 2, DeleteExtra: true, RetentionDays: 7},
	}); err != nil {
		t.Fatal(err)
	}

	const old = "20200101T000000Z"
	fresh := time.Now().UTC().Format("20060102T150405Z")
	mock.seed("/backup/.olb-trash/"+old+"/old.txt", "x")
	mock.seed("/backup/.olb-trash/"+fresh+"/new.txt", "y")

	ag, err := agent.New(agent.Config{PanelURL: panelTS.URL, Name: node.Name, StateDir: agentDir, Once: true})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := ag.Run(ctx); err != nil {
		t.Fatalf("run: %v", err)
	}

	if mock.has("/backup/.olb-trash/" + old + "/old.txt") {
		t.Fatalf("expired snapshot was not purged: %v", mock.paths())
	}
	if !mock.has("/backup/.olb-trash/" + fresh + "/new.txt") {
		t.Fatalf("fresh snapshot was wrongly purged: %v", mock.paths())
	}
}

// latestRun returns the most recently started run recorded by the panel.
func latestRun(t *testing.T, st *store.Store) types.Run {
	t.Helper()
	runs := st.ListRuns("", 10)
	if len(runs) == 0 {
		t.Fatal("no runs recorded")
	}
	return runs[0]
}

func writeAgentState(t *testing.T, dir, nodeID, token string) {
	t.Helper()
	raw, _ := json.Marshal(map[string]any{
		"node_id":  nodeID,
		"token":    token,
		"last_run": map[string]string{},
	})
	if err := os.WriteFile(filepath.Join(dir, "state.json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
}

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func keys(m map[string][]byte) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
