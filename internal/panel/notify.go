package panel

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"opback/internal/types"
)

// This file implements the optional Telegram integration: push notifications
// for finished runs, plus a tiny long-polling bot that answers /status queries.

// tgHTTP is shared by every Telegram call. Contexts carry the deadlines.
var tgHTTP = &http.Client{}

type tgEnvelope struct {
	OK          bool            `json:"ok"`
	Description string          `json:"description"`
	Result      json.RawMessage `json:"result"`
}

type tgChat struct {
	ID        int64  `json:"id"`
	Type      string `json:"type"`
	Title     string `json:"title"`
	Username  string `json:"username"`
	FirstName string `json:"first_name"`
	LastName  string `json:"last_name"`
}

// displayName is a human-friendly label for a chat, used by the chat picker.
func (c tgChat) displayName() string {
	if c.Title != "" {
		return c.Title
	}
	if n := strings.TrimSpace(c.FirstName + " " + c.LastName); n != "" {
		return n
	}
	if c.Username != "" {
		return "@" + c.Username
	}
	return strconv.FormatInt(c.ID, 10)
}

type tgMessage struct {
	Text string `json:"text"`
	Chat tgChat `json:"chat"`
}

type tgUpdate struct {
	UpdateID int64      `json:"update_id"`
	Message  *tgMessage `json:"message"`
}

// tgCall performs one Bot API method and returns its "result" field.
func (s *Server) tgCall(ctx context.Context, token, method string, payload any) (json.RawMessage, error) {
	if strings.TrimSpace(token) == "" {
		return nil, fmt.Errorf("未配置 Bot Token")
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	url := "https://api.telegram.org/bot" + token + "/" + method
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := tgHTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var env tgEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("Telegram 返回异常 (HTTP %d)", resp.StatusCode)
	}
	if !env.OK {
		if env.Description != "" {
			return nil, fmt.Errorf("%s", env.Description)
		}
		return nil, fmt.Errorf("Telegram 请求失败 (HTTP %d)", resp.StatusCode)
	}
	return env.Result, nil
}

func (s *Server) tgSendMessage(ctx context.Context, token, chatID, text string) error {
	_, err := s.tgCall(ctx, token, "sendMessage", map[string]any{
		"chat_id":                  chatID,
		"text":                     text,
		"parse_mode":               "HTML",
		"disable_web_page_preview": true,
	})
	return err
}

func (s *Server) tgGetUpdates(ctx context.Context, token string, offset int64, timeout int) ([]tgUpdate, error) {
	raw, err := s.tgCall(ctx, token, "getUpdates", map[string]any{
		"offset":          offset,
		"timeout":         timeout,
		"allowed_updates": []string{"message"},
	})
	if err != nil {
		return nil, err
	}
	var ups []tgUpdate
	if err := json.Unmarshal(raw, &ups); err != nil {
		return nil, err
	}
	return ups, nil
}

// ---- notifications -------------------------------------------------------

// sendAndRecord pushes text to Telegram and stores the attempt in the log.
func (s *Server) sendAndRecord(ctx context.Context, tg types.TelegramSettings, text, level, title string) error {
	err := s.tgSendMessage(ctx, tg.BotToken, tg.ChatID, text)
	n := types.Notification{Level: level, Title: title, Text: text, Channel: "telegram", OK: err == nil}
	if err != nil {
		n.Error = err.Error()
	}
	_ = s.store.AddNotification(n)
	return err
}

// notifyRun pushes a finished run to Telegram when the operator enabled it.
// It runs asynchronously so a slow Telegram call never blocks a sync response.
func (s *Server) notifyRun(run types.Run) {
	tg := s.store.Settings().Telegram
	if !tg.Enabled || tg.BotToken == "" || tg.ChatID == "" {
		return
	}
	if !shouldNotify(tg, run.Status) {
		return
	}
	text := formatRunMessage(run)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		_ = s.sendAndRecord(ctx, tg, text, levelForStatus(run.Status), "备份"+statusWord(run.Status))
	}()
}

