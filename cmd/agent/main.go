// Command agent runs on each machine that should be backed up. It registers
// with the panel, pulls its backup jobs, and uploads files to OpenList on
// schedule (or once, with -once).
package main

import (
	"context"
	"errors"
	"flag"
	"log"
	"os"
	"os/signal"
	"syscall"

	"opback/internal/agent"
)

func main() {
	panelURL := flag.String("panel-url", envOr("OPBACK_PANEL_URL", ""), "面板地址，例如 http://10.0.0.1:8080（必填）")
	name := flag.String("name", envOr("OPBACK_NAME", ""), "本节点在面板中显示的名称（默认使用主机名）")
	enrollToken := flag.String("enroll-token", envOr("OPBACK_ENROLL_TOKEN", ""), "面板注册密钥（面板启用时必填）")
	stateDir := flag.String("state-dir", envOr("OPBACK_STATE_DIR", "./agent-state"), "状态目录（保存令牌、增量索引等）")
	serviceName := flag.String("service-name", envOr("OPBACK_SERVICE_NAME", "opback-agent"), "systemd 服务名（收到远程卸载指令时用于停止并移除自身）")
	once := flag.Bool("once", false, "只执行一轮所有启用的任务后退出（适合搭配系统 cron）")

	flag.Parse()

	ag, err := agent.New(agent.Config{
		PanelURL:    *panelURL,
		Name:        *name,
		EnrollToken: *enrollToken,
		StateDir:    *stateDir,
		ServiceName: *serviceName,
		Once:        *once,
	})
	if err != nil {
		log.Fatalf("初始化失败: %v", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	err = ag.Run(ctx)
	if errors.Is(err, agent.ErrUninstalled) {
		log.Printf("agent 已按面板指令卸载，进程退出")
		return
	}
	if err != nil && err != context.Canceled {
		log.Fatalf("agent 退出: %v", err)
	}
	log.Printf("agent 已停止")
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
