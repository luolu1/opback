// Package store implements the panel's persistence layer. It keeps all state
// in a single JSON file that is rewritten atomically, which is plenty for the
// scale a self-hosted backup panel operates at and keeps the binary
// dependency-free.
package store

import (
	"crypto/hmac"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"opback/internal/types"
)

// ErrNotFound is returned by getters when an entity does not exist.
var ErrNotFound = errors.New("not found")

// ErrConflict is returned when creating an entity whose unique key is taken.
var ErrConflict = errors.New("already exists")

// Settings holds panel-wide configuration.
type Settings struct {
	AdminUser     string                 `json:"admin_user"`
	AdminSalt     string                 `json:"admin_salt"`
	AdminPassHash string                 `json:"admin_pass_hash"`
	SessionSecret string                 `json:"session_secret"`
	EnrollToken   string                 `json:"enroll_token"`
	OpenList      types.AgentConfig      `json:"openlist"`
	Telegram      types.TelegramSettings `json:"telegram"`
	SyncInterval  int                    `json:"sync_interval"`
	MaxRuns       int                    `json:"max_runs"`

	// TwoFAEnabled turns on TOTP second-factor authentication for the admin
	// account. TwoFASecret holds the base32 seed once enabled; TwoFAPending
	// holds a not-yet-confirmed seed during enrolment.
	TwoFAEnabled bool   `json:"twofa_enabled,omitempty"`
	TwoFASecret  string `json:"twofa_secret,omitempty"`
	TwoFAPending string `json:"twofa_pending,omitempty"`
	// RecoveryHashes holds one-way hashes (HMAC-SHA256 keyed by the session
	// secret) of the one-time recovery codes issued when 2FA was enabled.
	RecoveryHashes []string `json:"recovery_hashes,omitempty"`
	// SessionEpoch is bumped whenever the admin credentials change; signed
	// session cookies carry the epoch so old sessions stop working.
	SessionEpoch int64 `json:"session_epoch,omitempty"`
}

type data struct {
	Nodes         []types.Node         `json:"nodes"`
	Jobs          []types.Job          `json:"jobs"`
	Runs          []types.Run          `json:"runs"`
	OpenLists     []types.OpenList     `json:"openlists,omitempty"`
	Notifications []types.Notification `json:"notifications,omitempty"`
	Settings      Settings             `json:"settings"`
}

// Store is a concurrency-safe JSON document store.
type Store struct {
	path string

	mu   sync.Mutex
	data data
}

// Open loads (or initialises) the store at path.
func Open(path string) (*Store, error) {
	s := &Store{path: path}
	raw, err := os.ReadFile(path)
	switch {
	case err == nil:
		if len(raw) > 0 {
			if err := json.Unmarshal(raw, &s.data); err != nil {
				return nil, err
			}
		}
	case os.IsNotExist(err):
		// fresh store
	default:
		return nil, err
	}

	if err := s.initSettings(); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *Store) initSettings() error {
	d := &s.data
	changed := false
	if d.Settings.SessionSecret == "" {
		d.Settings.SessionSecret = randomHex(32)
		changed = true
	}
	if d.Settings.SyncInterval <= 0 {
		d.Settings.SyncInterval = 15
		changed = true
	}
	if d.Settings.MaxRuns <= 0 {
		d.Settings.MaxRuns = 1000
		changed = true
	}
	if d.Settings.AdminUser == "" {
		d.Settings.AdminUser = "admin"
		d.Settings.AdminSalt = randomHex(16)
		d.Settings.AdminPassHash = hashPassword("admin", d.Settings.AdminSalt)
		changed = true
	}
	// Migrate a single legacy OpenList connection (settings.openlist) into the
	// multi-connection model: create a "默认连接" entry and point any jobs that
	// do not reference one at it.
	if d.Settings.OpenList.BaseURL != "" {
		id := ""
		for _, o := range d.OpenLists {
			if o.BaseURL == d.Settings.OpenList.BaseURL && o.Username == d.Settings.OpenList.Username {
				id = o.ID
			}
		}
		if id == "" {
			id = "ol_" + randomHex(8)
			d.OpenLists = append(d.OpenLists, types.OpenList{
				ID:        id,
				Name:      "默认连接",
				BaseURL:   d.Settings.OpenList.BaseURL,
				Username:  d.Settings.OpenList.Username,
				Password:  d.Settings.OpenList.Password,
				Status:    "unknown",
				CreatedAt: time.Now().UTC(),
			})
		}
		for i := range d.Jobs {
			if d.Jobs[i].OpenListID == "" {
				d.Jobs[i].OpenListID = id
				changed = true
			}
		}
		// Keep settings.OpenList populated as a fallback for jobs that still
		// carry no OpenListID (e.g. legacy data written after startup).
		changed = true
	}
	if changed {
		return s.saveLocked()
	}
	return nil
}