// isTerminalRun reports whether a run has finished (success, partial or failed).
func isTerminalRun(status string) bool {
	switch status {
	case types.RunSuccess, types.RunPartial, types.RunFailed:
		return true
	}
	return false
}

func shouldNotify(tg types.TelegramSettings, status string) bool {
	switch status {
	case types.RunSuccess, types.RunPartial:
		return tg.NotifySuccess
	case types.RunFailed:
		return tg.NotifyFailure
	}
	return false
}

func levelForStatus(status string) string {
	switch status {
	case types.RunSuccess:
		return "success"
	case types.RunFailed, types.RunPartial:
		return "error"
	}
	return "info"
}

func statusWord(status string) string {
	switch status {
	case types.RunSuccess:
		return "成功"
	case types.RunPartial:
		return "部分失败"
	case types.RunFailed:
		return "失败"
	case types.RunRunning:
		return "进行中"
	}
	return status
}

func statusIcon(status string) string {
	switch status {
	case types.RunSuccess:
		return "✅"
	case types.RunPartial:
		return "⚠️"
	case types.RunFailed:
		return "❌"
	case types.RunRunning:
		return "🔄"
	}
	return "⚪"
}

// ---- message formatting --------------------------------------------------

func tgEscape(s string) string {
	s = strings.ReplaceAll(s, "&", "&amp;")
	s = strings.ReplaceAll(s, "<", "&lt;")
	s = strings.ReplaceAll(s, ">", "&gt;")
	return s
}

func formatRunMessage(run types.Run) string {
	var b strings.Builder
	title := tgEscape(run.JobName)
	if run.NodeName != "" {
		title += " (" + tgEscape(run.NodeName) + ")"
	}
	fmt.Fprintf(&b, "%s <b>备份%s</b> · %s\n", statusIcon(run.Status), statusWord(run.Status), title)

	fmt.Fprintf(&b, "文件 %d 上传 / %d 跳过 / %d 失败 · %s", run.FilesDone, run.FilesSkipped, run.FilesFailed, humanBytes(run.BytesDone))
	if d := runDuration(run); d != "" {
		fmt.Fprintf(&b, " · 耗时 %s", d)
	}
	b.WriteString("\n")

	meta := make([]string, 0, 2)
	if run.OpenListName != "" {
		meta = append(meta, "连接 "+tgEscape(run.OpenListName))
	}
	meta = append(meta, run.StartedAt.Local().Format("2006-01-02 15:04:05"))
	b.WriteString(strings.Join(meta, " · "))

	// The agent's summary ("完成：上传 0，跳过 3，共 0 字节") merely repeats the
	// counts on the line above, so only surface it for failures/partial runs,
	// where it carries the actual error detail.
	if run.Message != "" && run.Status != types.RunSuccess {
		fmt.Fprintf(&b, "\n信息：%s", tgEscape(run.Message))
	}
	return b.String()
}

// botCommands is the menu Telegram shows when the user types "/". It is
// published with setMyCommands so the bot looks like a normal bot.
var botCommands = []map[string]string{
	{"command": "status", "description": "各任务最近一次同步状态"},
	{"command": "tasks", "description": "任务列表（连接、路径、计划）"},
	{"command": "sync", "description": "同步历史与成功率"},
	{"command": "id", "description": "获取当前会话的 Chat ID"},
	{"command": "help", "description": "显示帮助"},
}

// tgSetCommands publishes the command menu for a token (best effort).
func (s *Server) tgSetCommands(ctx context.Context, token string) error {
	_, err := s.tgCall(ctx, token, "setMyCommands", map[string]any{"commands": botCommands})
	return err
}

