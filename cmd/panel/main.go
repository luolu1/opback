// Command panel runs the central management panel: the web UI that manages
// nodes and backup jobs, and the API the agents talk to.
//
// The front-end (web UI + admin API) and the back-end (agent API + installer)
// listen on separate host/port pairs. Both are configured through a JSON
// config file; command-line flags override it and environment variables act as
// fallbacks, in the precedence flag > config > env > default.
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"opback/internal/config"
	"opback/internal/panel"
	"opback/internal/store"
)

// version is set at build time via -ldflags "-X main.version=...".
var version = "dev"

func main() {
	configPath := flag.String("config", envOr("OPBACK_CONFIG", "./config.json"), "配置文件路径（JSON，可选）")
	webAddr := flag.String("web-addr", "", "前端监听地址（Web UI + 管理 API），例如 0.0.0.0:8080")
	agentAddr := flag.String("agent-addr", "", "后端监听地址（Agent 接入 + 安装脚本），例如 0.0.0.0:8081")
	addrAlias := flag.String("addr", "", "已弃用：等同 -web-addr")
	dataDir := flag.String("data", "", "数据目录")
	agentDir := flag.String("agent-dir", "", "存放各平台 agent 二进制的目录（供一键安装脚本下载）")

	// Optional one-shot bootstrap values; also settable from the UI later.
	enrollToken := flag.String("enroll-token", "", "Agent 注册密钥（留空表示允许任意 agent 注册）")
	openlistURL := flag.String("openlist-url", "", "OpenList 地址")
	openlistUser := flag.String("openlist-user", "", "OpenList 用户名")
	openlistPass := flag.String("openlist-pass", "", "OpenList 密码")

	// Offline recovery. Any of these makes the panel perform the action and
	// exit without starting the servers.
	resetAdmin := flag.Bool("reset-admin", false, "离线重置：用户名改为 admin，生成新密码，并关闭两步验证，然后退出")
	setUser := flag.String("set-user", "", "离线重置：修改管理员用户名，然后退出")
	setPassword := flag.String("set-password", "", "离线重置：设置管理员密码，然后退出")
	disable2FA := flag.Bool("disable-2fa", false, "离线重置：关闭两步验证，然后退出")

	printVersion := flag.Bool("version", false, "打印版本并退出")

	flag.Parse()

	if *printVersion {
		fmt.Println(version)
		return
	}

	set := map[string]bool{}
	flag.Visit(func(f *flag.Flag) { set[f.Name] = true })

	cfg, err := config.Load(*configPath, config.Default())
	if err != nil {
		log.Fatalf("%v", err)
	}

	// Resolve the data directory (and create it) before opening the store, so
	// the offline reset flags can find it.
	dDir := firstNonEmpty(*dataDir, cfg.DataDir, os.Getenv("OPBACK_DATA"), "./panel-data")
	if err := os.MkdirAll(dDir, 0o750); err != nil {
		log.Fatalf("创建数据目录失败: %v", err)
	}
	st, err := store.Open(filepath.Join(dDir, "store.json"))
	if err != nil {
		log.Fatalf("打开数据存储失败: %v", err)
	}

	if *resetAdmin || *setUser != "" || *setPassword != "" || *disable2FA {
		if err := runAdminReset(st, *resetAdmin, *setUser, *setPassword, *disable2FA); err != nil {
			log.Fatalf("重置失败: %v", err)
		}
		return
	}

	// Web listener: flag > config > env > default.
	resolvedWeb := firstNonEmpty(
		flagOrEmpty(set["web-addr"], *webAddr),
		cfg.Web.Addr(),
		os.Getenv("OPBACK_WEB_ADDR"),
		"",
	)
	if resolvedWeb == "" {
		// Backwards-compatible alias and legacy env var.
		resolvedWeb = firstNonEmpty(flagOrEmpty(set["addr"], *addrAlias), os.Getenv("OPBACK_ADDR"), ":8080")
	}
	// Agent listener.
	resolvedAgent := firstNonEmpty(
		flagOrEmpty(set["agent-addr"], *agentAddr),
		cfg.Agent.Addr(),
		os.Getenv("OPBACK_AGENT_ADDR"),
		":8081",
	)

	aDir := firstNonEmpty(*agentDir, cfg.AgentDir, os.Getenv("OPBACK_AGENT_DIR"), "./bin")

	// Apply bootstrap values only when they carry a value (flag > config > env).
	_ = st.UpdateSettings(func(s *store.Settings) {
		if v := firstNonEmpty(*enrollToken, cfg.EnrollToken, os.Getenv("OPBACK_ENROLL_TOKEN")); v != "" {
			s.EnrollToken = v
		}
	})
	olURL := firstNonEmpty(*openlistURL, cfg.OpenList.URL, os.Getenv("OPBACK_OPENLIST_URL"))
	if olURL != "" {
		if err := st.EnsureDefaultOpenList(olURL,
			firstNonEmpty(*openlistUser, cfg.OpenList.Username, os.Getenv("OPBACK_OPENLIST_USER")),
			firstNonEmpty(*openlistPass, cfg.OpenList.Password, os.Getenv("OPBACK_OPENLIST_PASS"))); err != nil {
			log.Printf("写入 OpenList 连接失败: %v", err)
		}
	}

	srv := panel.New(st)
	srv.SetAgentDir(aDir)
	srv.SetAgentAddr(resolvedAgent)
	srv.SetAgentPublicURL(cfg.AgentPublicURL)
	srv.SetTrustedOrigins(cfg.TrustedOrigins)
	srv.SetVersion(version)
	// Serve the session cookie over TLS only when we actually terminate TLS.
	cookieSecure := cfg.CookieSecure || (cfg.TLSCert != "" && cfg.TLSKey != "")
	srv.SetCookieSecure(cookieSecure)

	// The Telegram bot idles until an operator enables it in Settings.
	go srv.RunBot(context.Background())

	settings := st.Settings()
	if len(st.ListOpenLists()) == 0 {
		log.Printf("提示：尚未配置 OpenList 连接，请登录面板后在“OpenList 连接”中添加。")
	}
	if settings.EnrollToken == "" {
		log.Printf("警告：未设置 Agent 注册密钥，任何能访问后端端口的客户端都可注册节点。建议在“设置”中配置注册密钥。")
	}

	webSrv := &http.Server{Addr: resolvedWeb, Handler: srv.WebHandler()}
	agentSrv := &http.Server{Addr: resolvedAgent, Handler: srv.AgentHandler()}

	log.Printf("OpenList 备份面板已启动")
	log.Printf("  前端 (Web UI + 管理 API): %s", resolvedWeb)
	log.Printf("  后端 (Agent 接入 + 安装脚本): %s", resolvedAgent)
	if st.CheckAdmin(settings.AdminUser, "admin") {
		log.Printf("首次登录默认账号：%s / admin（请尽快修改密码）", settings.AdminUser)
	} else {
		log.Printf("当前管理员：%s", settings.AdminUser)
	}

	errCh := make(chan error, 2)
	go func() {
		if cfg.TLSCert != "" && cfg.TLSKey != "" {
			errCh <- webSrv.ListenAndServeTLS(cfg.TLSCert, cfg.TLSKey)
			return
		}
		errCh <- webSrv.ListenAndServe()
	}()
	go func() { errCh <- agentSrv.ListenAndServe() }()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	select {
	case err := <-errCh:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("服务退出: %v", err)
		}
	case <-sig:
		log.Printf("正在关闭…")
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = webSrv.Shutdown(ctx)
		_ = agentSrv.Shutdown(ctx)
	}
}