// ---- persistence helpers -------------------------------------------------

// saveLocked writes the document atomically. Callers must hold s.mu.
func (s *Store) saveLocked() error {
	raw, err := json.MarshalIndent(&s.data, "", "  ")
	if err != nil {
		return err
	}
	dir := filepath.Dir(s.path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".store-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)

	if _, err := tmp.Write(raw); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	// The document holds secrets (session key, hashed credentials, OpenList and
	// Telegram tokens); keep it readable only by the owner.
	if err := os.Chmod(tmpName, 0o600); err != nil {
		return err
	}
	return os.Rename(tmpName, s.path)
}

// update runs fn while holding the lock and persists if fn reports a change.
func (s *Store) update(fn func(d *data) (bool, error)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	changed, err := fn(&s.data)
	if err != nil {
		return err
	}
	if changed {
		return s.saveLocked()
	}
	return nil
}

// ---- settings ------------------------------------------------------------

// Settings returns a copy of the current settings.
func (s *Store) Settings() Settings {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.data.Settings
}

// UpdateSettings mutates settings via fn and persists the result.
func (s *Store) UpdateSettings(fn func(*Settings)) error {
	return s.update(func(d *data) (bool, error) {
		fn(&d.Settings)
		return true, nil
	})
}

// SetAdminPassword stores a new password hash for the admin account and bumps
// the session epoch so any existing sessions are invalidated.
func (s *Store) SetAdminPassword(user, password string) error {
	return s.update(func(d *data) (bool, error) {
		salt := randomHex(16)
		d.Settings.AdminUser = user
		d.Settings.AdminSalt = salt
		d.Settings.AdminPassHash = hashPassword(password, salt)
		d.Settings.SessionEpoch++
		return true, nil
	})
}

// SetAdminUser renames the admin account without touching its credentials.
// Sessions are not invalidated: the session payload does not authorise by
// name, so a rename grants no extra privileges.
func (s *Store) SetAdminUser(user string) error {
	return s.update(func(d *data) (bool, error) {
		d.Settings.AdminUser = user
		return true, nil
	})
}

// BumpSessionEpoch invalidates every outstanding session.
func (s *Store) BumpSessionEpoch() error {
	return s.update(func(d *data) (bool, error) {
		d.Settings.SessionEpoch++
		return true, nil
	})
}

// CheckAdmin verifies the supplied credentials. It transparently upgrades
// legacy hashes to the current PBKDF2 scheme on a successful login.
func (s *Store) CheckAdmin(user, password string) bool {
	set := s.Settings()
	// Always run the KDF so a wrong username does not short-circuit the work.
	okPass := verifyPasswordHash(password, set.AdminSalt, set.AdminPassHash)
	okUser := subtle.ConstantTimeCompare([]byte(user), []byte(set.AdminUser)) == 1
	if !okUser || !okPass {
		return false
	}
	if isLegacyHash(set.AdminPassHash) {
		salt := randomHex(16)
		_ = s.UpdateSettings(func(st *Settings) {
			st.AdminSalt = salt
			st.AdminPassHash = hashPassword(password, salt)
		})
	}
	return true
}

// HashRecoveryCode returns the stored representation of a recovery code,
// keyed by the session secret so a leaked store without the secret is not
// enough to forge codes.
func (s *Store) HashRecoveryCode(code string) string {
	secret := s.Settings().SessionSecret
	return hmacHex(secret, normalizeRecoveryCode(code))
}

// SetTwoFAPending stores an unconfirmed TOTP seed during enrolment.
func (s *Store) SetTwoFAPending(secret string) error {
	return s.update(func(d *data) (bool, error) {
		d.Settings.TwoFAPending = secret
		return true, nil
	})
}

// EnableTwoFA activates TOTP with the given seed and recovery hashes.
func (s *Store) EnableTwoFA(secret string, recoveryHashes []string) error {
	return s.update(func(d *data) (bool, error) {
		d.Settings.TwoFAEnabled = true
		d.Settings.TwoFASecret = secret
		d.Settings.TwoFAPending = ""
		d.Settings.RecoveryHashes = recoveryHashes
		d.Settings.SessionEpoch++
		return true, nil
	})
}

// DisableTwoFA removes the TOTP seed and recovery codes.
func (s *Store) DisableTwoFA() error {
	return s.update(func(d *data) (bool, error) {
		d.Settings.TwoFAEnabled = false
		d.Settings.TwoFASecret = ""
		d.Settings.TwoFAPending = ""
		d.Settings.RecoveryHashes = nil
		d.Settings.SessionEpoch++
		return true, nil
	})
}

// ReplaceRecoveryHashes swaps in a new set of recovery codes.
func (s *Store) ReplaceRecoveryHashes(hashes []string) error {
	return s.update(func(d *data) (bool, error) {
		d.Settings.RecoveryHashes = hashes
		return true, nil
	})
}

// ConsumeRecoveryHash removes hash and reports whether it was present.
func (s *Store) ConsumeRecoveryHash(hash string) bool {
	found := false
	_ = s.update(func(d *data) (bool, error) {
		kept := d.Settings.RecoveryHashes[:0]
		for _, h := range d.Settings.RecoveryHashes {
			if !found && subtle.ConstantTimeCompare([]byte(h), []byte(hash)) == 1 {
				found = true
				continue
			}
			kept = append(kept, h)
		}
		d.Settings.RecoveryHashes = kept
		return found, nil
	})
	return found
}

// hashPassword derives a PBKDF2-HMAC-SHA256 hash. The stored form is
// "pbkdf2$<iterations>$<hex>" so the parameters travel with the hash and can
// be raised later without breaking existing logins.
func hashPassword(password, salt string) string {
	key, err := pbkdf2.Key(sha256.New, password, []byte(salt), pbkdf2Iterations, 32)
	if err != nil {
		return ""
	}
	return fmt.Sprintf("pbkdf2$%d$%s", pbkdf2Iterations, hex.EncodeToString(key))
}

func legacyHash(password, salt string) string {
	sum := sha256.Sum256([]byte(salt + "\x00" + password))
	return hex.EncodeToString(sum[:])
}

func isLegacyHash(stored string) bool {
	return stored != "" && !strings.HasPrefix(stored, "pbkdf2$")
}

// verifyPasswordHash checks password against stored, supporting both the
// current PBKDF2 format and the legacy salted-SHA256 format.
func verifyPasswordHash(password, salt, stored string) bool {
	if strings.HasPrefix(stored, "pbkdf2$") {
		parts := strings.SplitN(stored, "$", 3)
		if len(parts) != 3 {
			return false
		}
		iter, err := strconv.Atoi(parts[1])
		if err != nil || iter <= 0 {
			return false
		}
		key, err := pbkdf2.Key(sha256.New, password, []byte(salt), iter, 32)
		if err != nil {
			return false
		}
		return subtle.ConstantTimeCompare([]byte(hex.EncodeToString(key)), []byte(parts[2])) == 1
	}
	if stored == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(legacyHash(password, salt)), []byte(stored)) == 1
}