func helpMessage(ready bool) string {
	var b strings.Builder
	b.WriteString("🤖 <b>OpenList 备份机器人</b>\n\n")
	b.WriteString("/id — 获取当前会话的 Chat ID\n")
	b.WriteString("/status — 各任务最近一次同步状态\n")
	b.WriteString("/tasks — 任务列表（连接、路径、计划）\n")
	b.WriteString("/sync — 同步历史（最近 10 次与成功率）\n")
	b.WriteString("/help — 显示本帮助\n")
	if ready {
		b.WriteString("\n备份成功或失败时，我会自动推送通知。")
	} else {
		b.WriteString("\n⚠️ 尚未配置 Chat ID：先用 /id 获取本会话 ID，填入面板「设置 → Telegram」后，/status、/tasks、/sync 才能使用。")
	}
	return b.String()
}

// ---- status snapshot (shared by the bot and the web UI) ------------------

type syncStatusItem struct {
	JobID       string    `json:"job_id"`
	JobName     string    `json:"job_name"`
	NodeID      string    `json:"node_id"`
	NodeName    string    `json:"node_name"`
	NodeOnline  bool      `json:"node_online"`
	Enabled     bool      `json:"enabled"`
	Interval    int       `json:"interval"`
	HasRun      bool      `json:"has_run"`
	RunID       string    `json:"run_id"`
	Status      string    `json:"status"`
	StartedAt   time.Time `json:"started_at"`
	FinishedAt  time.Time `json:"finished_at"`
	FilesDone   int       `json:"files_done"`
	FilesFailed int       `json:"files_failed"`
	BytesDone   int64     `json:"bytes_done"`
	Message     string    `json:"message"`
}

type syncStatusResponse struct {
	GeneratedAt time.Time        `json:"generated_at"`
	NodesTotal  int              `json:"nodes_total"`
	NodesOnline int              `json:"nodes_online"`
	JobsTotal   int              `json:"jobs_total"`
	Success     int              `json:"success"`
	Failed      int              `json:"failed"`
	Never       int              `json:"never"`
	Items       []syncStatusItem `json:"items"`
}

// buildSyncStatus summarises the latest run of every job, newest first.
func (s *Server) buildSyncStatus() syncStatusResponse {
	settings := s.store.Settings()
	now := time.Now().UTC()
	nodes := s.store.ListNodes()
	online := make(map[string]bool, len(nodes))
	for _, n := range nodes {
		online[n.ID] = nodeOnline(n, settings.SyncInterval, now)
	}

	jobs := s.store.ListJobs("")
	resp := syncStatusResponse{
		GeneratedAt: now,
		NodesTotal:  len(nodes),
		JobsTotal:   len(jobs),
		Items:       make([]syncStatusItem, 0, len(jobs)),
	}
	for _, n := range nodes {
		if online[n.ID] {
			resp.NodesOnline++
		}
	}

	for _, j := range jobs {
		it := syncStatusItem{
			JobID:      j.ID,
			JobName:    j.Name,
			NodeID:     j.NodeID,
			NodeOnline: online[j.NodeID],
			Enabled:    j.Enabled,
			Interval:   j.Interval,
		}
		if n, err := s.store.GetNode(j.NodeID); err == nil {
			it.NodeName = n.Name
		}
		if runs := s.store.ListRuns(j.ID, 1); len(runs) > 0 {
			r := runs[0]
			it.HasRun = true
			it.RunID = r.ID
			it.Status = r.Status
			it.StartedAt = r.StartedAt
			it.FinishedAt = r.FinishedAt
			it.FilesDone = r.FilesDone
			it.FilesFailed = r.FilesFailed
			it.BytesDone = r.BytesDone
			it.Message = r.Message
			switch r.Status {
			case types.RunSuccess, types.RunPartial:
				resp.Success++
			case types.RunFailed:
				resp.Failed++
			}
		} else {
			resp.Never++
		}
		resp.Items = append(resp.Items, it)
	}

	sort.SliceStable(resp.Items, func(i, j int) bool {
		a, b := resp.Items[i], resp.Items[j]
		if a.HasRun != b.HasRun {
			return a.HasRun
		}
		return a.StartedAt.After(b.StartedAt)
	})
	return resp
}

