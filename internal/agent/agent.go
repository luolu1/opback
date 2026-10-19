package agent

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"runtime"
	"strings"
	"sync"
	"time"

	"opback/internal/openlist"
	"opback/internal/types"
)

// Version is reported to the panel and shown in the UI.
const Version = "0.1.0"

// Agent is the per-machine worker that pulls jobs from the panel and uploads
// files to OpenList.
type Agent struct {
	cfg Config
	st  *stateStore

	http     *http.Client
	hostname string

	ctx context.Context

	// OpenList clients, one per connection configured on the panel, keyed by
	// OpenList ID (the empty key is the legacy single connection). Refreshed
	// whenever the panel sends new credentials.
	olMu      sync.Mutex
	olCfgs    map[string]types.AgentConfig
	olClients map[string]*openlist.Client

	// Jobs last received from the panel.
	jobsMu sync.Mutex
	jobs   []types.Job

	// Run reports awaiting delivery, keyed by run ID.
	reportsMu sync.Mutex
	reports   map[string]types.RunReport

	// Jobs currently executing, keyed by job ID.
	runningMu sync.Mutex
	running   map[string]bool

	// syncCh nudges the main loop to sync immediately.
	syncCh chan struct{}
}

// New constructs an agent, loading any persisted state.
func New(cfg Config) (*Agent, error) {
	if cfg.PanelURL == "" {
		return nil, fmt.Errorf("必须提供面板地址 (panel-url)")
	}
	cfg.PanelURL = strings.TrimRight(cfg.PanelURL, "/")
	if cfg.StateDir == "" {
		cfg.StateDir = "./agent-state"
	}
	ss, err := loadState(cfg.StateDir)
	if err != nil {
		return nil, err
	}
	host, _ := os.Hostname()
	if cfg.Name == "" {
		cfg.Name = host
	}
	return &Agent{
		cfg:       cfg,
		st:        ss,
		hostname:  host,
		http:      &http.Client{Timeout: 60 * time.Second},
		olCfgs:    map[string]types.AgentConfig{},
		olClients: map[string]*openlist.Client{},
		reports:   map[string]types.RunReport{},
		running:   map[string]bool{},
		syncCh:    make(chan struct{}, 1),
	}, nil
}

// Run drives the agent until the context is cancelled (or, in once mode,
// until every enabled job has run a single time).
func (a *Agent) Run(ctx context.Context) error {
	a.ctx = ctx
	log.Printf("agent 启动：节点名称=%s hostname=%s panel=%s", a.cfg.Name, a.hostname, a.cfg.PanelURL)

	if a.cfg.Once {
		return a.runOnce(ctx)
	}

	interval := 15 * time.Second
	for {
		resp, err := a.sync(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			log.Printf("与面板同步失败：%v", err)
		} else {
			if resp.SyncInterval > 0 {
				interval = time.Duration(resp.SyncInterval) * time.Second
			}
			a.setOpenLists(resp.OpenLists)
			a.setJobs(resp.Jobs)
			for _, cmd := range resp.Commands {
				switch cmd.Action {
				case "run":
					if job, ok := a.findJob(cmd.JobID); ok {
						a.startJob(job, types.TriggerManual)
					}
				case "uninstall":
					if err := a.uninstall(ctx); err != nil {
						log.Printf("远程卸载失败：%v（请在目标机器上手动清理）", err)
					}
					return ErrUninstalled
				}
			}
			a.scheduleDue(resp.Jobs)
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(interval):
		case <-a.syncCh:
		}
	}
}

// runOnce executes every enabled job sequentially and pushes the results.
func (a *Agent) runOnce(ctx context.Context) error {
	resp, err := a.sync(ctx)
	if err != nil {
		return err
	}
	a.setOpenLists(resp.OpenLists)
	a.setJobs(resp.Jobs)

	for _, job := range resp.Jobs {
		if !job.Enabled {
			continue
		}
		log.Printf("单次模式：执行任务 %s", job.Name)
		a.runJob(ctx, job, types.TriggerManual)
		a.st.setLastRun(job.ID, time.Now().UTC())
	}
	if _, err := a.sync(ctx); err != nil {
		log.Printf("上报运行结果失败：%v", err)
	}
	return nil
}

// scheduleDue starts every enabled job whose interval has elapsed.
func (a *Agent) scheduleDue(jobs []types.Job) {
	now := time.Now().UTC()
	for _, job := range jobs {
		if !job.Enabled {
			continue
		}
		interval := job.EffectiveInterval()
		if interval <= 0 {
			continue
		}
		last := a.st.lastRun(job.ID)
		if last.IsZero() || now.Sub(last) >= time.Duration(interval)*time.Second {
			a.startJob(job, types.TriggerSchedule)
		}
	}
}

