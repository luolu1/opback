package panel

import (
	"fmt"
	"strings"

	"opback/internal/types"
)

// This file renders the bot's /status, /tasks and /sync answers.
//
// /status and /sync are plain text lists: they read well on mobile and avoid
// the tappable "copy" affordance Telegram adds to code blocks. /tasks, on the
// other hand, is a 5-column table whose plain-text form wraps awkwardly on
// narrow screens, so it is rendered inside a monospace <pre> block where the
// columns stay aligned (and the client can scroll instead of wrapping).

// ---- display-width helpers (for the /tasks table) ------------------------

// runeWidth approximates the terminal cell width of r: East Asian wide and
// emoji glyphs occupy two cells, everything else one.
func runeWidth(r rune) int {
	if r == 0x200D || (r >= 0xFE00 && r <= 0xFE0F) { // ZWJ, variation selectors
		return 0
	}
	switch {
	case r >= 0x1100 && r <= 0x115F, // Hangul Jamo
		r >= 0x2E80 && r <= 0x303E, // CJK radicals, Kangxi
		r >= 0x3041 && r <= 0x33FF, // Hiragana .. CJK compatibility
		r >= 0x3400 && r <= 0x4DBF, // CJK extension A
		r >= 0x4E00 && r <= 0x9FFF, // CJK unified ideographs
		r >= 0xA000 && r <= 0xA4CF, // Yi
		r >= 0xAC00 && r <= 0xD7A3, // Hangul syllables
		r >= 0xF900 && r <= 0xFAFF, // CJK compatibility ideographs
		r >= 0xFE30 && r <= 0xFE4F, // CJK compatibility forms
		r >= 0xFF00 && r <= 0xFF60, // Fullwidth forms
		r >= 0xFFE0 && r <= 0xFFE6,
		r >= 0x1F000 && r <= 0x1FAFF, // emoji, symbols, pictographs
		r >= 0x2600 && r <= 0x27BF,   // misc symbols & dingbats (✅ ❌ ⚠)
		r >= 0x2B00 && r <= 0x2BFF:   // arrows & geometric shapes (⬜ ⬛)
		return 2
	}
	return 1
}

func displayWidth(s string) int {
	w := 0
	for _, r := range s {
		w += runeWidth(r)
	}
	return w
}

func padRight(s string, w int) string {
	if d := displayWidth(s); d < w {
		return s + strings.Repeat(" ", w-d)
	}
	return s
}

// truncateDisplay cuts s to at most w cells, appending an ellipsis when shortened.
func truncateDisplay(s string, w int) string {
	if w <= 0 || displayWidth(s) <= w {
		return s
	}
	var b strings.Builder
	used := 0
	for _, r := range s {
		rw := runeWidth(r)
		if used+rw > w-1 {
			break
		}
		b.WriteRune(r)
		used += rw
	}
	return b.String() + "…"
}