// ---- bot loop ------------------------------------------------------------

func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// RunBot long-polls Telegram for commands until ctx is cancelled. It polls
// whenever a Bot Token is configured—even before Enabled/ChatID are set—so the
// operator can use /id to discover the Chat ID during setup. It is safe to call
// even with no token: it simply idles and re-checks settings.
func (s *Server) RunBot(ctx context.Context) {
	var offset int64
	lastToken := ""
	commandsSet := ""
	backoff := 5 * time.Second
	for {
		if ctx.Err() != nil {
			return
		}
		tg := s.store.Settings().Telegram
		if strings.TrimSpace(tg.BotToken) == "" {
			if !sleepCtx(ctx, 10*time.Second) {
				return
			}
			continue
		}
		if tg.BotToken != lastToken {
			// A token change invalidates the old offset and command menu.
			offset = 0
			lastToken = tg.BotToken
			commandsSet = ""
			backoff = 5 * time.Second
		}
		if commandsSet != lastToken {
			setCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
			err := s.tgSetCommands(setCtx, tg.BotToken)
			cancel()
			if err == nil {
				commandsSet = lastToken
			}
		}
		pollCtx, cancel := context.WithTimeout(ctx, 35*time.Second)
		ups, err := s.tgGetUpdates(pollCtx, tg.BotToken, offset, 25)
		cancel()
		if err != nil {
			// Back off so a bad token does not hammer the Telegram API.
			if !sleepCtx(ctx, backoff) {
				return
			}
			if backoff < time.Minute {
				backoff *= 2
			}
			continue
		}
		backoff = 5 * time.Second
		for _, u := range ups {
			if u.UpdateID >= offset {
				offset = u.UpdateID + 1
			}
			if u.Message != nil {
				s.recordChat(u.Message.Chat)
				// Read the Chat ID fresh: the operator may have just saved it
				// while we were blocked in the long poll, and a stale empty
				// value would silently drop the command.
				fresh := strings.TrimSpace(s.store.Settings().Telegram.ChatID)
				s.handleBotMessage(ctx, tg.BotToken, fresh, *u.Message)
			}
		}
	}
}

func (s *Server) handleBotMessage(ctx context.Context, token, configured string, m tgMessage) {
	chatID := strconv.FormatInt(m.Chat.ID, 10)
	cmd, reply, needID := botRoute(m.Text, chatID, configured)
	switch {
	case needID:
		s.sendBot(ctx, token, chatID, needChatIDMessage(chatID))
		return
	case !reply:
		return
	}

	send := func(text string) { s.sendBot(ctx, token, chatID, text) }
	switch cmd {
	case "/id":
		send(idMessage(chatID, m.Chat))
	case "/start", "/help":
		send(helpMessage(configured != ""))
	case "/status":
		send(formatStatusMessage(s.buildSyncStatus()))
	case "/tasks":
		send(s.buildTasksMessage())
	case "/sync":
		send(s.buildSyncHistoryMessage())
	default:
		send("未知命令。\n\n" + helpMessage(true))
	}
}

func (s *Server) sendBot(ctx context.Context, token, chatID, text string) {
	sendCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	_ = s.tgSendMessage(sendCtx, token, chatID, text)
}

func idMessage(chatID string, c tgChat) string {
	reply := "🆔 <b>本会话 Chat ID</b>\n<code>" + chatID + "</code>"
	if name := c.displayName(); name != "" {
		reply += "\n名称：" + tgEscape(name)
	}
	if c.Type != "" {
		reply += "\n类型：" + tgEscape(c.Type)
	}
	reply += "\n\n把它填入面板「设置 → Telegram 机器人推送」的 Chat ID 即可。"
	return reply
}

func needChatIDMessage(chatID string) string {
	return "⚠️ <b>请先在面板添加 Chat ID</b>\n<code>" + chatID + "</code>\n\n" +
		"打开面板「设置 → Telegram 机器人推送」，把上面的 ID 填入 Chat ID 并保存，即可使用 /status、/tasks、/sync。"
}

