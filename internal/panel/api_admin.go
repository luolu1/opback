package panel

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"opback/internal/store"
	"opback/internal/types"
)

// ---- nodes ---------------------------------------------------------------

type nodeView struct {
	types.Node
	Status   string `json:"status"`
	JobCount int    `json:"job_count"`
}

func (s *Server) nodeView(n types.Node) nodeView {
	n.Token = "" // never leak tokens in listings
	settings := s.store.Settings()
	status := "offline"
	if nodeOnline(n, settings.SyncInterval, time.Now().UTC()) {
		status = "online"
	}
	return nodeView{Node: n, Status: status, JobCount: len(s.store.ListJobs(n.ID))}
}

func (s *Server) handleListNodes(w http.ResponseWriter, r *http.Request) {
	nodes := s.store.ListNodes()
	out := make([]nodeView, 0, len(nodes))
	for _, n := range nodes {
		out = append(out, s.nodeView(n))
	}
	writeOK(w, out)
}

func (s *Server) handleGetNode(w http.ResponseWriter, r *http.Request) {
	n, err := s.store.GetNode(r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusNotFound, "节点不存在")
		return
	}
	// A single-node fetch includes the token so the operator can configure an
	// agent by hand.
	view := s.nodeView(n)
	view.Token = n.Token
	writeOK(w, view)
}

type createNodeRequest struct {
	Name string `json:"name"`
}

func (s *Server) handleCreateNode(w http.ResponseWriter, r *http.Request) {
	var req createNodeRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if strings.TrimSpace(req.Name) == "" {
		writeErr(w, http.StatusBadRequest, "节点名称不能为空")
		return
	}
	n, err := s.store.CreateNode(req.Name, "", "", "", "")
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeOK(w, n) // includes the token, shown once to the operator
}

func (s *Server) handleDeleteNode(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	node, err := s.store.GetNode(id)
	if err != nil {
		writeErr(w, http.StatusNotFound, "节点不存在")
		return
	}

	// force=1 removes the record immediately (used when the agent is offline
	// and cannot run the remote uninstall, or when it is stuck).
	if r.URL.Query().Get("force") == "1" {
		if err := s.store.DeleteNode(id); err != nil {
			writeErr(w, http.StatusNotFound, "节点不存在")
			return
		}
		writeOK(w, map[string]any{"ok": true, "removed": true})
		return
	}

	// Graceful path: drop the node's jobs, ask the agent to uninstall itself,
	// and keep the node visible (marked 卸载中) until it confirms.
	if err := s.store.DeleteJobsByNode(id); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if err := s.store.SetNodeUninstalling(id, true); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.enqueueCommand(id, types.Command{Action: "uninstall"})

	online := nodeOnline(node, s.store.Settings().SyncInterval, time.Now().UTC())
	writeOK(w, map[string]any{
		"ok":           true,
		"uninstalling": true,
		"online":       online,
		"message":      "已下发卸载指令，等待 agent 确认",
	})
}

// ---- jobs ----------------------------------------------------------------

type jobRequest struct {
	Name       string            `json:"name"`
	NodeID     string            `json:"node_id"`
	OpenListID string            `json:"openlist_id"`
	Source     string            `json:"source"`
	Dest       string            `json:"dest"`
	Interval   int               `json:"interval"`
	Enabled    *bool             `json:"enabled"`
	Options    *types.JobOptions `json:"options"`
}

func (s *Server) handleListJobs(w http.ResponseWriter, r *http.Request) {
	writeOK(w, s.store.ListJobs(r.URL.Query().Get("node_id")))
}

func (s *Server) handleGetJob(w http.ResponseWriter, r *http.Request) {
	j, err := s.store.GetJob(r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusNotFound, "任务不存在")
		return
	}
	writeOK(w, j)
}

func (s *Server) handleCreateJob(w http.ResponseWriter, r *http.Request) {
	var req jobRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if err := s.validateJobReq(req); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	job := types.Job{
		Name:       req.Name,
		NodeID:     req.NodeID,
		OpenListID: req.OpenListID,
		Source:     strings.TrimRight(req.Source, "/"),
		Dest:       "/" + strings.Trim(req.Dest, "/"),
		Interval:   req.Interval,
		Enabled:    true,
		Options:    normalizeOptions(req.Options),
	}
	if req.Enabled != nil {
		job.Enabled = *req.Enabled
	}
	created, err := s.store.CreateJob(job)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeOK(w, created)
}

func (s *Server) handleUpdateJob(w http.ResponseWriter, r *http.Request) {
	var req jobRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if err := s.validateJobReq(req); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	updated, err := s.store.UpdateJob(r.PathValue("id"), func(j *types.Job) {
		j.Name = req.Name
		j.NodeID = req.NodeID
		j.OpenListID = req.OpenListID
		j.Source = strings.TrimRight(req.Source, "/")
		j.Dest = "/" + strings.Trim(req.Dest, "/")
		j.Interval = req.Interval
		if req.Enabled != nil {
			j.Enabled = *req.Enabled
		}
		j.Options = normalizeOptions(req.Options)
	})
	if err != nil {
		writeErr(w, http.StatusNotFound, "任务不存在")
		return
	}
	writeOK(w, updated)
}