// ---- nodes ---------------------------------------------------------------

// ListNodes returns all nodes, newest first.
func (s *Store) ListNodes() []types.Node {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]types.Node, len(s.data.Nodes))
	copy(out, s.data.Nodes)
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	return out
}

// GetNode looks up a node by ID.
func (s *Store) GetNode(id string) (types.Node, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, n := range s.data.Nodes {
		if n.ID == id {
			return n, nil
		}
	}
	return types.Node{}, ErrNotFound
}

// NodeByToken finds the node owning an agent token. The comparison is
// constant-time so token bytes cannot be probed through timing.
func (s *Store) NodeByToken(token string) (types.Node, error) {
	if token == "" {
		return types.Node{}, ErrNotFound
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var found types.Node
	matched := false
	for _, n := range s.data.Nodes {
		if subtle.ConstantTimeCompare([]byte(n.Token), []byte(token)) == 1 {
			found = n
			matched = true
		}
	}
	if !matched {
		return types.Node{}, ErrNotFound
	}
	return found, nil
}

// CreateNode registers a new node, allocating an ID and token.
func (s *Store) CreateNode(name, hostname, osName, arch, version string) (types.Node, error) {
	n := types.Node{
		ID:           "node_" + randomHex(8),
		Name:         name,
		Token:        randomHex(24),
		Hostname:     hostname,
		OS:           osName,
		Arch:         arch,
		AgentVersion: version,
		// LastSeen stays zero until the agent actually talks to the panel, so a
		// freshly created node is reported as offline.
		CreatedAt: time.Now().UTC(),
	}
	err := s.update(func(d *data) (bool, error) {
		if n.Name == "" {
			n.Name = hostname
		}
		d.Nodes = append(d.Nodes, n)
		return true, nil
	})
	if err != nil {
		return types.Node{}, err
	}
	return n, nil
}

// UpdateNode applies fn to the node with the given ID.
func (s *Store) UpdateNode(id string, fn func(*types.Node)) error {
	return s.update(func(d *data) (bool, error) {
		for i := range d.Nodes {
			if d.Nodes[i].ID == id {
				fn(&d.Nodes[i])
				return true, nil
			}
		}
		return false, ErrNotFound
	})
}

// TouchNode records an agent heartbeat.
func (s *Store) TouchNode(id, hostname, osName, arch, version, ip string) error {
	return s.UpdateNode(id, func(n *types.Node) {
		n.LastSeen = time.Now().UTC()
		if hostname != "" {
			n.Hostname = hostname
		}
		if osName != "" {
			n.OS = osName
		}
		if arch != "" {
			n.Arch = arch
		}
		if version != "" {
			n.AgentVersion = version
		}
		if ip != "" {
			n.IP = ip
		}
	})
}

// SetNodeIP records the peer address a node last connected from, without
// touching its heartbeat timestamp. Used at registration time.
func (s *Store) SetNodeIP(id, ip string) error {
	if ip == "" {
		return nil
	}
	return s.UpdateNode(id, func(n *types.Node) { n.IP = ip })
}

// DeleteNode removes a node and every job belonging to it.
func (s *Store) DeleteNode(id string) error {
	return s.update(func(d *data) (bool, error) {
		nodes := d.Nodes[:0]
		found := false
		for _, n := range d.Nodes {
			if n.ID == id {
				found = true
				continue
			}
			nodes = append(nodes, n)
		}
		if !found {
			return false, ErrNotFound
		}
		d.Nodes = nodes

		jobs := d.Jobs[:0]
		for _, j := range d.Jobs {
			if j.NodeID != id {
				jobs = append(jobs, j)
			}
		}
		d.Jobs = jobs
		return true, nil
	})
}

// DeleteJobsByNode removes every job belonging to a node while keeping the
// node itself (used when starting a graceful remote uninstall).
func (s *Store) DeleteJobsByNode(id string) error {
	return s.update(func(d *data) (bool, error) {
		jobs := d.Jobs[:0]
		for _, j := range d.Jobs {
			if j.NodeID != id {
				jobs = append(jobs, j)
			}
		}
		d.Jobs = jobs
		return true, nil
	})
}

// SetNodeUninstalling flips the node's uninstall flag.
func (s *Store) SetNodeUninstalling(id string, v bool) error {
	return s.UpdateNode(id, func(n *types.Node) { n.Uninstalling = v })
}

// ---- OpenList connections ------------------------------------------------

// ListOpenLists returns all configured OpenList connections, oldest first.
func (s *Store) ListOpenLists() []types.OpenList {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]types.OpenList, len(s.data.OpenLists))
	copy(out, s.data.OpenLists)
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	return out
}

