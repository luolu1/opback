package panel

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

// installScript is served at /install.sh. It downloads the platform-specific
// agent binary from the panel and registers it as a systemd service, so the
// operator only has to paste a single command on the target machine.
//
// Running it again on a machine that already has the service performs an
// in-place upgrade: the binary is replaced and the service restarted, while
// the node's identity (state dir), name and backup jobs are preserved. When
// --name / --enroll-token are omitted they are read back from the existing
// unit, so a bare `curl .../install.sh | sudo bash` is a valid update command.
//
// The script is POSIX sh and is meant to be run as root, e.g.:
//
//	curl -fsSL http://panel:8080/install.sh | sudo bash -s -- \
//	  --panel-url http://panel:8080 --name web-1 --enroll-token SECRET
//
// It must not contain backticks because it is embedded as a Go raw string.
const installScript = `#!/bin/sh
# opback Agent 一键安装脚本（Linux + systemd）
# 由面板自动生成，可通过 /install.sh 获取。
set -eu

PANEL_URL="__PANEL_URL__"
NODE_NAME=""
ENROLL_TOKEN=""
STATE_DIR="/var/lib/opback-agent"
DOWNLOAD_BASE=""
BIN_PATH="/usr/local/bin/opback-agent"
SERVICE_NAME="opback-agent"

usage() {
  cat <<'USAGE'
用法: install.sh [--panel-url URL] [选项]

说明:
  首次安装时用 --name 指定节点名称；在已安装的机器上再次运行本脚本
  （可不带任何参数）即为「就地覆盖升级」：更新 agent 二进制并重启服务，
  保留节点身份与已有备份任务。

选项:
  --panel-url URL       面板地址（默认取脚本来源地址），例如 http://10.0.0.1:8080
  --name NAME           本节点在面板中显示的名称（默认沿用已安装名称，否则用主机名）
  --enroll-token TOKEN  面板注册密钥（面板启用注册密钥时必填；更新时自动沿用）
  --state-dir DIR       状态目录（默认 /var/lib/opback-agent）
  --download-base URL   下载 agent 的来源地址（默认与 --panel-url 相同）
  -h, --help            显示帮助
USAGE
}

while [ $# -gt 0 ]; do
  case "$1" in
    --panel-url)     PANEL_URL="${2:-}"; shift 2 ;;
    --name)          NODE_NAME="${2:-}"; shift 2 ;;
    --enroll-token)  ENROLL_TOKEN="${2:-}"; shift 2 ;;
    --state-dir)     STATE_DIR="${2:-}"; shift 2 ;;
    --download-base) DOWNLOAD_BASE="${2:-}"; shift 2 ;;
    -h|--help)       usage; exit 0 ;;
    *) echo "未知参数: $1" >&2; usage >&2; exit 1 ;;
  esac
done

if [ -z "$PANEL_URL" ]; then
  echo "错误: 缺少 --panel-url" >&2
  usage >&2
  exit 1
fi
# 去掉结尾斜杠
PANEL_URL=$(printf '%s' "$PANEL_URL" | sed 's:/*$::')
[ -n "$DOWNLOAD_BASE" ] || DOWNLOAD_BASE="$PANEL_URL"
DOWNLOAD_BASE=$(printf '%s' "$DOWNLOAD_BASE" | sed 's:/*$::')

if [ "$(id -u)" -ne 0 ]; then
  echo "错误: 请以 root 运行（例如: ... | sudo bash -s -- ...）" >&2
  exit 1
fi

# 平滑更新：若本机已安装过该服务，且这次没有显式给出 --name / --enroll-token，
# 则沿用已有服务里的值。这样重复运行安装命令就是一次「就地覆盖升级」，
# 不会把节点改名，也不会丢失注册密钥（节点身份由 state-dir 保留）。
UNIT_PATH="/etc/systemd/system/$SERVICE_NAME.service"
if [ -f "$UNIT_PATH" ]; then
  if [ -z "$NODE_NAME" ]; then
    NODE_NAME=$(sed -n 's/.* -name "\([^"]*\)".*/\1/p' "$UNIT_PATH" | head -n 1)
  fi
  if [ -z "$ENROLL_TOKEN" ]; then
    ENROLL_TOKEN=$(sed -n 's/.* -enroll-token "\([^"]*\)".*/\1/p' "$UNIT_PATH" | head -n 1)
  fi
fi

if [ -z "$NODE_NAME" ]; then
  NODE_NAME=$(hostname 2>/dev/null || uname -n)
fi

OS=$(uname -s | tr '[:upper:]' '[:lower:]')
if [ "$OS" != "linux" ]; then
  echo "错误: 本脚本仅支持 Linux + systemd（当前系统: $OS）。其他平台请手动部署 agent。" >&2
  exit 1
fi

case "$(uname -m)" in
  x86_64|amd64)        ARCH=amd64 ;;
  aarch64|arm64)       ARCH=arm64 ;;
  armv7l|armv7|armhf)  ARCH=arm ;;
  *) echo "错误: 不支持的架构: $(uname -m)" >&2; exit 1 ;;
esac

if ! command -v systemctl >/dev/null 2>&1; then
  echo "错误: 未找到 systemctl，无法以服务方式安装。" >&2
  exit 1
fi

DL_URL="$DOWNLOAD_BASE/downloads/agent/$OS/$ARCH"
if [ -f "$BIN_PATH" ]; then
  echo "==> 检测到已有安装，执行覆盖更新（保留节点身份与备份任务）"
fi
echo "==> 下载 agent: $DL_URL"
TMP=$(mktemp)
trap 'rm -f "$TMP"' EXIT
if command -v curl >/dev/null 2>&1; then
  curl -fsSL "$DL_URL" -o "$TMP"
elif command -v wget >/dev/null 2>&1; then
  wget -qO "$TMP" "$DL_URL"
else
  echo "错误: 需要 curl 或 wget 才能下载 agent。" >&2
  exit 1
fi

# Write to a temp file in the same directory and rename it into place: this
# atomically replaces a running binary instead of failing with "Text file busy".
mkdir -p "$(dirname "$BIN_PATH")"
TMP_BIN="$(dirname "$BIN_PATH")/.opback-agent.new.$$"
install -m 0755 "$TMP" "$TMP_BIN"
mv -f "$TMP_BIN" "$BIN_PATH"
echo "==> 已安装二进制: $BIN_PATH"

mkdir -p "$STATE_DIR"

ENROLL_ARG=""
if [ -n "$ENROLL_TOKEN" ]; then
  ENROLL_ARG=" -enroll-token \"$ENROLL_TOKEN\""
fi

echo "==> 写入 systemd 服务: /etc/systemd/system/$SERVICE_NAME.service"
cat > "/etc/systemd/system/$SERVICE_NAME.service" <<EOF
[Unit]
Description=opback Agent
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
ExecStart=$BIN_PATH -panel-url "$PANEL_URL" -name "$NODE_NAME" -state-dir "$STATE_DIR" -service-name "$SERVICE_NAME"$ENROLL_ARG
Restart=always
RestartSec=10
# Let the detached uninstall helper survive when the panel removes this node.
KillMode=process

[Install]
WantedBy=multi-user.target
EOF

systemctl daemon-reload
# Always (re)start: "enable --now" alone leaves an already-running instance on
# its old ExecStart, so re-installing (new name / enroll token) would be a no-op.
systemctl enable "$SERVICE_NAME" >/dev/null 2>&1 || true
systemctl restart "$SERVICE_NAME"

echo "==> 安装完成，agent 已作为服务启动。"
echo "    状态: systemctl status $SERVICE_NAME"
echo "    日志: journalctl -u $SERVICE_NAME -f"
`