// botRoute decides how to answer a message:
//   - /id, /start and /help always work, even while the operator is still
//     setting the bot up;
//   - the status commands are answered only once the panel has a Chat ID, and
//     only for that chat, so a leaked token cannot expose task status;
//   - when no Chat ID is configured yet, the sender is told their own id and
//     asked to add it in the panel.
func botRoute(text, chatID, configured string) (cmd string, reply, needID bool) {
	cmd = botCommand(text)
	if cmd == "" {
		return "", false, false
	}
	switch cmd {
	case "/id", "/start", "/help":
		return cmd, true, false
	}
	if configured == "" {
		return cmd, false, true
	}
	if chatID != configured {
		return cmd, false, false
	}
	return cmd, true, false
}

// botCommand normalises a message into a lower-case command, stripping the
// "@botname" suffix Telegram adds in group chats.
func botCommand(text string) string {
	fields := strings.Fields(strings.TrimSpace(text))
	if len(fields) == 0 {
		return ""
	}
	cmd := strings.ToLower(fields[0])
	if i := strings.IndexByte(cmd, '@'); i >= 0 {
		cmd = cmd[:i]
	}
	return cmd
}

// ---- HTTP handlers -------------------------------------------------------

func (s *Server) handleSyncStatus(w http.ResponseWriter, r *http.Request) {
	writeOK(w, s.buildSyncStatus())
}

func (s *Server) handleListNotifications(w http.ResponseWriter, r *http.Request) {
	limit := 100
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = n
		}
	}
	writeOK(w, s.store.ListNotifications(limit))
}

type testTelegramRequest struct {
	BotToken string `json:"bot_token"`
	ChatID   string `json:"chat_id"`
}

