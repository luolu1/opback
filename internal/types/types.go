// Package types holds the data structures shared between the panel and the
// agent. Everything is JSON-serialisable so the exact same struct can travel
// over the sync API and be persisted by either side.
package types

import "time"

// Run status values.
const (
	RunRunning = "running"
	RunSuccess = "success"
	RunPartial = "partial" // finished but some files failed
	RunFailed  = "failed"
)

// Trigger values describe what started a run.
const (
	TriggerSchedule = "schedule"
	TriggerManual   = "manual"
)

// Node is an agent registered with the panel.
type Node struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Token    string `json:"token,omitempty"` // secret, never sent to list endpoints
	Hostname string `json:"hostname"`
	// IP is the peer address the panel last observed the agent connecting from.
	// It is the agent's real network address, which is often more useful than
	// the self-reported hostname (which may be "localhost" or stale).
	IP           string    `json:"ip,omitempty"`
	OS           string    `json:"os"`
	Arch         string    `json:"arch"`
	AgentVersion string    `json:"agent_version"`
	LastSeen     time.Time `json:"last_seen"`
	// Uninstalling is set once the operator asked the agent to remove itself.
	// The node stays visible until the agent confirms (or is force-deleted).
	Uninstalling bool      `json:"uninstalling,omitempty"`
	CreatedAt    time.Time `json:"created_at"`
}

// JobOptions tunes how a backup job is executed.
type JobOptions struct {
	// Incremental skips files whose size and modification time match the
	// last successful upload recorded in the agent's local manifest.
	Incremental bool `json:"incremental"`
	// Overwrite allows replacing an existing file at the destination.
	Overwrite bool `json:"overwrite"`
	// FollowSymlinks follows symbolic links while walking the source.
	FollowSymlinks bool `json:"follow_symlinks"`
	// Exclude is a list of path globs (relative to the source root) that are
	// skipped. Matched against the relative path using path/filepath.Match.
	Exclude []string `json:"exclude,omitempty"`
	// Include, when non-empty, restricts the backup to matching relative
	// paths.
	Include []string `json:"include,omitempty"`
	// Concurrency is the number of files uploaded in parallel.
	Concurrency int `json:"concurrency"`
	// DeleteExtra removes files that exist at the destination but are absent
	// from the source selection (after include/exclude filtering). When
	// RetentionDays > 0 they are moved to a trash folder instead of being
	// deleted outright.
	DeleteExtra bool `json:"delete_extra"`
	// RetentionDays keeps files moved aside by DeleteExtra in a trash folder
	// for this many days before they are permanently purged. Only meaningful
	// when DeleteExtra is set. 0 deletes extras immediately.
	RetentionDays int `json:"retention_days"`
}

// Job is a single backup definition: copy a local directory on a node to a
// directory inside OpenList.
type Job struct {
	ID         string     `json:"id"`
	Name       string     `json:"name"`
	NodeID     string     `json:"node_id"`
	OpenListID string     `json:"openlist_id,omitempty"` // which OpenList connection to use; empty means the default one
	Source     string     `json:"source"`                // local path on the agent
	Dest       string     `json:"dest"`                  // OpenList path, e.g. /backups/web1
	Interval   int        `json:"interval"`
	Enabled    bool       `json:"enabled"`
	Options    JobOptions `json:"options"`
	CreatedAt  time.Time  `json:"created_at"`
	UpdatedAt  time.Time  `json:"updated_at"`
}

