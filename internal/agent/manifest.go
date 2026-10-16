package agent

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
)

// manifestEntry records the size and modification time of the last successful
// upload of a file, keyed by its path relative to the source root.
type manifestEntry struct {
	Size  int64 `json:"size"`
	Mtime int64 `json:"mtime"` // Unix seconds
}

// manifest is the per-job incremental index. It lets the agent skip files that
// have not changed since the previous successful run.
type manifest struct {
	path string

	mu      sync.Mutex
	entries map[string]manifestEntry
	dirty   bool
}

func loadManifest(stateDir, jobID string) (*manifest, error) {
	dir := filepath.Join(stateDir, "manifests")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	m := &manifest{
		path:    filepath.Join(dir, jobID+".json"),
		entries: map[string]manifestEntry{},
	}
	raw, err := os.ReadFile(m.path)
	if err == nil && len(raw) > 0 {
		if err := json.Unmarshal(raw, &m.entries); err != nil {
			// A corrupt manifest is not fatal; treat it as empty and rebuild.
			m.entries = map[string]manifestEntry{}
		}
	}
	return m, nil
}

// matches reports whether rel is unchanged relative to the manifest.
func (m *manifest) matches(rel string, size, mtime int64) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	e, ok := m.entries[rel]
	return ok && e.Size == size && e.Mtime == mtime
}

// set records a successful upload.
func (m *manifest) set(rel string, size, mtime int64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.entries[rel] = manifestEntry{Size: size, Mtime: mtime}
	m.dirty = true
}

// save persists the manifest if it changed.
func (m *manifest) save() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.dirty {
		return nil
	}
	raw, err := json.Marshal(m.entries)
	if err != nil {
		return err
	}
	tmp := m.path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, m.path); err != nil {
		return err
	}
	m.dirty = false
	return nil
}
