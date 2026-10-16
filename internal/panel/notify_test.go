package panel

import (
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"opback/internal/store"
	"opback/internal/types"
)

func TestFormatRunMessage(t *testing.T) {
	start := time.Date(2026, 10, 16, 4, 0, 0, 0, time.UTC)
	run := types.Run{
		ID:           "r1",
		JobName:      "web <backup>",
		NodeName:     "web-1",
		OpenListName: "主连接",
		Status:       types.RunFailed,
		StartedAt:    start,
		FinishedAt:   start.Add(90 * time.Second),
		FilesDone:    3,
		FilesSkipped: 1,
		FilesFailed:  2,
		BytesDone:    1536,
		Message:      "boom & bust",
	}
	msg := formatRunMessage(run)
	for _, want := range []string{
		"备份失败",
		"web &lt;backup&gt;",
		"web-1",
		"主连接",
		"3 上传 / 1 跳过 / 2 失败",
		"1.5 KB",
		"1 分 30 秒",
		"boom &amp; bust",
	} {
		if !strings.Contains(msg, want) {
			t.Fatalf("formatRunMessage missing %q in:\n%s", want, msg)
		}
	}
}

func TestFormatRunMessageSuccessIsCompact(t *testing.T) {
	run := types.Run{
		JobName:      "tg-image",
		NodeName:     "4c-hk",
		OpenListName: "默认连接",
		Status:       types.RunSuccess,
		StartedAt:    time.Now(),
		FilesSkipped: 3,
		Message:      "完成：上传 0，跳过 3，共 0 字节",
	}
	msg := formatRunMessage(run)
	if strings.Contains(msg, "信息：") || strings.Contains(msg, "完成：") {
		t.Fatalf("a successful run should omit the redundant note:\n%s", msg)
	}
	if n := strings.Count(msg, "\n"); n != 2 {
		t.Fatalf("want 3 compact lines (2 newlines), got %d:\n%s", n, msg)
	}
}

func TestNotifySelection(t *testing.T) {
	tg := types.TelegramSettings{NotifySuccess: true, NotifyFailure: false}
	if !shouldNotify(tg, types.RunSuccess) {
		t.Fatalf("success should notify")
	}
	if !shouldNotify(tg, types.RunPartial) {
		t.Fatalf("partial should notify like success")
	}
	if shouldNotify(tg, types.RunFailed) {
		t.Fatalf("failure notifications are off")
	}
	if shouldNotify(tg, types.RunRunning) {
		t.Fatalf("running runs must not notify")
	}

	for _, s := range []string{types.RunSuccess, types.RunPartial, types.RunFailed} {
		if !isTerminalRun(s) {
			t.Fatalf("%s should be terminal", s)
		}
	}
	if isTerminalRun(types.RunRunning) {
		t.Fatalf("running must not be terminal")
	}
}

func TestFormatStatusMessage(t *testing.T) {
	ss := syncStatusResponse{
		GeneratedAt: time.Now(),
		NodesTotal:  2,
		NodesOnline: 1,
		JobsTotal:   2,
		Success:     1,
		Failed:      0,
		Never:       1,
		Items: []syncStatusItem{
			{JobName: "alpha", NodeName: "n1", HasRun: true, Status: types.RunSuccess, StartedAt: time.Now().Add(-2 * time.Minute), BytesDone: 2048},
			{JobName: "beta", NodeName: "n2", HasRun: false, Enabled: true},
		},
	}
	msg := formatStatusMessage(ss)
	for _, want := range []string{"任务同步状态", "在线节点：1/2", "成功 1 · 失败 0 · 未运行 1", "alpha (n1)", "成功 · 2 分钟前", "beta (n2)", "尚未运行"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("formatStatusMessage missing %q in:\n%s", want, msg)
		}
	}
	if strings.Contains(msg, "<pre>") {
		t.Fatalf("formatStatusMessage should not use a code block:\n%s", msg)
	}
}