// runAdminReset performs offline credential recovery and prints the outcome.
func runAdminReset(st *store.Store, full bool, user, password string, disable2FA bool) error {
	set := st.Settings()
	newUser := set.AdminUser
	newPass := ""
	if full {
		newUser = "admin"
		newPass = randomPassword()
	}
	if u := strings.TrimSpace(user); u != "" {
		newUser = u
	}
	if password != "" {
		newPass = password
	}
	if newPass != "" {
		if len(newPass) < 8 {
			return errors.New("密码至少 8 位")
		}
		if err := st.SetAdminPassword(newUser, newPass); err != nil {
			return err
		}
		fmt.Printf("已设置管理员：%s\n新密码：%s\n", newUser, newPass)
	} else if newUser != set.AdminUser {
		if err := st.SetAdminUser(newUser); err != nil {
			return err
		}
		fmt.Printf("已重命名管理员：%s\n", newUser)
	}
	if disable2FA || full {
		if err := st.DisableTwoFA(); err != nil {
			return err
		}
		fmt.Println("已关闭两步验证（2FA）")
	}
	fmt.Println("完成。请重新启动面板。")
	return nil
}

func randomPassword() string {
	b := make([]byte, 12)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("opback-%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b)
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

// flagOrEmpty returns val only when the flag was explicitly set, so an unset
// flag does not mask a config-file value.
func flagOrEmpty(isSet bool, val string) string {
	if isSet {
		return val
	}
	return ""
}