func (s *Server) handleTestTelegram(w http.ResponseWriter, r *http.Request) {
	var req testTelegramRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	tg := s.store.Settings().Telegram
	token := strings.TrimSpace(req.BotToken)
	if token == "" {
		token = tg.BotToken
	}
	chatID := strings.TrimSpace(req.ChatID)
	if chatID == "" {
		chatID = tg.ChatID
	}
	if token == "" || chatID == "" {
		writeErr(w, http.StatusBadRequest, "请先填写 Bot Token 与 Chat ID")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	text := "✅ <b>测试消息</b>\nOpenList 备份面板已成功连接 Telegram 机器人。"
	if err := s.sendAndRecord(ctx, types.TelegramSettings{BotToken: token, ChatID: chatID}, text, "info", "连接测试"); err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	writeOK(w, map[string]bool{"ok": true})
}

type tgChatInfo struct {
	ID          string `json:"id"`
	Type        string `json:"type"`
	Name        string `json:"name"`
	Username    string `json:"username"`
	LastMessage string `json:"last_message"`
}

// recordChat remembers a chat the bot has seen, newest last.
func (s *Server) recordChat(c tgChat) {
	if c.ID == 0 {
		return
	}
	s.chatMu.Lock()
	defer s.chatMu.Unlock()
	if s.chats == nil {
		s.chats = make(map[int64]tgChatInfo)
	}
	if _, ok := s.chats[c.ID]; !ok {
		s.chatSeen = append(s.chatSeen, c.ID)
	}
	s.chats[c.ID] = tgChatInfo{
		ID:       strconv.FormatInt(c.ID, 10),
		Type:     c.Type,
		Name:     c.displayName(),
		Username: c.Username,
	}
}

// recentChats returns remembered chats, newest first.
func (s *Server) recentChats() []tgChatInfo {
	s.chatMu.Lock()
	defer s.chatMu.Unlock()
	out := make([]tgChatInfo, 0, len(s.chatSeen))
	for i := len(s.chatSeen) - 1; i >= 0; i-- {
		if c, ok := s.chats[s.chatSeen[i]]; ok {
			out = append(out, c)
		}
	}
	return out
}

// handleListTelegramChats lists chats that messaged the bot, so the operator
// can pick a Chat ID instead of hunting for it with a third-party bot.
func (s *Server) handleListTelegramChats(w http.ResponseWriter, r *http.Request) {
	// Prefer chats the running bot already saw: a fresh getUpdates may come
	// back empty because the bot confirms (and consumes) updates.
	if chats := s.recentChats(); len(chats) > 0 {
		writeOK(w, map[string]any{"ok": true, "chats": chats})
		return
	}
	token := strings.TrimSpace(r.URL.Query().Get("token"))
	if token == "" {
		token = s.store.Settings().Telegram.BotToken
	}
	if token == "" {
		writeErr(w, http.StatusBadRequest, "请先填写 Bot Token")
		return
	}
	chats, err := s.fetchChats(r.Context(), token)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	writeOK(w, map[string]any{"ok": true, "chats": chats})
}

// fetchChats asks Telegram for pending updates and extracts distinct chats.
// timeout=0 returns immediately; offset=0 leaves updates unconfirmed so the
// long-polling bot still processes them.
func (s *Server) fetchChats(ctx context.Context, token string) ([]tgChatInfo, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	raw, err := s.tgCall(ctx, token, "getUpdates", map[string]any{
		"offset":          0,
		"timeout":         0,
		"allowed_updates": []string{"message"},
	})
	if err != nil {
		return nil, err
	}
	var ups []tgUpdate
	if err := json.Unmarshal(raw, &ups); err != nil {
		return nil, fmt.Errorf("解析 Telegram 返回失败")
	}
	seen := make(map[int64]bool)
	chats := make([]tgChatInfo, 0)
	for _, u := range ups {
		if u.Message == nil || seen[u.Message.Chat.ID] {
			continue
		}
		seen[u.Message.Chat.ID] = true
		c := u.Message.Chat
		s.recordChat(c)
		chats = append(chats, tgChatInfo{
			ID:          strconv.FormatInt(c.ID, 10),
			Type:        c.Type,
			Name:        c.displayName(),
			Username:    c.Username,
			LastMessage: tgFirstLine(u.Message.Text),
		})
	}
	return chats, nil
}

func tgFirstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if r := []rune(s); len(r) > 40 {
		s = string(r[:40]) + "…"
	}
	return s
}

// ---- small formatting helpers --------------------------------------------

func humanBytes(n int64) string {
	if n < 0 {
		n = 0
	}
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	units := []string{"KB", "MB", "GB", "TB", "PB"}
	f := float64(n)
	i := -1
	for f >= unit && i < len(units)-1 {
		f /= unit
		i++
	}
	if f >= 10 {
		return fmt.Sprintf("%.0f %s", f, units[i])
	}
	return fmt.Sprintf("%.1f %s", f, units[i])
}

func runDuration(run types.Run) string {
	if run.StartedAt.IsZero() || run.FinishedAt.IsZero() {
		return ""
	}
	d := run.FinishedAt.Sub(run.StartedAt)
	if d < 0 {
		return ""
	}
	s := int(d.Seconds())
	if s < 60 {
		return fmt.Sprintf("%d 秒", s)
	}
	if m := s / 60; m < 60 {
		return fmt.Sprintf("%d 分 %d 秒", m, s%60)
	}
	m := s / 60
	return fmt.Sprintf("%d 时 %d 分", m/60, m%60)
}

func humanAgo(t time.Time) string {
	if t.IsZero() {
		return "从未"
	}
	d := time.Since(t)
	if d < time.Minute {
		return "刚刚"
	}
	if d < time.Hour {
		return fmt.Sprintf("%d 分钟前", int(d.Minutes()))
	}
	if d < 24*time.Hour {
		return fmt.Sprintf("%d 小时前", int(d.Hours()))
	}
	return fmt.Sprintf("%d 天前", int(d.Hours()/24))
}