// GetOpenList looks up a connection by ID.
func (s *Store) GetOpenList(id string) (types.OpenList, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, o := range s.data.OpenLists {
		if o.ID == id {
			return o, nil
		}
	}
	return types.OpenList{}, ErrNotFound
}

// CreateOpenList persists a new connection.
func (s *Store) CreateOpenList(o types.OpenList) (types.OpenList, error) {
	o.ID = "ol_" + randomHex(8)
	o.CreatedAt = time.Now().UTC()
	if o.Status == "" {
		o.Status = "unknown"
	}
	err := s.update(func(d *data) (bool, error) {
		d.OpenLists = append(d.OpenLists, o)
		return true, nil
	})
	return o, err
}

// UpdateOpenList applies fn to the connection with the given ID.
func (s *Store) UpdateOpenList(id string, fn func(*types.OpenList)) (types.OpenList, error) {
	var updated types.OpenList
	err := s.update(func(d *data) (bool, error) {
		for i := range d.OpenLists {
			if d.OpenLists[i].ID == id {
				fn(&d.OpenLists[i])
				updated = d.OpenLists[i]
				return true, nil
			}
		}
		return false, ErrNotFound
	})
	return updated, err
}

// DeleteOpenList removes a connection. Jobs referencing it are cleared unless
// they are guarded by the caller (see CountJobsByOpenList).
func (s *Store) DeleteOpenList(id string) error {
	return s.update(func(d *data) (bool, error) {
		out := d.OpenLists[:0]
		found := false
		for _, o := range d.OpenLists {
			if o.ID == id {
				found = true
				continue
			}
			out = append(out, o)
		}
		if !found {
			return false, ErrNotFound
		}
		d.OpenLists = out
		for i := range d.Jobs {
			if d.Jobs[i].OpenListID == id {
				d.Jobs[i].OpenListID = ""
			}
		}
		return true, nil
	})
}

