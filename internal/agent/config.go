package agent

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Config is the agent's static configuration, supplied via flags or a file.
type Config struct {
	// PanelURL is the base URL of the management panel, e.g.
	// http://panel.example.com:8080.
	PanelURL string
	// EnrollToken must match the panel's enroll token when one is configured.
	EnrollToken string
	// Name is the display name of this node. Defaults to the hostname.
	Name string
	// StateDir holds the token, last-run times and incremental manifests.
	StateDir string
	// ServiceName is the systemd unit the agent runs as, used when removing
	// itself on a panel-issued uninstall. Defaults to opback-agent.
	ServiceName string
	// Once runs every enabled job a single time, then exits.
	Once bool
}

// state is the mutable agent state persisted between restarts.
type state struct {
	NodeID  string               `json:"node_id"`
	Token   string               `json:"token"`
	LastRun map[string]time.Time `json:"last_run"`
}

type stateStore struct {
	path string

	mu sync.Mutex
	s  state
}

func loadState(dir string) (*stateStore, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	ss := &stateStore{
		path: filepath.Join(dir, "state.json"),
		s:    state{LastRun: map[string]time.Time{}},
	}
	raw, err := os.ReadFile(ss.path)
	if err == nil && len(raw) > 0 {
		if err := json.Unmarshal(raw, &ss.s); err != nil {
			return nil, err
		}
	}
	if ss.s.LastRun == nil {
		ss.s.LastRun = map[string]time.Time{}
	}
	return ss, nil
}

func (ss *stateStore) saveLocked() error {
	raw, err := json.MarshalIndent(&ss.s, "", "  ")
	if err != nil {
		return err
	}
	tmp := ss.path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, ss.path)
}

func (ss *stateStore) credentials() (nodeID, token string) {
	ss.mu.Lock()
	defer ss.mu.Unlock()
	return ss.s.NodeID, ss.s.Token
}

func (ss *stateStore) setCredentials(nodeID, token string) error {
	ss.mu.Lock()
	defer ss.mu.Unlock()
	ss.s.NodeID = nodeID
	ss.s.Token = token
	return ss.saveLocked()
}

func (ss *stateStore) clearCredentials() error {
	return ss.setCredentials("", "")
}

func (ss *stateStore) lastRun(jobID string) time.Time {
	ss.mu.Lock()
	defer ss.mu.Unlock()
	return ss.s.LastRun[jobID]
}

func (ss *stateStore) setLastRun(jobID string, t time.Time) {
	ss.mu.Lock()
	defer ss.mu.Unlock()
	ss.s.LastRun[jobID] = t
	_ = ss.saveLocked()
}
