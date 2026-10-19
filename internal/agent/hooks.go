package agent

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"opback/internal/types"
)

const (
	defaultHookTimeout   = 10 * time.Minute
	maxHookOutputLines   = 50
	maxHookOutputBytes   = 16 << 10
	maxHookErrorLineSize = 300
)

// runHook executes the job's pre or post command through the system shell on
// the agent. Hook output is written to the run log, and a non-zero exit status
// or a timeout is returned as an error.
//
// Post hooks run with a detached context so that they still execute when the
// job was cancelled (for example during agent shutdown): that is what resumes
// a service or removes a temporary dump created by the pre hook.
func runHook(
	ctx context.Context,
	phase string,
	job types.Job,
	trigger string,
	logf func(string, ...any),
) error {
	script := strings.TrimSpace(job.Options.PreCommand)
	if phase == "post" {
		script = strings.TrimSpace(job.Options.PostCommand)
	}
	if script == "" {
		return nil
	}

	label := phaseLabel(phase)
	timeout := time.Duration(job.Options.HookTimeoutSeconds) * time.Second
	if timeout <= 0 {
		timeout = defaultHookTimeout
	}

	base := ctx
	if phase == "post" {
		base = context.WithoutCancel(ctx)
	}
	hctx, cancel := context.WithTimeout(base, timeout)
	defer cancel()

	var cmd *exec.Cmd
	if runtime.GOOS == "windows" {
		cmd = exec.CommandContext(hctx, "cmd", "/C", script)
	} else {
		cmd = exec.CommandContext(hctx, "/bin/sh", "-c", script)
	}
	cmd.Env = append(os.Environ(),
		"OPBACK_JOB_ID="+job.ID,
		"OPBACK_JOB_NAME="+job.Name,
		"OPBACK_SOURCE="+job.Source,
		"OPBACK_DEST="+job.Dest,
		"OPBACK_TRIGGER="+trigger,
		"OPBACK_HOOK="+phase,
	)
	configureHookCmd(cmd)
	// Even if a detached grandchild keeps the output pipe open, do not let the
	// hook block the job forever.
	cmd.WaitDelay = 5 * time.Second
	// Run relative to the source directory so short paths in the command are
	// predictable, but only when that directory actually exists.
	if fi, err := os.Stat(job.Source); err == nil {
		if fi.IsDir() {
			cmd.Dir = job.Source
		} else {
			cmd.Dir = parentDir(job.Source)
		}
	}

	logf("%s命令开始：%s", label, script)
	out, err := cmd.CombinedOutput()
	for i, line := range hookOutputLines(out) {
		if i == 0 {
			logf("%s命令输出：%s", label, line)
		} else {
			logf("  %s", line)
		}
	}

	switch {
	case hctx.Err() == context.DeadlineExceeded:
		return fmt.Errorf("%s命令超时（%s）", label, timeout)
	case hctx.Err() != nil:
		return fmt.Errorf("%s命令被取消：%v", label, hctx.Err())
	case err != nil:
		return fmt.Errorf("%s命令失败：%v（%s）", label, err, hookErrorDetail(out))
	}
	logf("%s命令完成", label)
	return nil
}

func phaseLabel(phase string) string {
	if phase == "post" {
		return "后置"
	}
	return "前置"
}

// hookOutputLines normalises command output into a bounded number of log
// lines, dropping blank lines and truncating very noisy commands.
func hookOutputLines(out []byte) []string {
	if len(out) == 0 {
		return nil
	}
	if len(out) > maxHookOutputBytes {
		out = out[:maxHookOutputBytes]
	}
	raw := strings.ReplaceAll(string(out), "\r\n", "\n")
	raw = strings.ReplaceAll(raw, "\r", "\n")
	var lines []string
	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimRight(line, " \t")
		if strings.TrimSpace(line) == "" {
			continue
		}
		if len(lines) == maxHookOutputLines {
			lines = append(lines, "…（输出已截断）")
			break
		}
		lines = append(lines, line)
	}
	return lines
}

// hookErrorDetail returns the last non-empty output line, which is usually the
// most useful part of a shell command failure.
func hookErrorDetail(out []byte) string {
	lines := hookOutputLines(out)
	if len(lines) == 0 {
		return "无输出"
	}
	last := lines[len(lines)-1]
	if len(last) > maxHookErrorLineSize {
		last = last[:maxHookErrorLineSize] + "…"
	}
	return last
}

func parentDir(p string) string {
	p = strings.TrimRight(p, "/\\")
	if i := strings.LastIndexAny(p, "/\\"); i > 0 {
		return p[:i]
	}
	return ""
}