// SetOpenListStatus records the result of a connection test.
func (s *Store) SetOpenListStatus(id, status, message string) error {
	return s.update(func(d *data) (bool, error) {
		for i := range d.OpenLists {
			if d.OpenLists[i].ID == id {
				d.OpenLists[i].Status = status
				d.OpenLists[i].Message = message
				d.OpenLists[i].LastCheck = time.Now().UTC()
				return true, nil
			}
		}
		return false, ErrNotFound
	})
}

// CountJobsByOpenList returns how many jobs use a connection.
func (s *Store) CountJobsByOpenList(id string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, j := range s.data.Jobs {
		if j.OpenListID == id {
			n++
		}
	}
	return n
}

// OpenListName resolves a connection's display name, tolerating deletions.
func (s *Store) OpenListName(id string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, o := range s.data.OpenLists {
		if o.ID == id {
			return o.Name
		}
	}
	return ""
}

// EnsureDefaultOpenList creates or updates the connection used by the panel's
// bootstrap flags (-openlist-url/-user/-pass) and by jobs without an explicit
// OpenListID.
func (s *Store) EnsureDefaultOpenList(baseURL, username, password string) error {
	return s.update(func(d *data) (bool, error) {
		for i := range d.OpenLists {
			if d.OpenLists[i].Name == "默认连接" || d.OpenLists[i].BaseURL == baseURL {
				if baseURL != "" {
					d.OpenLists[i].BaseURL = baseURL
				}
				if username != "" {
					d.OpenLists[i].Username = username
				}
				if password != "" {
					d.OpenLists[i].Password = password
				}
				return true, nil
			}
		}
		d.OpenLists = append(d.OpenLists, types.OpenList{
			ID:        "ol_" + randomHex(8),
			Name:      "默认连接",
			BaseURL:   baseURL,
			Username:  username,
			Password:  password,
			Status:    "unknown",
			CreatedAt: time.Now().UTC(),
		})
		return true, nil
	})
}

// ---- jobs ----------------------------------------------------------------

