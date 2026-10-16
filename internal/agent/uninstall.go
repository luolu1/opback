package agent

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// ErrUninstalled is returned by Run after the agent has carried out a
// panel-issued uninstall. The caller should exit cleanly.
var ErrUninstalled = errors.New("agent uninstalled")

// defaultServiceName matches the unit installed by /install.sh.
const defaultServiceName = "opback-agent"

// uninstall notifies the panel, then hands the actual cleanup to a detached
// child process. The child survives the agent's exit (and systemd's stop when
// the unit uses KillMode=process), removing the service, binary and state.
func (a *Agent) uninstall(ctx context.Context) error {
	log.Printf("收到卸载指令，开始自卸载流程")

	// 1. Tell the panel to drop this node right away (best effort).
	if err := a.notifyUninstall(ctx); err != nil {
		log.Printf("通知面板卸载失败（将继续本机清理）：%v", err)
	}

	// 2. Build a platform-specific cleanup script.
	script := a.cleanupScript()
	if script == "" {
		return fmt.Errorf("当前平台 (%s) 不支持自动卸载", runtime.GOOS)
	}

	// 3. Run it detached so it is not killed along with this process.
	if err := detachCleanup("sh", "-c", script); err != nil {
		return fmt.Errorf("启动卸载清理进程失败: %w", err)
	}
	log.Printf("已启动后台清理进程，agent 即将退出")
	return nil
}

// notifyUninstall calls the panel's uninstall endpoint with the node token.
func (a *Agent) notifyUninstall(ctx context.Context) error {
	_, token := a.st.credentials()
	if token == "" {
		return nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		a.cfg.PanelURL+"/api/agent/uninstall", bytes.NewReader([]byte("{}")))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Node-Token", token)

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("http %d", resp.StatusCode)
	}
	return nil
}

// serviceName returns the systemd unit name to disable.
func (a *Agent) serviceName() string {
	if s := strings.TrimSpace(a.cfg.ServiceName); s != "" {
		return s
	}
	return defaultServiceName
}

// agentBinaryPath resolves the absolute path of the running executable.
func agentBinaryPath() string {
	p, err := os.Executable()
	if err != nil {
		return ""
	}
	if abs, err := filepath.Abs(p); err == nil {
		return abs
	}
	return p
}

// cleanupScript returns a POSIX shell script that removes the service, the
// binary and the state directory, or "" when the platform is unsupported.
func (a *Agent) cleanupScript() string {
	switch runtime.GOOS {
	case "linux":
	default:
		return ""
	}
	svc := shellQuote(a.serviceName())
	unit := shellQuote("/etc/systemd/system/" + a.serviceName() + ".service")
	bin := shellQuote(agentBinaryPath())
	state := shellQuote(a.cfg.StateDir)

	// sleep 1 gives the current process time to exit before the binary is
	// removed. Disabling the unit first prevents a restart.
	return fmt.Sprintf(`#!/bin/sh
set -u
sleep 1
if command -v systemctl >/dev/null 2>&1; then
  systemctl disable --now %s >/dev/null 2>&1 || true
fi
rm -f %s
if command -v systemctl >/dev/null 2>&1; then
  systemctl daemon-reload >/dev/null 2>&1 || true
fi
if [ -n %s ]; then rm -f %s; fi
if [ -n %s ]; then rm -rf %s; fi
exit 0
`, svc, unit, bin, bin, state, state)
}

// shellQuote wraps s in single quotes for safe interpolation into sh.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