func (s *Server) handleDeleteJob(w http.ResponseWriter, r *http.Request) {
	if err := s.store.DeleteJob(r.PathValue("id")); err != nil {
		writeErr(w, http.StatusNotFound, "任务不存在")
		return
	}
	writeOK(w, map[string]bool{"ok": true})
}

func (s *Server) handleRunJob(w http.ResponseWriter, r *http.Request) {
	j, err := s.store.GetJob(r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusNotFound, "任务不存在")
		return
	}
	s.enqueueCommand(j.NodeID, types.Command{JobID: j.ID, Action: "run"})
	writeOK(w, map[string]any{"ok": true, "queued": true, "node_id": j.NodeID})
}

func (s *Server) validateJobReq(req jobRequest) error {
	if strings.TrimSpace(req.Name) == "" {
		return errString("任务名称不能为空")
	}
	if strings.TrimSpace(req.NodeID) == "" {
		return errString("必须选择一个节点")
	}
	if _, err := s.store.GetNode(req.NodeID); err != nil {
		return errString("节点不存在")
	}
	if req.OpenListID != "" {
		if _, err := s.store.GetOpenList(req.OpenListID); err != nil {
			return errString("OpenList 连接不存在")
		}
	}
	if strings.TrimSpace(req.Source) == "" {
		return errString("源路径不能为空")
	}
	if strings.TrimSpace(req.Dest) == "" {
		return errString("目标路径不能为空")
	}
	if !strings.HasPrefix(req.Dest, "/") {
		return errString("目标路径必须以 / 开头")
	}
	return nil
}

func normalizeOptions(o *types.JobOptions) types.JobOptions {
	if o == nil {
		return types.JobOptions{Overwrite: true, Incremental: true, Concurrency: 2}
	}
	out := *o
	if out.Concurrency <= 0 {
		out.Concurrency = 2
	}
	if out.RetentionDays < 0 {
		out.RetentionDays = 0
	}
	if out.RetentionDays > 3650 {
		out.RetentionDays = 3650
	}
	return out
}

// ---- runs ----------------------------------------------------------------

func (s *Server) handleListRuns(w http.ResponseWriter, r *http.Request) {
	jobID := r.URL.Query().Get("job_id")
	limit := 100
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = n
		}
	}
	writeOK(w, s.store.ListRuns(jobID, limit))
}

func (s *Server) handleGetRun(w http.ResponseWriter, r *http.Request) {
	for _, run := range s.store.ListRuns("", 0) {
		if run.ID == r.PathValue("id") {
			writeOK(w, run)
			return
		}
	}
	writeErr(w, http.StatusNotFound, "运行记录不存在")
}

// ---- overview ------------------------------------------------------------

// overviewPoint is one day of activity on the dashboard's bar chart.
type overviewPoint struct {
	Date    string `json:"date"` // YYYY-MM-DD (UTC)
	Runs    int    `json:"runs"`
	Success int    `json:"success"`
	Failed  int    `json:"failed"`
	Bytes   int64  `json:"bytes"`
}

func (s *Server) handleOverview(w http.ResponseWriter, r *http.Request) {
	settings := s.store.Settings()
	nodes := s.store.ListNodes()
	now := time.Now().UTC()

	online := 0
	for _, n := range nodes {
		if nodeOnline(n, settings.SyncInterval, now) {
			online++
		}
	}
	jobs := s.store.ListJobs("")
	enabled := 0
	for _, j := range jobs {
		if j.Enabled {
			enabled++
		}
	}

	openlists := s.store.ListOpenLists()
	olOK, olErr := 0, 0
	for _, o := range openlists {
		switch o.Status {
		case "ok":
			olOK++
		case "error":
			olErr++
		}
	}

	allRuns := s.store.ListRuns("", 0)
	statusCounts := map[string]int{types.RunSuccess: 0, types.RunFailed: 0, types.RunPartial: 0, types.RunRunning: 0}
	var bytesTotal, bytes24h int64
	cutoff24h := now.Add(-24 * time.Hour)
	for _, run := range allRuns {
		if _, ok := statusCounts[run.Status]; ok {
			statusCounts[run.Status]++
		}
		if run.Status == types.RunSuccess || run.Status == types.RunPartial {
			bytesTotal += run.BytesDone
			if run.StartedAt.After(cutoff24h) {
				bytes24h += run.BytesDone
			}
		}
	}

	// 14-day daily activity series, oldest first.
	const days = 14
	series := make([]overviewPoint, days)
	index := make(map[string]int, days)
	for i := 0; i < days; i++ {
		d := now.AddDate(0, 0, -(days - 1 - i)).Format("2006-01-02")
		series[i] = overviewPoint{Date: d}
		index[d] = i
	}
	for _, run := range allRuns {
		if run.StartedAt.IsZero() {
			continue
		}
		i, ok := index[run.StartedAt.UTC().Format("2006-01-02")]
		if !ok {
			continue
		}
		series[i].Runs++
		switch run.Status {
		case types.RunSuccess, types.RunPartial:
			series[i].Success++
			series[i].Bytes += run.BytesDone
		case types.RunFailed:
			series[i].Failed++
		}
	}

	writeOK(w, map[string]any{
		"nodes_total":   len(nodes),
		"nodes_online":  online,
		"jobs_total":    len(jobs),
		"jobs_enabled":  enabled,
		"bytes_total":   bytesTotal,
		"bytes_24h":     bytes24h,
		"status_counts": statusCounts,
		"series":        series,
		"openlists":     len(openlists),
		"openlist_ok":   olOK,
		"openlist_err":  olErr,
		"recent_runs":   s.store.ListRuns("", 20),
		"openlist_set":  len(openlists) > 0 || settings.OpenList.BaseURL != "",
		"sync_interval": settings.SyncInterval,
	})
}