// ListJobs returns all jobs. When nodeID is non-empty, only that node's jobs
// are returned.
func (s *Store) ListJobs(nodeID string) []types.Job {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]types.Job, 0, len(s.data.Jobs))
	for _, j := range s.data.Jobs {
		if nodeID == "" || j.NodeID == nodeID {
			out = append(out, j)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	return out
}

// GetJob looks up a job by ID.
func (s *Store) GetJob(id string) (types.Job, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, j := range s.data.Jobs {
		if j.ID == id {
			return j, nil
		}
	}
	return types.Job{}, ErrNotFound
}

// CreateJob persists a new job.
func (s *Store) CreateJob(j types.Job) (types.Job, error) {
	j.ID = "job_" + randomHex(8)
	j.CreatedAt = time.Now().UTC()
	j.UpdatedAt = j.CreatedAt
	if j.Options.Concurrency <= 0 {
		j.Options.Concurrency = 2
	}
	err := s.update(func(d *data) (bool, error) {
		d.Jobs = append(d.Jobs, j)
		return true, nil
	})
	return j, err
}

// UpdateJob applies fn to the job with the given ID.
func (s *Store) UpdateJob(id string, fn func(*types.Job)) (types.Job, error) {
	var updated types.Job
	err := s.update(func(d *data) (bool, error) {
		for i := range d.Jobs {
			if d.Jobs[i].ID == id {
				fn(&d.Jobs[i])
				d.Jobs[i].UpdatedAt = time.Now().UTC()
				updated = d.Jobs[i]
				return true, nil
			}
		}
		return false, ErrNotFound
	})
	return updated, err
}

// DeleteJob removes a job by ID.
func (s *Store) DeleteJob(id string) error {
	return s.update(func(d *data) (bool, error) {
		out := d.Jobs[:0]
		found := false
		for _, j := range d.Jobs {
			if j.ID == id {
				found = true
				continue
			}
			out = append(out, j)
		}
		if !found {
			return false, ErrNotFound
		}
		d.Jobs = out
		return true, nil
	})
}

// ---- runs ----------------------------------------------------------------

// UpsertRun inserts a run or updates an existing one, then prunes history.
func (s *Store) UpsertRun(r types.Run) error {
	return s.update(func(d *data) (bool, error) {
		for i := range d.Runs {
			if d.Runs[i].ID == r.ID {
				// Keep previously stored logs when the update carries none.
				if len(r.Log) == 0 {
					r.Log = d.Runs[i].Log
				}
				d.Runs[i] = r
				return true, nil
			}
		}
		d.Runs = append(d.Runs, r)
		if max := d.Settings.MaxRuns; max > 0 && len(d.Runs) > max {
			// Runs are appended roughly in start order; drop the oldest.
			sort.SliceStable(d.Runs, func(i, j int) bool {
				return d.Runs[i].StartedAt.Before(d.Runs[j].StartedAt)
			})
			d.Runs = d.Runs[len(d.Runs)-max:]
		}
		return true, nil
	})
}

// ListRuns returns runs filtered by jobID (optional), most recent first.
func (s *Store) ListRuns(jobID string, limit int) []types.Run {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]types.Run, 0, len(s.data.Runs))
	for _, r := range s.data.Runs {
		if jobID == "" || r.JobID == jobID {
			out = append(out, r)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].StartedAt.After(out[j].StartedAt) })
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out
}

// JobName resolves a job's display name, tolerating deleted jobs.
func (s *Store) JobName(id string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, j := range s.data.Jobs {
		if j.ID == id {
			return j.Name
		}
	}
	return ""
}

// GetRun returns a stored run by ID and whether it was found.
func (s *Store) GetRun(id string) (types.Run, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, r := range s.data.Runs {
		if r.ID == id {
			return r, true
		}
	}
	return types.Run{}, false
}

// ---- notifications -------------------------------------------------------

// AddNotification records a pushed (or attempted) notification, pruning the
// oldest entries so the log stays bounded.
func (s *Store) AddNotification(n types.Notification) error {
	if n.ID == "" {
		n.ID = "ntf_" + randomHex(8)
	}
	if n.CreatedAt.IsZero() {
		n.CreatedAt = time.Now().UTC()
	}
	return s.update(func(d *data) (bool, error) {
		d.Notifications = append(d.Notifications, n)
		const max = 500
		if len(d.Notifications) > max {
			d.Notifications = d.Notifications[len(d.Notifications)-max:]
		}
		return true, nil
	})
}

// ListNotifications returns notifications, most recent first.
func (s *Store) ListNotifications(limit int) []types.Notification {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]types.Notification, len(s.data.Notifications))
	copy(out, s.data.Notifications)
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out
}

func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		// crypto/rand should never fail; fall back to time-based entropy.
		for i := range b {
			b[i] = byte(time.Now().UnixNano() >> (i % 8))
		}
	}
	return hex.EncodeToString(b)
}

// pbkdf2Iterations is the work factor for new password hashes. It is stored
// alongside each hash so it can be raised without breaking existing logins.
const pbkdf2Iterations = 210_000

// hmacHex returns hex(HMAC-SHA256(key, msg)).
func hmacHex(key, msg string) string {
	mac := hmac.New(sha256.New, []byte(key))
	mac.Write([]byte(msg))
	return hex.EncodeToString(mac.Sum(nil))
}

// normalizeRecoveryCode folds case and strips separators so users can type a
// recovery code with or without dashes.
func normalizeRecoveryCode(code string) string {
	code = strings.ToUpper(strings.TrimSpace(code))
	code = strings.ReplaceAll(code, "-", "")
	code = strings.ReplaceAll(code, " ", "")
	return code
}