func TestHumanInterval(t *testing.T) {
	cases := map[int]string{30: "每 30 秒", 60: "每 1 分钟", 90: "每 1 分 30 秒", 3600: "每 1 小时", 7200: "每 2 小时", 86400: "每 1 天", 172800: "每 2 天"}
	for in, want := range cases {
		if got := humanInterval(in); got != want {
			t.Fatalf("humanInterval(%d) = %q, want %q", in, got, want)
		}
	}
	if got := humanPlan(3600, false); got != "已停用" {
		t.Fatalf("disabled job plan = %q, want 已停用", got)
	}
	if got := humanPlan(0, true); got != "仅手动" {
		t.Fatalf("manual job plan = %q, want 仅手动", got)
	}
}

func TestHistorySquare(t *testing.T) {
	cases := map[string]string{
		types.RunSuccess: "🟩",
		types.RunPartial: "🟨",
		types.RunFailed:  "🟥",
		types.RunRunning: "⬜",
	}
	for status, want := range cases {
		if got := historySquare(status); got != want {
			t.Fatalf("historySquare(%s) = %q, want %q", status, got, want)
		}
	}
}

func TestBuildTasksAndSyncMessages(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "store.json"))
	if err != nil {
		t.Fatal(err)
	}
	node, err := st.CreateNode("n1", "n1", "linux", "amd64", "test")
	if err != nil {
		t.Fatal(err)
	}
	job, err := st.CreateJob(types.Job{
		Name: "docs", NodeID: node.ID, Source: "/srv/docs", Dest: "/backups/docs",
		Interval: 3600, Enabled: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	s := New(st)

	// /tasks renders a monospace table with one row per job.
	tasks := s.buildTasksMessage()
	for _, want := range []string{"任务列表", "<pre>", "任务名称", "OpenList", "源路径", "目标路径", "计划", "docs (n1)", "/srv/docs", "/backups/docs", "每 1 小时"} {
		if !strings.Contains(tasks, want) {
			t.Fatalf("buildTasksMessage missing %q in:\n%s", want, tasks)
		}
	}

	// /sync shows a square per run (oldest first) plus a success rate.
	base := time.Now().Add(-10 * time.Minute).UTC()
	for i, status := range []string{types.RunSuccess, types.RunFailed, types.RunSuccess} {
		if err := st.UpsertRun(types.Run{
			ID: "r" + strconv.Itoa(i), JobID: job.ID, JobName: job.Name,
			Status: status, StartedAt: base.Add(time.Duration(i) * time.Minute),
		}); err != nil {
			t.Fatal(err)
		}
	}
	hist := s.buildSyncHistoryMessage()
	for _, want := range []string{"同步历史", "成功率 66%", "🟩🟥🟩"} {
		if !strings.Contains(hist, want) {
			t.Fatalf("buildSyncHistoryMessage missing %q in:\n%s", want, hist)
		}
	}
	if strings.Contains(hist, "<pre>") {
		t.Fatalf("buildSyncHistoryMessage should not use a code block:\n%s", hist)
	}
}

func TestHumanBytesAndDuration(t *testing.T) {
	cases := map[int64]string{0: "0 B", 1023: "1023 B", 1024: "1.0 KB", 1536: "1.5 KB", 1048576: "1.0 MB", 10 * 1048576: "10 MB"}
	for in, want := range cases {
		if got := humanBytes(in); got != want {
			t.Fatalf("humanBytes(%d) = %q, want %q", in, got, want)
		}
	}

	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	durCases := []struct {
		d    time.Duration
		want string
	}{
		{45 * time.Second, "45 秒"},
		{90 * time.Second, "1 分 30 秒"},
		{2 * time.Hour, "2 时 0 分"},
	}
	for _, c := range durCases {
		run := types.Run{StartedAt: start, FinishedAt: start.Add(c.d)}
		if got := runDuration(run); got != c.want {
			t.Fatalf("runDuration(%s) = %q, want %q", c.d, got, c.want)
		}
	}
}

func TestBotRoute(t *testing.T) {
	cases := []struct {
		text       string
		chatID     string
		configured string
		wantCmd    string
		wantReply  bool
		wantNeedID bool
	}{
		// /id works during setup, even before a Chat ID is configured.
		{"/id", "42", "", "/id", true, false},
		{"/id@my_bot", "42", "", "/id", true, false},
		// /start and /help are always answered.
		{"/start", "42", "", "/start", true, false},
		{"/help", "42", "99", "/help", true, false},
		// Status commands prompt for a Chat ID before one is configured.
		{"/status", "42", "", "/status", false, true},
		{"/sync", "42", "", "/sync", false, true},
		{"/tasks", "42", "", "/tasks", false, true},
		// ...and are silently ignored from any other chat once configured.
		{"/status", "42", "99", "/status", false, false},
		// Once configured and matching, they are answered.
		{"/status", "99", "99", "/status", true, false},
		{"/STATUS", "99", "99", "/status", true, false},
		// Blank input is never answered.
		{"   ", "99", "99", "", false, false},
	}
	for _, c := range cases {
		cmd, reply, needID := botRoute(c.text, c.chatID, c.configured)
		if cmd != c.wantCmd || reply != c.wantReply || needID != c.wantNeedID {
			t.Fatalf("botRoute(%q, chat=%q, configured=%q) = (%q, %v, %v), want (%q, %v, %v)",
				c.text, c.chatID, c.configured, cmd, reply, needID, c.wantCmd, c.wantReply, c.wantNeedID)
		}
	}
}

func TestNeedChatIDMessage(t *testing.T) {
	msg := needChatIDMessage("-1001234")
	if !strings.Contains(msg, "-1001234") || !strings.Contains(msg, "请先在面板添加 Chat ID") {
		t.Fatalf("needChatIDMessage should include the id and prompt:\n%s", msg)
	}
}

func TestHelpMessageMentionsID(t *testing.T) {
	for _, ready := range []bool{true, false} {
		msg := helpMessage(ready)
		if !strings.Contains(msg, "/id") {
			t.Fatalf("helpMessage(%v) should mention /id:\n%s", ready, msg)
		}
	}
	if !strings.Contains(helpMessage(false), "尚未配置 Chat ID") {
		t.Fatalf("helpMessage(false) should warn about the missing Chat ID")
	}
	if strings.Contains(helpMessage(true), "尚未配置 Chat ID") {
		t.Fatalf("helpMessage(true) should not warn about the Chat ID")
	}
}

func TestChatCache(t *testing.T) {
	s := New(nil)
	if got := s.recentChats(); len(got) != 0 {
		t.Fatalf("fresh server should have no chats, got %d", len(got))
	}
	s.recordChat(tgChat{ID: 1, Type: "private", FirstName: "Ada", Username: "ada"})
	s.recordChat(tgChat{ID: -100, Type: "group", Title: "Ops"})
	s.recordChat(tgChat{ID: 1, Type: "private", FirstName: "Ada", Username: "ada"}) // duplicate

	got := s.recentChats()
	if len(got) != 2 {
		t.Fatalf("want 2 chats, got %d: %+v", len(got), got)
	}
	// Newest first: the group was recorded after Ada.
	if got[0].ID != "-100" || got[0].Name != "Ops" {
		t.Fatalf("first chat = %+v, want the group", got[0])
	}
	if got[1].ID != "1" || got[1].Name != "Ada" {
		t.Fatalf("second chat = %+v, want Ada", got[1])
	}
}

func TestRenderTableAlignment(t *testing.T) {
	out := renderTable(
		[]string{"任务名称", "状态"},
		[][]string{{"a", "ok"}, {"较长名称", "failed"}},
	)
	lines := strings.Split(out, "\n")
	if len(lines) != 3 {
		t.Fatalf("want header + 2 rows, got:\n%s", out)
	}
	want := displayWidth(lines[0])
	for _, ln := range lines {
		if got := displayWidth(ln); got != want {
			t.Fatalf("row %q width %d, want %d:\n%s", ln, got, want, out)
		}
	}
	if renderTable([]string{"x"}, nil) != "" {
		t.Fatalf("empty table should render nothing")
	}
}