// handleInstallScript serves the generic installer. It is public so a fresh
// machine can bootstrap itself without first authenticating.
func (s *Server) handleInstallScript(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/x-shellscript; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	body := strings.ReplaceAll(installScript, "__PANEL_URL__", s.agentBaseURL(r))
	_, _ = io.WriteString(w, body)
}

// handleAgentDownload serves a prebuilt agent binary for the requested
// platform. Files live in the configured agent directory (default ./bin) with
// names like opback-agent-linux-amd64.
func (s *Server) handleAgentDownload(w http.ResponseWriter, r *http.Request) {
	osName := strings.ToLower(r.PathValue("os"))
	arch := strings.ToLower(r.PathValue("arch"))
	if !platformToken(osName) || !platformToken(arch) {
		writeErr(w, http.StatusBadRequest, "非法的平台参数")
		return
	}

	name := fmt.Sprintf("opback-agent-%s-%s", osName, arch)
	if osName == "windows" {
		name += ".exe"
	}

	dir := s.agentDir
	if dir == "" {
		dir = "bin"
	}
	f, err := os.Open(filepath.Join(dir, name))
	if err != nil {
		writeErr(w, http.StatusNotFound, "未找到该平台的 agent 二进制，请先在面板机运行 make cross")
		return
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil || fi.IsDir() {
		writeErr(w, http.StatusNotFound, "agent 二进制不可用")
		return
	}

	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", "attachment; filename=\""+name+"\"")
	http.ServeContent(w, r, name, fi.ModTime(), f)
}

// platformToken reports whether s is a safe, simple OS/arch identifier.
func platformToken(s string) bool {
	if s == "" || len(s) > 16 {
		return false
	}
	for _, c := range s {
		if (c < 'a' || c > 'z') && (c < '0' || c > '9') {
			return false
		}
	}
	return true
}