// OpenList is one OpenList server the panel can upload to. Credentials are
// handed to agents during sync, so the same job can target different servers.
type OpenList struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	BaseURL   string    `json:"base_url"`
	Username  string    `json:"username"`
	Password  string    `json:"password,omitempty"` // secret, masked in list views
	Status    string    `json:"status"`             // "unknown" | "ok" | "error"
	Message   string    `json:"message,omitempty"`
	LastCheck time.Time `json:"last_check,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

// EffectiveInterval returns the schedule interval in seconds, applying sane
// defaults. A value <= 0 means the job only runs on demand.
func (j Job) EffectiveInterval() int {
	if j.Interval < 0 {
		return 0
	}
	return j.Interval
}

// Run is one execution of a Job, reported by the agent.
type Run struct {
	ID           string    `json:"id"`
	JobID        string    `json:"job_id"`
	JobName      string    `json:"job_name"`
	NodeID       string    `json:"node_id"`
	NodeName     string    `json:"node_name,omitempty"`
	OpenListName string    `json:"openlist_name,omitempty"`
	Source       string    `json:"source,omitempty"`
	Dest         string    `json:"dest,omitempty"`
	Status       string    `json:"status"`
	Trigger      string    `json:"trigger"`
	StartedAt    time.Time `json:"started_at"`
	FinishedAt   time.Time `json:"finished_at,omitempty"`
	FilesTotal   int       `json:"files_total"`
	FilesDone    int       `json:"files_done"`
	FilesSkipped int       `json:"files_skipped"`
	FilesFailed  int       `json:"files_failed"`
	BytesTotal   int64     `json:"bytes_total"`
	BytesDone    int64     `json:"bytes_done"`
	Message      string    `json:"message,omitempty"`
	Log          []string  `json:"log,omitempty"`
}

// RunReport is the subset of a Run the agent sends to the panel. Status
// "running" updates live progress; a terminal status finalises the run.
type RunReport struct {
	RunID        string    `json:"run_id"`
	JobID        string    `json:"job_id"`
	Status       string    `json:"status"`
	Trigger      string    `json:"trigger"`
	StartedAt    time.Time `json:"started_at"`
	FinishedAt   time.Time `json:"finished_at,omitempty"`
	FilesTotal   int       `json:"files_total"`
	FilesDone    int       `json:"files_done"`
	FilesSkipped int       `json:"files_skipped"`
	FilesFailed  int       `json:"files_failed"`
	BytesTotal   int64     `json:"bytes_total"`
	BytesDone    int64     `json:"bytes_done"`
	Message      string    `json:"message,omitempty"`
	Log          []string  `json:"log,omitempty"`
}

// ToRun converts a report into a stored Run.
func (r RunReport) ToRun(jobName string) Run {
	return Run{
		ID:           r.RunID,
		JobID:        r.JobID,
		JobName:      jobName,
		Status:       r.Status,
		Trigger:      r.Trigger,
		StartedAt:    r.StartedAt,
		FinishedAt:   r.FinishedAt,
		FilesTotal:   r.FilesTotal,
		FilesDone:    r.FilesDone,
		FilesSkipped: r.FilesSkipped,
		FilesFailed:  r.FilesFailed,
		BytesTotal:   r.BytesTotal,
		BytesDone:    r.BytesDone,
		Message:      r.Message,
		Log:          r.Log,
	}
}

// AgentConfig is the OpenList connection details handed to agents on sync.
type AgentConfig struct {
	BaseURL  string `json:"base_url"`
	Username string `json:"username"`
	Password string `json:"password"`
}

// TelegramSettings configures the panel's optional Telegram bot. The bot both
// pushes run results and answers status queries from the configured chat.
type TelegramSettings struct {
	Enabled       bool   `json:"enabled"`
	BotToken      string `json:"bot_token"`
	ChatID        string `json:"chat_id"`
	NotifySuccess bool   `json:"notify_success"`
	NotifyFailure bool   `json:"notify_failure"`
}

// Notification is one message the panel pushed (or attempted to push) to an
// external channel, retained so the web UI can show what was sent.
type Notification struct {
	ID        string    `json:"id"`
	Level     string    `json:"level"` // "success" | "error" | "info"
	Title     string    `json:"title"`
	Text      string    `json:"text"`
	Channel   string    `json:"channel"` // "telegram"
	OK        bool      `json:"ok"`
	Error     string    `json:"error,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

// SyncRequest is the body an agent posts on each heartbeat.
type SyncRequest struct {
	Hostname     string      `json:"hostname"`
	OS           string      `json:"os"`
	Arch         string      `json:"arch"`
	AgentVersion string      `json:"agent_version"`
	Reports      []RunReport `json:"reports,omitempty"`
}

// Command tells an agent to do something out of band: run a job now, or
// uninstall itself.
type Command struct {
	JobID  string `json:"job_id,omitempty"`
	Action string `json:"action"` // "run" | "uninstall"
}

// SyncResponse is everything an agent needs for its next cycle.
type SyncResponse struct {
	NodeID       string                 `json:"node_id"`
	SyncInterval int                    `json:"sync_interval"`
	OpenLists    map[string]AgentConfig `json:"openlists,omitempty"` // keyed by OpenList ID
	Jobs         []Job                  `json:"jobs"`
	Commands     []Command              `json:"commands,omitempty"`
}