// startJob launches a job unless it is already running.
func (a *Agent) startJob(job types.Job, trigger string) bool {
	a.runningMu.Lock()
	if a.running[job.ID] {
		a.runningMu.Unlock()
		return false
	}
	a.running[job.ID] = true
	a.runningMu.Unlock()

	go func() {
		defer func() {
			a.runningMu.Lock()
			delete(a.running, job.ID)
			a.runningMu.Unlock()
		}()
		a.runJob(a.ctx, job, trigger)
		a.st.setLastRun(job.ID, time.Now().UTC())
	}()
	return true
}

// runJob performs a single backup and records the report.
func (a *Agent) runJob(ctx context.Context, job types.Job, trigger string) {
	started := time.Now().UTC()
	runID := "run_" + randHex(8)
	base := types.RunReport{
		RunID:     runID,
		JobID:     job.ID,
		Status:    types.RunRunning,
		Trigger:   trigger,
		StartedAt: started,
	}
	a.putReport(runID, base)
	a.requestSync()
	log.Printf("开始任务 %q：%s -> %s", job.Name, job.Source, job.Dest)

	ol := a.openListClientFor(job.OpenListID)
	if ol == nil {
		base.Status = types.RunFailed
		base.Message = "面板尚未配置该任务对应的 OpenList 连接"
		base.FinishedAt = time.Now().UTC()
		a.putReport(runID, base)
		a.requestSync()
		log.Printf("任务 %q 失败：%s", job.Name, base.Message)
		return
	}

	mf, err := loadManifest(a.cfg.StateDir, job.ID)
	if err != nil {
		log.Printf("任务 %q：加载增量索引失败（将全量上传）：%v", job.Name, err)
		mf = &manifest{entries: map[string]manifestEntry{}}
	}

	var logMu sync.Mutex
	var logLines []string
	logf := func(format string, args ...any) {
		msg := fmt.Sprintf(format, args...)
		logMu.Lock()
		if len(logLines) < 200 {
			logLines = append(logLines, msg)
		}
		logMu.Unlock()
		log.Printf("[%s] %s", job.Name, msg)
	}

	stats, errs := runBackup(ctx, ol, job, mf, func(s backupStats) {
		r := base
		r.FilesTotal = s.FilesTotal
		r.FilesDone = s.FilesDone
		r.FilesSkipped = s.FilesSkipped
		r.FilesFailed = s.FilesFailed
		r.BytesTotal = s.BytesTotal
		r.BytesDone = s.BytesDone
		a.putReport(runID, r)
	}, logf)

	final := base
	final.FilesTotal = stats.FilesTotal
	final.FilesDone = stats.FilesDone
	final.FilesSkipped = stats.FilesSkipped
	final.FilesFailed = stats.FilesFailed
	final.BytesTotal = stats.BytesTotal
	final.BytesDone = stats.BytesDone
	final.FinishedAt = time.Now().UTC()
	final.Status = finalStatus(stats, errs)
	final.Message = summary(stats, errs)
	logMu.Lock()
	final.Log = append([]string(nil), logLines...)
	logMu.Unlock()
	a.putReport(runID, final)
	a.requestSync()

	log.Printf("任务 %q 结束：%s（上传 %d，跳过 %d，失败 %d，共 %d 字节）",
		job.Name, final.Status, stats.FilesDone, stats.FilesSkipped, stats.FilesFailed, stats.BytesDone)
}

// ---- reporting -----------------------------------------------------------

func (a *Agent) putReport(runID string, r types.RunReport) {
	a.reportsMu.Lock()
	a.reports[runID] = r
	a.reportsMu.Unlock()
}

func (a *Agent) snapshotReports() []types.RunReport {
	a.reportsMu.Lock()
	defer a.reportsMu.Unlock()
	out := make([]types.RunReport, 0, len(a.reports))
	for _, r := range a.reports {
		out = append(out, r)
	}
	return out
}

// ackReports drops reports that reached a terminal state and were delivered.
func (a *Agent) ackReports(sent []types.RunReport) {
	a.reportsMu.Lock()
	defer a.reportsMu.Unlock()
	for _, r := range sent {
		if r.Status == types.RunRunning {
			continue
		}
		cur, ok := a.reports[r.RunID]
		if !ok {
			continue
		}
		if cur.Status == r.Status && cur.FilesDone == r.FilesDone {
			delete(a.reports, r.RunID)
		}
	}
}

func (a *Agent) requestSync() {
	select {
	case a.syncCh <- struct{}{}:
	default:
	}
}

// ---- panel communication -------------------------------------------------

type registerRequest struct {
	Name        string `json:"name"`
	Hostname    string `json:"hostname"`
	OS          string `json:"os"`
	Arch        string `json:"arch"`
	Version     string `json:"version"`
	EnrollToken string `json:"enroll_token"`
}

type registerResponse struct {
	NodeID string `json:"node_id"`
	Token  string `json:"token"`
}