// ---- settings ------------------------------------------------------------

type settingsView struct {
	AdminUser    string                 `json:"admin_user"`
	EnrollToken  string                 `json:"enroll_token"`
	SyncInterval int                    `json:"sync_interval"`
	MaxRuns      int                    `json:"max_runs"`
	TwoFAEnabled bool                   `json:"twofa_enabled"`
	Telegram     types.TelegramSettings `json:"telegram"`
}

func (s *Server) handleGetSettings(w http.ResponseWriter, r *http.Request) {
	set := s.store.Settings()
	tg := set.Telegram
	// An unconfigured panel shows both toggles on so enabling the bot does
	// something sensible without extra clicks.
	if tg.BotToken == "" && tg.ChatID == "" && !tg.Enabled {
		tg.NotifySuccess = true
		tg.NotifyFailure = true
	}
	writeOK(w, settingsView{
		AdminUser:    set.AdminUser,
		EnrollToken:  set.EnrollToken,
		SyncInterval: set.SyncInterval,
		MaxRuns:      set.MaxRuns,
		TwoFAEnabled: set.TwoFAEnabled,
		Telegram:     tg,
	})
}

type updateSettingsRequest struct {
	EnrollToken  *string                 `json:"enroll_token"`
	SyncInterval *int                    `json:"sync_interval"`
	MaxRuns      *int                    `json:"max_runs"`
	Telegram     *types.TelegramSettings `json:"telegram"`
}

func (s *Server) handleUpdateSettings(w http.ResponseWriter, r *http.Request) {
	var req updateSettingsRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	err := s.store.UpdateSettings(func(set *store.Settings) {
		if req.EnrollToken != nil {
			set.EnrollToken = strings.TrimSpace(*req.EnrollToken)
		}
		if req.SyncInterval != nil && *req.SyncInterval > 0 {
			set.SyncInterval = *req.SyncInterval
		}
		if req.MaxRuns != nil && *req.MaxRuns > 0 {
			set.MaxRuns = *req.MaxRuns
		}
		if req.Telegram != nil {
			tg := *req.Telegram
			tg.BotToken = strings.TrimSpace(tg.BotToken)
			tg.ChatID = strings.TrimSpace(tg.ChatID)
			set.Telegram = tg
		}
	})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.handleGetSettings(w, r)
}

type passwordRequest struct {
	Username        string `json:"username"`
	Password        string `json:"password"`
	CurrentPassword string `json:"current_password"`
}

func (s *Server) handleChangePassword(w http.ResponseWriter, r *http.Request) {
	var req passwordRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	set := s.store.Settings()
	user := strings.TrimSpace(req.Username)
	if user == "" {
		user = set.AdminUser
	}
	// Changing the password (or touching an account protected by 2FA) requires
	// re-proving the current password so a hijacked session cannot silently
	// rotate the credentials.
	if req.Password != "" || set.TwoFAEnabled {
		if !s.verifyPassword(req.CurrentPassword) {
			writeErr(w, http.StatusUnauthorized, "当前密码错误")
			return
		}
	}
	// An empty password means "only rename the account"; the session cookie
	// stays valid because it is signed from the payload, not the username.
	if req.Password == "" {
		if user == set.AdminUser {
			writeOK(w, map[string]bool{"ok": true})
			return
		}
		if err := s.store.SetAdminUser(user); err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeOK(w, map[string]bool{"ok": true})
		return
	}
	if len(req.Password) < 8 {
		writeErr(w, http.StatusBadRequest, "密码至少 8 位")
		return
	}
	if err := s.store.SetAdminPassword(user, req.Password); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	// Re-issue the session so the operator who just changed the password is not
	// logged out by the epoch bump.
	s.finishLogin(w, user)
}

// errString is a tiny error type that avoids importing errors at every call.
type errString string

func (e errString) Error() string { return string(e) }