// renderTable lays rows out in aligned columns. Cells are HTML-escaped so the
// result is safe to drop inside a <pre> block.
func renderTable(headers []string, rows [][]string) string {
	if len(rows) == 0 {
		return ""
	}
	cols := len(headers)
	cells := make([][]string, len(rows))
	widths := make([]int, cols)
	for i, h := range headers {
		widths[i] = displayWidth(tgEscape(h))
	}
	for r, row := range rows {
		cells[r] = make([]string, cols)
		for c := 0; c < cols; c++ {
			if c < len(row) {
				cells[r][c] = tgEscape(row[c])
			}
			if w := displayWidth(cells[r][c]); w > widths[c] {
				widths[c] = w
			}
		}
	}
	var b strings.Builder
	for c := 0; c < cols; c++ {
		if c > 0 {
			b.WriteString("  ")
		}
		b.WriteString(padRight(tgEscape(headers[c]), widths[c]))
	}
	b.WriteString("\n")
	for _, row := range cells {
		for c := 0; c < cols; c++ {
			if c > 0 {
				b.WriteString("  ")
			}
			b.WriteString(padRight(row[c], widths[c]))
		}
		b.WriteString("\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

func wrapPre(s string) string {
	if s == "" {
		return ""
	}
	return "<pre>" + s + "</pre>"
}

// syncItemLabel renders "job (node)" for a task.
func syncItemLabel(jobName, nodeName string) string {
	if nodeName != "" {
		return jobName + " (" + nodeName + ")"
	}
	return jobName
}

// ---- /status -------------------------------------------------------------

func formatStatusMessage(ss syncStatusResponse) string {
	var b strings.Builder
	b.WriteString("📊 <b>任务同步状态</b>\n")
	fmt.Fprintf(&b, "更新：%s\n", ss.GeneratedAt.Local().Format("2006-01-02 15:04:05"))
	fmt.Fprintf(&b, "在线节点：%d/%d\n", ss.NodesOnline, ss.NodesTotal)
	fmt.Fprintf(&b, "成功 %d · 失败 %d · 未运行 %d\n", ss.Success, ss.Failed, ss.Never)
	if len(ss.Items) == 0 {
		b.WriteString("\n暂无任务。")
		return b.String()
	}
	b.WriteString("\n")
	for _, it := range ss.Items {
		label := tgEscape(syncItemLabel(it.JobName, it.NodeName))
		switch {
		case it.HasRun:
			fmt.Fprintf(&b, "%s <b>%s</b> — %s · %s\n",
				statusIcon(it.Status), label, statusWord(it.Status), humanAgo(it.StartedAt))
		case !it.Enabled:
			fmt.Fprintf(&b, "⚪ <b>%s</b> — 尚未运行（已停用）\n", label)
		default:
			fmt.Fprintf(&b, "⚪ <b>%s</b> — 尚未运行\n", label)
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

// ---- /tasks --------------------------------------------------------------

func (s *Server) buildTasksMessage() string {
	jobs := s.store.ListJobs("")
	olName := make(map[string]string)
	for _, ol := range s.store.ListOpenLists() {
		olName[ol.ID] = ol.Name
	}
	nodeName := make(map[string]string)
	for _, n := range s.store.ListNodes() {
		nodeName[n.ID] = n.Name
	}

	var b strings.Builder
	fmt.Fprintf(&b, "📋 <b>任务列表</b>（共 %d 个）\n", len(jobs))
	if len(jobs) == 0 {
		b.WriteString("\n暂无任务。")
		return b.String()
	}

	rows := make([][]string, 0, len(jobs))
	for _, j := range jobs {
		ol := olName[j.OpenListID]
		if ol == "" {
			ol = "默认连接"
		}
		rows = append(rows, []string{
			truncateDisplay(syncItemLabel(j.Name, nodeName[j.NodeID]), 24),
			truncateDisplay(ol, 14),
			truncateDisplay(j.Source, 22),
			truncateDisplay(j.Dest, 22),
			humanPlan(j.Interval, j.Enabled),
		})
	}
	b.WriteString("\n")
	b.WriteString(wrapPre(renderTable([]string{"任务名称", "OpenList", "源路径", "目标路径", "计划"}, rows)))
	return b.String()
}

// humanPlan renders a job's schedule.
func humanPlan(interval int, enabled bool) string {
	if !enabled {
		return "已停用"
	}
	if interval <= 0 {
		return "仅手动"
	}
	return humanInterval(interval)
}

func humanInterval(sec int) string {
	switch {
	case sec < 60:
		return fmt.Sprintf("每 %d 秒", sec)
	case sec >= 3600 && sec%3600 == 0:
		h := sec / 3600
		if h%24 == 0 {
			return fmt.Sprintf("每 %d 天", h/24)
		}
		return fmt.Sprintf("每 %d 小时", h)
	case sec%60 == 0:
		return fmt.Sprintf("每 %d 分钟", sec/60)
	default:
		return fmt.Sprintf("每 %d 分 %d 秒", sec/60, sec%60)
	}
}

// ---- /sync ---------------------------------------------------------------

// historySquare maps a run status to a coloured square.
func historySquare(status string) string {
	switch status {
	case types.RunSuccess:
		return "🟩"
	case types.RunPartial:
		return "🟨"
	case types.RunFailed:
		return "🟥"
	default:
		return "⬜"
	}
}

func (s *Server) buildSyncHistoryMessage() string {
	jobs := s.store.ListJobs("")
	nodeName := make(map[string]string)
	for _, n := range s.store.ListNodes() {
		nodeName[n.ID] = n.Name
	}

	const window = 10
	var b strings.Builder
	fmt.Fprintf(&b, "📈 <b>同步历史</b>（最近 %d 次）\n", window)
	b.WriteString("🟩 成功 · 🟨 部分失败 · 🟥 失败")
	if len(jobs) == 0 {
		b.WriteString("\n\n暂无任务。")
		return b.String()
	}
	for _, j := range jobs {
		runs := s.store.ListRuns(j.ID, window) // newest first
		b.WriteString("\n\n")
		fmt.Fprintf(&b, "<b>%s</b>\n", tgEscape(syncItemLabel(j.Name, nodeName[j.NodeID])))
		if len(runs) == 0 {
			b.WriteString("暂无记录")
			continue
		}
		var squares strings.Builder
		succ := 0
		for i := len(runs) - 1; i >= 0; i-- { // oldest first, newest last
			squares.WriteString(historySquare(runs[i].Status))
			if runs[i].Status == types.RunSuccess {
				succ++
			}
		}
		fmt.Fprintf(&b, "%s 成功率 %d%%", squares.String(), succ*100/len(runs))
	}
	return b.String()
}