func (a *Agent) register(ctx context.Context) error {
	reqBody, _ := json.Marshal(registerRequest{
		Name:        a.cfg.Name,
		Hostname:    a.hostname,
		OS:          runtime.GOOS,
		Arch:        runtime.GOARCH,
		Version:     Version,
		EnrollToken: a.cfg.EnrollToken,
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.cfg.PanelURL+"/api/agent/register", bytes.NewReader(reqBody))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := a.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("注册失败 (http %d): %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	var out registerResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		return err
	}
	if out.Token == "" {
		return fmt.Errorf("注册响应缺少令牌")
	}
	log.Printf("节点注册成功：%s", out.NodeID)
	return a.st.setCredentials(out.NodeID, out.Token)
}

func (a *Agent) sync(ctx context.Context) (*types.SyncResponse, error) {
	_, token := a.st.credentials()
	if token == "" {
		if err := a.register(ctx); err != nil {
			return nil, err
		}
		_, token = a.st.credentials()
	}

	reports := a.snapshotReports()
	reqBody, _ := json.Marshal(types.SyncRequest{
		Hostname:     a.hostname,
		OS:           runtime.GOOS,
		Arch:         runtime.GOARCH,
		AgentVersion: Version,
		Reports:      reports,
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.cfg.PanelURL+"/api/agent/sync", bytes.NewReader(reqBody))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Node-Token", token)

	resp, err := a.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))

	if resp.StatusCode == http.StatusUnauthorized {
		// Token no longer valid: forget it and register again next cycle.
		_ = a.st.clearCredentials()
		return nil, fmt.Errorf("节点令牌已失效，将重新注册")
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("同步失败 (http %d): %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}

	var out types.SyncResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, err
	}
	a.ackReports(reports)
	return &out, nil
}

// ---- OpenList client -----------------------------------------------------

// setOpenLists reconciles the local clients with the connections the panel
// sent. Clients are reused when credentials did not change so that an
// established session survives a sync.
func (a *Agent) setOpenLists(cfgs map[string]types.AgentConfig) {
	a.olMu.Lock()
	defer a.olMu.Unlock()
	if a.olClients == nil {
		a.olClients = map[string]*openlist.Client{}
		a.olCfgs = map[string]types.AgentConfig{}
	}
	for id, cfg := range cfgs {
		if old, ok := a.olCfgs[id]; ok && old == cfg && a.olClients[id] != nil {
			continue
		}
		a.olCfgs[id] = cfg
		a.olClients[id] = openlist.New(cfg.BaseURL, cfg.Username, cfg.Password)
		if id == "" {
			log.Printf("已更新默认 OpenList 连接：%s", cfg.BaseURL)
		} else {
			log.Printf("已更新 OpenList 连接 [%s]：%s", id, cfg.BaseURL)
		}
	}
	for id := range a.olClients {
		if _, ok := cfgs[id]; !ok {
			delete(a.olClients, id)
			delete(a.olCfgs, id)
			log.Printf("已移除 OpenList 连接 [%s]", id)
		}
	}
}

// openListClientFor returns the client for a job's OpenList ID. Jobs without
// an explicit ID use the legacy single connection ("") or, when only one
// connection exists, that one.
func (a *Agent) openListClientFor(id string) *openlist.Client {
	a.olMu.Lock()
	defer a.olMu.Unlock()
	if c, ok := a.olClients[id]; ok {
		return c
	}
	if id == "" && len(a.olClients) == 1 {
		for _, c := range a.olClients {
			return c
		}
	}
	return nil
}

// ---- job lookup ----------------------------------------------------------

func (a *Agent) setJobs(jobs []types.Job) {
	a.jobsMu.Lock()
	a.jobs = jobs
	a.jobsMu.Unlock()
}

func (a *Agent) findJob(id string) (types.Job, bool) {
	a.jobsMu.Lock()
	defer a.jobsMu.Unlock()
	for _, j := range a.jobs {
		if j.ID == id {
			return j, true
		}
	}
	return types.Job{}, false
}

// ---- helpers -------------------------------------------------------------

func finalStatus(s backupStats, errs []string) string {
	switch {
	case s.FilesTotal == 0 && len(errs) > 0:
		return types.RunFailed
	case s.FilesFailed > 0 && s.FilesDone == 0 && s.FilesSkipped == 0:
		return types.RunFailed
	case len(errs) > 0:
		// Something went wrong even though every selected file was handled:
		// the manifest could not be saved, or a file kept changing while it
		// was copied/read.
		return types.RunPartial
	default:
		return types.RunSuccess
	}
}

func summary(s backupStats, errs []string) string {
	if len(errs) > 0 {
		head := errs[0]
		if len(errs) > 1 {
			return fmt.Sprintf("%d 个错误，示例：%s（另有 %d 条）", len(errs), head, len(errs)-1)
		}
		return fmt.Sprintf("%d 个错误：%s", len(errs), head)
	}
	if s.FilesTotal == 0 {
		return "没有需要备份的文件"
	}
	return fmt.Sprintf("完成：上传 %d，跳过 %d，共 %d 字节", s.FilesDone, s.FilesSkipped, s.BytesDone)
}

func randHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return hex.EncodeToString([]byte(time.Now().String()))
	}
	return hex.EncodeToString(b)
}
