# 安全说明（Security）

> **简体中文** | [English](SECURITY.md)

本文记录 opback 的**安全模型、已实施的加固**与**剩余风险**。系统定位为**内网 / 受信链路**工具，但已按「最小可用加固」原则加入多层防护。

---

## 1. 部署模型与信任边界

```
                前端端口 (:8080)                后端端口 (:8081)
   管理员浏览器 ───────────────┐         ┌─────────────── 各机器 Agent
                              ▼         ▼
                         ┌─────────────────────┐
                         │        面板          │
                         │  会话 Cookie (HMAC)  │
                         │  store.json (0600)   │
                         └─────────────────────┘
```

- **前端端口**（`web`）：Web UI 与管理 API。建议仅内网访问，或置于 HTTPS 反向代理之后。
- **后端端口**（`agent`）：`/api/agent/*`、`/install.sh`、`/downloads/agent/*`。需要让待接入的机器访问。
- 两个端口使用**各自独立的路由表**：管理 API 不会出现在后端监听，agent 接入/安装脚本也不会出现在前端监听（前端对 `/install.sh`、`/downloads/` 显式返回 404）。
- 信任边界：面板与 Agent 之间的网络应可信（内网、VPN 或 TLS 反代）。当前面板↔Agent 之间为**明文 JSON**，未做端到端加密。

---

## 2. 认证与会话

### 管理端（浏览器）

- 登录成功后下发 **HMAC-SHA256 签名令牌**，存于 `HttpOnly` Cookie（`SameSite=Lax`，启用 TLS 时为 `Secure`）。
- 令牌 payload 含签发时间与过期时间（默认 **7 天**），并绑定 **SessionEpoch**。
- **修改密码、开启/关闭 2FA** 会递增 `SessionEpoch`，从而**立即使所有已签发会话失效**。
- 用户名变更**不**使会话失效（便于管理员改名后继续使用）。
- 密码与 2FA 挑战使用**不同令牌类型**（`kind` 字段区分），挑战令牌不能当作会话令牌使用。
- 会话可通过 Cookie 或 `Authorization` 头携带。

### 2FA（TOTP，RFC 6238）

- 标准 TOTP：SHA-1、6 位、30 秒步长，接受 **±1 个窗口**的时钟偏移。
- 开启时生成密钥与 `otpauth://` 链接，并一次性下发 **8 个恢复码**（`XXXXX-XXXXX`）。
- 恢复码经 `HMAC(SessionSecret, 归一化码)` 存储，**一次性消耗**（用后即从存储移除）。
- 登录二步：`POST /api/auth/login` 在开启 2FA 时返回 `need_2fa` + 5 分钟有效的一次性 `challenge`；`POST /api/auth/login/2fa` 接受动态码或恢复码。
- 关闭 2FA / 重新生成恢复码需要当前密码，并（关闭时）验证动态码或恢复码。

### Agent 端

- 注册 `POST /api/agent/register`：校验 `enroll_token`（若配置）。返回 `node_id` 与节点令牌。
- 心跳 `POST /api/agent/sync`：使用 `X-Node-Token` 头鉴权，令牌比对采用**常时比较**。
- **防接管**：当 `enroll_token` 未配置时，同名注册**不会**复用/接管已有节点，而是创建为新节点；只有出示该节点已有令牌时才会复用记录（并轮换出新令牌）。这样即使注册密钥留空，攻击者也无法凭名字顶替现有节点。

---

## 3. 口令存储

- 管理员口令使用 **PBKDF2-HMAC-SHA256**（210000 次迭代、随机盐）存储，格式 `pbkdf2$<iter>$<hex>`。
- 历史遗留的加盐 SHA-256 哈希仍可校验，并会在**下次成功登录时静默升级**为 PBKDF2（升级过程不改变 SessionEpoch）。
- `store.json` 以 `0600` 权限写入。

---

## 4. Web 安全

- **CSRF / 跨源**：所有**写操作** `/api/*` 请求会校验 `Origin` 与 `Sec-Fetch-Site`。同源放行；跨源除非命中 `trusted_origins`（用于反向代理 / 前后端分域部署）否则拒绝。
- **登录限流**：按来源 IP 统计失败次数，默认 **5 分钟内 8 次失败**即封禁 **10 分钟**（内存态）。
- **安全响应头**：`X-Content-Type-Options: nosniff`、`X-Frame-Options: DENY`、`Referrer-Policy: no-referrer`、`Cross-Origin-Opener-Policy: same-origin`、`Content-Security-Policy`（内嵌 UI 自用，禁内联脚本）；启用 TLS 时附加 `Strict-Transport-Security`。
- **口令比较**使用常时比较，避免时序侧信道。
- 前端对 `nosniff` 之外不加载任何第三方资源（无 CDN、无外部字体/脚本）。

---

## 5. 传输与文件权限

- 面板默认明文 HTTP；生产建议启用内置 TLS（`tls_cert`/`tls_key`）或前置 HTTPS 反代。启用内置 TLS 时 Cookie 自动带 `Secure`。
- `store.json`（面板）与 `state.json`（Agent）均以 `0600` 写入。
- OpenList 密码、Telegram Bot Token 目前**明文**存储于 `store.json`，依赖文件系统权限保护。

---

## 6. 重置流程（无需登录）

忘记密码或丢失 2FA 时，在面板机上执行（命中后直接修改数据并退出，不启动服务）：

| 命令 | 作用 |
| --- | --- |
| `-reset-admin` | 用户名重置为 `admin`，生成并打印随机新密码，同时关闭 2FA |
| `-set-user <name>` | 仅修改管理员用户名 |
| `-set-password <pw>` | 仅修改管理员密码 |
| `-disable-2fa` | 仅关闭 2FA |

重置密码或关闭 2FA 会递增 `SessionEpoch`，使既有会话失效。执行时请确保面板进程未运行（`store.json` 由文件锁保护）。

---

## 7. 剩余风险与假设

1. 面板↔Agent 之间未端到端加密；凭证会下发到每个 Agent（存于 `state.json`，权限 `0600`）。请确保链路可信。
2. `store.json` 中的 OpenList 密码与 Telegram Bot Token 为明文，依赖文件权限；未做静态加密。
3. 单管理员、无多用户与细粒度权限模型。
4. `enroll_token` 留空时，任意可达后端的机器仍可注册为**新**节点（无法接管已有节点，但可消耗节点名额）；生产请务必设置。
5. `/install.sh` 与 `/downloads/agent/*` 在后端端口**公开无鉴权**（下载静态脚本/二进制）。这些端点不泄露任何凭证，但请勿把后端端口对公网开放。
6. 口令强度未做复杂度校验（仅要求长度 ≥ 8），2FA 恢复码由管理员自行保管。
7. 任务的 `pre_command` / `post_command` 钩子会**以 Agent 运行身份**（通常是 root）在节点上执行任意 shell 命令。
   能创建或编辑面板任务的人，实际上已在所有已注册节点上获得远程代码执行能力；请保护好管理员账号
   （强口令 + 2FA），并把面板访问权限视为这些主机的 root 权限。

---

## 8. 漏洞反馈

如发现安全问题，请勿公开提交 issue，先通过仓库维护者提供的私下渠道联系，并附上复现步骤、影响范围与版本号（`opback-panel -version`）。
