# opback

> **简体中文** | [English](README.md)

用「中心管理面板 + 各机器 Agent」替代在每台机器上手动配置 rclone 的备份方案。

- **面板（panel）**：一台机器上运行，提供 Web UI 与 API。用科技风仪表盘统一管理各节点、备份任务、多个 OpenList 连接，并查看详细的运行记录。可选接入 **Telegram 机器人**，推送备份成功/失败并支持命令查询任务同步情况。
- **Agent**：每台需要备份的机器上运行。定时向面板同步，领取任务与命令，把本地文件备份到指定的 OpenList，并回传结果。支持从面板**远程卸载**自身。
- **安全**：**前后端端口分离**（Web UI 与管理 API 一个端口，Agent 接入一个端口），支持 **2FA 两步验证** + 一次性恢复码、登录限流、`Origin`/`Sec-Fetch-Site` 校验、PBKDF2 口令哈希与安全响应头；忘记密码或丢失 2FA 时可用命令行重置。
- **部署**：单文件二进制，支持 **systemd、Docker / Compose、cron 一次性**三种方式；`host`/`port` 由 JSON 配置文件定义。

面板只负责下发配置，**调度在 Agent 本地执行**。因此面板临时宕机不会影响已经下发任务的按点运行，Agent 恢复连接后会自动补报运行结果。

---

## 目录

- [架构](#架构)
- [快速开始](#快速开始)
- [使用说明](#使用说明)
- [部署](#部署)
- [配置项](#配置项)
- [HTTP API](#http-api)
- [安全注意事项](#安全注意事项)
- [故障排查：Agent 残留](docs/agent-residue.zh-CN.md)
- [限制与后续计划](#限制与后续计划)
- [开发](#开发)

---

## 架构

```
                 ┌────────────────────────────────────────┐
                 │                 面板 Panel              │
                 │  前端 :8080   Web UI + 管理 API         │
                 │  后端 :8081   /api/agent/* + /install.sh│
                 │  JSON 持久化 (store.json)               │
                 └──────────────┬─────────────────────────┘
            注册 / 定时 sync ▲   │ 下发任务、命令、OpenList 凭证
                             │   ▼
        ┌────────────────────┼────────────────────┐
        │                    │                    │
   ┌────┴─────┐         ┌────┴─────┐         ┌────┴─────┐
   │  Agent   │         │  Agent   │         │  Agent   │
   │ 本地调度  │         │ 本地调度  │         │ 本地调度  │
   └────┬─────┘         └────┬─────┘         └────┬─────┘
        │  上传（PUT /api/fs/put）                │
        └───────────────────┬───────────────────┘
                            ▼
                    ┌──────────────┐
                    │   OpenList   │
                    └──────────────┘
```

> **前后端端口分离**：Web UI 与管理 API 监听**前端端口**（默认 `:8080`），Agent 接入与一键安装脚本监听**后端端口**（默认 `:8081`）。二者可分别绑定到不同网卡/端口，便于只把后端暴露给待接入的机器，或在前端前置 HTTPS 反代。地址由配置文件（`web.host/port`、`agent.host/port`）定义，也可用 `-web-addr` / `-agent-addr` 覆盖。

面板可选接入 **Telegram 机器人**：把备份结果推送到指定会话，并支持 `/status` 等命令查询各任务的同步情况（详见下文）。

工作流程：

1. Agent 首次启动时用注册密钥（enroll token）向面板 `POST /api/agent/register`，拿到 `node_id` 与长期令牌，存入本地 `state.json`。
2. Agent 每隔一段时间（默认 15 秒，可配置）向面板 `POST /api/agent/sync`。请求里带上最近的运行结果上报（status=running 表示进度，终态表示完成）。
3. 面板在响应里返回该节点的所有启用任务、该节点任务所引用的 **OpenList 连接**（按 ID 提供，支持一个面板对接多个 OpenList 服务器），以及待执行命令（例如「立即运行某任务」「卸载自身」）。
4. Agent 在本地按各任务的 interval 调度执行；到点后遍历源目录，按任务绑定的连接把文件流式上传到对应 OpenList，并把结果发回面板。
5. 面板把运行记录持久化（含节点名、连接名、源/目标路径等细节），Web UI 可查看，超量的旧记录会被裁剪。

### 增量备份

Agent 在每个节点本地维护一份 manifest（相对路径 → 文件大小 + 修改时间秒）。启用「增量」后，若大小与 mtime 都未变化，则跳过上传。首次运行会全量上传，之后仅上传变更文件。

### 目标端清理与保留（回收站）

启用「删除目标端多余文件」后，每次运行结束时会对比目标端与源端：目标端存在、但源端已被删除（或不再被包含/排除规则选中）的文件会被清理。为避免误删导致备份立即消失，可设置**保留天数**：

- **保留天数 > 0**：多余文件不是直接删除，而是移动到目标端的回收站目录 `<目标路径>/.olb-trash/<运行时间戳>/…`，并按原目录结构存放。此后每次运行都会清理回收站中**超过保留天数**的快照。
- **保留天数 = 0**：立即永久删除多余文件。

示例：目标路径 `/backups/web1`，保留 7 天。源端 `sub/gone.txt` 被删除后，它会被移动到 `/backups/web1/.olb-trash/20261016T120000Z/sub/gone.txt`；7 天后的运行会把这个快照整体删除。移动后变空的目录也会被自动清理。

注意：

- `.olb-trash` 是程序管理的目录，**不要**把源数据直接备份到该名字下；清理逻辑会始终忽略它。
- 判定「多余」时会考虑包含/排除规则：被排除的文件（例如 `*.log`）若已存在于目标端，将被视为多余并清理。请确保排除规则符合预期。
- 「多余」只针对源端已消失的文件，**不做**目标端已有的同名但内容过旧文件的版本保留（同名文件按「覆盖」选项处理）。

### 多个 OpenList 连接

面板可同时管理多个 OpenList 服务器：在左侧「OpenList 连接」页面「添加连接」即可配置名称、地址与账号密码，每个连接保存后会自动测试并显示**连接状态**（正常 / 失败 / 未测试）。创建任务时选择一个连接，该任务的上传即发往对应的 OpenList。

- 每个任务通过 `openlist_id` 绑定一个连接；agent 在 sync 时只领取本节点任务所引用的连接凭证。
- 删除一个仍被任务使用的连接会返回冲突提示；可在确认后**强制删除**，相关任务的连接引用会被清空，需要重新指定。
- 为兼容旧数据，面板首次启动时若检测到旧的单一 OpenList 配置，会自动迁移为一个名为「默认连接」的连接。

### 远程卸载节点

在「节点」页面删除一个节点时，面板会执行**丝滑卸载**：

1. 删除该节点名下的所有备份任务；
2. 把节点标记为「卸载中」，并向 agent 下发 `uninstall` 命令；
3. agent 收到命令后先回调面板确认（节点随即从列表移除），再派生一个独立的后台进程，`systemctl disable --now` 停止并禁用服务、删除 systemd unit、二进制与状态目录。

若目标机器离线或清理失败（例如非 systemd / 非 Linux 环境），节点会一直停留在「卸载中」，此时可点「强制删除」仅从面板移除记录（不会影响目标机器，需手动清理）。systemd 单元使用 `KillMode=process`，以保证卸载用的派生进程不会被服务停止时一并杀掉。

### Telegram 机器人推送与查询（可选）

面板内置一个轻量 Telegram 机器人（仅用 Go 标准库实现，无需额外依赖），用于把备份结果推送到手机，并支持用命令查询各任务的同步情况。在「设置」页配置后生效：

- **Bot Token**：在 Telegram 里找 [@BotFather](https://t.me/BotFather) 创建机器人获得。
- **Chat ID**：填好 Bot Token 并**保存**后，直接在 Telegram 里给机器人发送 **`/id`**，它会回复本会话的 Chat ID，把它填到上面即可（推荐）。也可点 Chat ID 输入框旁的**「获取」**按钮，面板会列出最近给机器人发过消息的会话供点选，**选中后会自动保存**，无需再手动点「保存」；群组 ID 通常为负数。
- **推送开关**：可分别开启「备份成功时推送」与「备份失败时推送」。注意：自动推送需要同时配置好 Chat ID 并开启开关。

配置好后：

- **自动推送**：任务进入终态（成功 / 部分失败 / 失败）时，面板会推送一条紧凑的概况：`状态 · 任务名 (节点)`、`文件 上传/跳过/失败 · 数据量 · 耗时`、`连接 · 时间`，失败 / 部分失败时再附一行错误信息（成功时省略，因为与上面的计数重复）。为避免重复，只有「本次终结且上次尚未终结」的运行才会触发推送。
- **命令查询**：给机器人发送以下命令。`/id`、`/start`、`/help` 在配置 Chat ID 前即可使用；`/status`、`/tasks`、`/sync` 在尚未配置 Chat ID 时会回复「请先在面板添加 Chat ID：<你的 ID>」，配置后**只回应上面配置的 Chat ID**（避免 token 泄漏后他人窥探任务状态）：

  | 命令 | 说明 |
  | --- | --- |
  | `/id` | 回复当前会话的 Chat ID（配置前即可用） |
  | `/start`、`/help` | 显示可用命令 |
  | `/status` | 汇总在线节点、各任务最近一次同步状态与时间（纯文本列表） |
  | `/tasks` | 任务清单：任务名、所属 OpenList 连接、源/目标路径、计划（等宽表格） |
  | `/sync` | 各任务最近 10 次同步历史（🟩 成功 / 🟨 部分失败 / 🟥 失败）与成功率 |

- **面板内查看**：同一份聚合状态也出现在 Web 面板的「任务」页「最近同步」列，并在「设置」页展示「推送记录」（含发送结果与失败原因）。

机器人采用后台长轮询（`getUpdates`），无需公网回调地址，面板能访问 `api.telegram.org` 即可。配置 Bot Token 后，面板会自动调用 `setMyCommands` 注册命令菜单，在 Telegram 里输入 `/` 即可看到 `/id`、`/status` 等命令。`/id`、`/start`、`/help` 在未配置 Chat ID 时也会响应，方便先拿到 ID 再完成配置；配置保存后立即生效，无需先发送测试消息。

### 账户安全：2FA 与重置

- **开启 2FA**：登录后在右上角头像菜单进入「个人中心」，点「开启 2FA」。面板会给出一个密钥与 `otpauth://` 链接（可导入验证器），输入验证器生成的 6 位动态码确认即可。开启后登录分为两步：先校验密码，再输入动态码。
- **恢复码**：开启时会一次性给出 **8 个恢复码**（形如 `XXXXX-XXXXX`）。每个恢复码**只能使用一次**，请离线妥善保存，用于手机丢失时登录。可在个人中心「重新生成恢复码」（旧的全部作废）。
- **关闭 2FA / 改密码**：均需输入当前密码；已开启 2FA 时关闭还需提供动态码或一个恢复码。
- **会话失效**：修改密码、开关 2FA 会使**所有已签发的会话 Cookie 立即失效**（需重新登录）。
- **忘记密码 / 丢失 2FA**：在面板机上用命令行重置（命中后直接改数据并退出，不启动服务）：

  ```bash
  # 重置管理员为 admin，生成随机新密码（同时关闭 2FA）；会打印新密码
  ./bin/opback-panel -config config.json -reset-admin

  # 单独修改用户名 / 密码 / 关闭 2FA
  ./bin/opback-panel -config config.json -set-user admin
  ./bin/opback-panel -config config.json -set-password 'new-password'
  ./bin/opback-panel -config config.json -disable-2fa
  ```

  执行重置时请确保对应面板进程**未在运行**（`store.json` 由文件锁保护）。

---

## 快速开始

### 前置条件

- Go 1.24+（仅编译时需要，运行只需编译出的单文件二进制）。
- 一个可用的 OpenList，已知其地址、用户名与密码。

### 1. 编译

```bash
git clone <repo> opback
cd opback
make build
# 产物：
#   bin/opback-panel   面板
#   bin/opback-agent   Agent
```

若想一次编译多平台 Agent 二进制：

```bash
make cross
# bin/opback-agent-linux-amd64
# bin/opback-agent-linux-arm64
# bin/opback-agent-linux-arm
# bin/opback-agent-windows-amd64.exe
# bin/opback-agent-darwin-arm64
```

> 这些二进制会被面板的**一键安装脚本**（`/install.sh`）按平台分发给目标机器。若跳过 `make cross`，目标机执行安装命令时会返回“未找到该平台的 agent 二进制”。

### 2. 启动面板

**前端（Web UI + 管理 API）与后端（Agent 接入 + 安装脚本）端口分离**：前者默认 `:8080`，后者默认 `:8081`。

推荐用一个 JSON 配置文件来定义监听地址（这样 host/port 一目了然，也方便部署时区分内网/外网）：

```bash
cp deploy/config.example.json config.json
# 按需修改 web.host/web.port 与 agent.host/agent.port
./bin/opback-panel -config config.json -data ./panel-data -agent-dir ./bin
```

也可完全用命令行参数 / 环境变量：

```bash
./bin/opback-panel \
  -web-addr :8080 \
  -agent-addr :8081 \
  -data ./panel-data \
  -agent-dir ./bin \
  -enroll-token 'CHANGE-ME-ENROLL' \
  -openlist-url 'https://your-openlist.example.com' \
  -openlist-user 'admin' \
  -openlist-pass 'your-openlist-password'
```

配置的优先级为 **命令行参数 > 配置文件 > 环境变量 > 内置默认值**。`-enroll-token` 可在 Web UI「设置」里修改；`-openlist-*` 是首次写入的便捷方式，会创建/更新一个名为「默认连接」的连接，之后在 Web UI 的「OpenList 连接」页面可管理多个连接。`-agent-dir` 指向存放 `make cross` 产物的目录，供一键安装脚本下载。

浏览器打开 `http://<面板地址>:8080`，用默认账号 **admin / admin** 登录，并**立即修改密码**（可在「个人中心」开启 **2FA 两步验证**）。

### 3. 接入 Agent（在被备份的机器上）

推荐在面板「节点」页点「添加节点」，然后在弹出的窗口点「复制安装命令」，把命令粘贴到目标机器（需 root 且有 systemd）。注意面板里生成的命令已经指向**后端（Agent 接入）端口**：

```bash
curl -fsSL 'http://<面板地址>:8081/install.sh' | sudo bash -s -- \
  --panel-url 'http://<面板地址>:8081' \
  --name 'web-1' \
  --enroll-token 'CHANGE-ME-ENROLL'
```

该脚本会自动：识别系统/架构 → 从面板下载对应 agent 二进制到 `/usr/local/bin/opback-agent`（同目录临时文件 + `mv` 原子替换，避免 `Text file busy`）→ 写入 `/etc/systemd/system/opback-agent.service` → `systemctl enable` + `systemctl restart`。之后可用 `systemctl status opback-agent` / `journalctl -u opback-agent -f` 查看。

> **平滑更新**：同一条安装命令可重复执行。重跑时会从未指定的参数（如 `--name`）沿用旧 unit 里的身份，沿用原节点 ID 与令牌，**无需重新注册**，节点不会重复。因此面板/Agent 发新版后，只需在各节点重新运行一次安装命令即可完成平滑升级（面板「节点」页按钮也显示为「安装/更新」）。

> 也可以从节点列表点任意节点的「安装命令」按钮随时重新获取该命令。面板未设置注册密钥时，命令会自动省略 `--enroll-token`。若面板设置了 `agent_public_url`（或由请求 Host + 后端端口推导），命令里的地址会自动使用对外可达的后端地址。

不想用一键脚本时，也可手动放置二进制后直接运行：

```bash
./bin/opback-agent \
  -panel-url 'http://<面板地址>:8081' \
  -enroll-token 'CHANGE-ME-ENROLL' \
  -name "$(hostname)" \
  -state-dir ./agent-state
```

Agent 启动并完成首次 sync 后，会在面板的「节点」列表中变为**在线**（未接入的节点一直显示离线）。

### 4. 创建备份任务

在面板「任务」页新建任务，填写：

- **节点**：目标 Agent。
- **源路径**：该机器上的本地目录，例如 `/etc/nginx`。
- **目标路径**：OpenList 中的目录，例如 `/backups/web1/nginx`。
- **间隔（秒）**：多久运行一次；填 `0` 表示只手动运行。
- **选项**：增量、覆盖、跟随符号链接、包含/排除 glob、并发数、目标端清理与保留。

保存后 Agent 会在下一次 sync（≤ 15 秒）领取该任务，并按间隔到点运行。点「立即运行」会下发一次性命令，无需等待调度。

---

## 使用说明

### 面板页面

左侧为竖排菜单，包含以下页面：

- **概览**：科技风仪表盘。环形图展示运行状态分布，柱状图展示近 14 天备份量，仪表条展示节点在线率 / 任务启用率 / 连接正常率，并列出最近运行；顶部卡片汇总在线节点、启用任务、累计与 24 小时备份数据量、连接健康度。
- **节点**：已注册 Agent 列表、在线状态（按 `last_seen` 判断，未接入的节点显示离线）、「安装命令」（可复制一键接入命令）、查看令牌、删除节点（远程卸载）。
- **任务**：创建/编辑/删除任务，选择所属 **OpenList 连接**；内嵌 OpenList 目录浏览器可直观选择目标路径；「最近同步」列展示每个任务最近一次运行的状态与时间（与 Telegram `/status` 同源），「记录」按钮查看该任务的同步历史；「立即运行」。
- **运行记录**：每次执行的状态、任务、节点、连接、源/目标路径、触发方式、开始/结束时间、耗时、平均速度、文件与数据量统计、信息与日志；点击可查看完整详情。
- **OpenList 连接**：管理多个 OpenList 服务器，保存后自动测试并显示连接状态；可编辑、测试、删除（在用连接需强制删除）。
- **设置**：注册密钥、sync 间隔、运行记录保留上限、**Telegram 机器人**（开关、Token、Chat ID、成功/失败推送、测试消息）与推送记录。
- **个人中心**（右上角头像菜单）：账户信息、修改用户名 / 密码（需输入当前密码）、**2FA 两步验证**（开启 / 关闭 / 重新生成恢复码）、退出登录。

> 「刷新」与「退出」按钮位于页面右上角：刷新按钮即时重新拉取当前页数据；点击头像可打开菜单进入「个人中心」或退出登录。

### 一次性模式（cron 友好）

若不想常驻进程，可让 Agent 跑一轮就退出，交给系统 cron/计划任务：

```bash
./bin/opback-agent -panel-url http://panel:8081 -once
```

`-once` 会执行该节点上所有启用的任务一次，然后退出。已在面板配置「间隔」< 0 等语义时请注意与 cron 周期的配合（`-once` 不依赖本地 interval）。

### Agent 状态文件

`-state-dir` 下会保存：

- `state.json`：`node_id`、访问令牌、各任务最近运行时间。
- 增量 manifest：记录已上传文件的 size 与 mtime。

删除该目录会让 Agent 重新注册，并触发一次全量上传。

---

## 部署

支持三种方式，可按需选择：

1. **二进制 / systemd**：面板与 Agent 都作为 systemd 服务常驻运行（下文详述，推荐用于生产）。
2. **Docker / Compose**：面板以容器运行（见下文「Docker Compose：面板」），Agent 仍在各机器上以 systemd 运行。
3. **免安装二进制 / cron**：直接运行编译产物；Agent 也可用 `-once` 交给系统 cron 定时执行。

> 本仓库 `deploy/` 目录提供了可直接套用的示例：`opback-panel.service`、`opback-agent.service`、`panel.env.example`、`agent.env.example`、`config.example.json`。下面也一并给出内容。

### systemd：面板

`/etc/systemd/system/opback-panel.service`

```ini
[Unit]
Description=opback Panel
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User=opback
Group=opback
WorkingDirectory=/opt/opback
# 前端 :8080（Web UI + 管理 API），后端 :8081（Agent 接入 + 安装脚本）。
# 也可改用 -config /etc/opback/config.json 统一管理 host/port。
ExecStart=/opt/opback/bin/opback-panel -config /etc/opback/config.json -web-addr :8080 -agent-addr :8081 -data /var/lib/opback
Restart=on-failure
RestartSec=3
# 推荐用环境变量传递敏感值（配合 EnvironmentFile，权限 0600）
# EnvironmentFile=/etc/opback/panel.env

[Install]
WantedBy=multi-user.target
```

### systemd：Agent

`/etc/systemd/system/opback-agent.service`

```ini
[Unit]
Description=opback Agent
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User=root
WorkingDirectory=/opt/opback
ExecStart=/opt/opback/bin/opback-agent -panel-url http://panel.example.com:8081 -state-dir /var/lib/opback-agent -service-name opback-agent
Restart=on-failure
RestartSec=5
# 让远程卸载时派生的清理进程在服务停止后仍然存活
KillMode=process
EnvironmentFile=/etc/opback/agent.env

[Install]
WantedBy=multi-user.target
```

> 若需备份系统目录（如 `/etc`、`/root`），Agent 需有读权限，常见做法是以 root 运行并限制其可访问范围。

`agent.env`（`chmod 600`）：

```
# 指向面板的「后端」（Agent 接入）端口，不是 Web UI 端口
OPBACK_PANEL_URL=http://panel.example.com:8081
OPBACK_ENROLL_TOKEN=CHANGE-ME-ENROLL
OPBACK_NAME=web1
OPBACK_STATE_DIR=/var/lib/opback-agent
```

启用：

```bash
sudo systemctl daemon-reload
sudo systemctl enable --now opback-panel
sudo systemctl enable --now opback-agent
```

### Docker Compose：面板

仓库自带 `Dockerfile`（多阶段构建，同时交叉编译各平台 agent 二进制）、`docker-compose.yml` 与根目录 `config.json`（定义前后端 host/port）。默认约定：

- `config.json` 以只读方式挂载到容器内 `/app/config.json`，并由 `-config` 指定，因此 **host/port 由配置文件定义**。
- 持久化数据挂载到宿主机 `./data`（容器内 `/data`）；Agent 二进制位于容器内 `/app/bin`。
- 端口映射：宿主机 `8080`（前端）/ `8081`（后端）。
- 敏感值（注册密钥、OpenList 凭证）从环境变量 / `.env` 注入，不写入镜像。

```bash
cd /opt/opback
cp .env.example .env      # 修改 OPBACK_ENROLL_TOKEN
docker compose up -d --build
docker compose logs -f panel
```

启动后同样用 `admin / admin` 登录。Agent 的安装命令仍由面板「节点」页生成，`install.sh` 会从容器内的 agent 二进制目录分发。

> 只把后端 `8081` 暴露给需要接入的机器；前端 `8080` 建议限制在内网或置于 HTTPS 反代之后。

### cron 模式下的 Agent

```
*/30 * * * * /opt/opback/bin/opback-agent -panel-url http://panel:8081 -once >> /var/log/opback-agent.log 2>&1
```

---

## 配置项

### 面板（`opback-panel`）

| 参数 | 环境变量 | 默认值 | 说明 |
| --- | --- | --- | --- |
| `-config` | `OPBACK_CONFIG` | `./config.json`（可缺省） | JSON 配置文件路径；文件不存在或为空会忽略 |
| `-web-addr` | `OPBACK_WEB_ADDR` | `:8080` | **前端**监听地址：Web UI + 管理 API |
| `-agent-addr` | `OPBACK_AGENT_ADDR` | `:8081` | **后端**监听地址：Agent 接入 + `/install.sh` + 二进制下载 |
| `-addr` | `OPBACK_ADDR` | — | 已弃用，等价于 `-web-addr`（保留向后兼容） |
| `-data` | `OPBACK_DATA` | `./panel-data` | 数据目录（`store.json`） |
| `-agent-dir` | `OPBACK_AGENT_DIR` | `./bin` | 存放各平台 agent 二进制的目录，供一键安装脚本下载 |
| `-enroll-token` | `OPBACK_ENROLL_TOKEN` | 空 | Agent 注册密钥；留空则允许未持令牌的 Agent 以新节点身份注册（建议设置） |
| `-openlist-url` | `OPBACK_OPENLIST_URL` | 空 | OpenList 地址（会去掉尾部 `/`） |
| `-openlist-user` | `OPBACK_OPENLIST_USER` | 空 | OpenList 用户名 |
| `-openlist-pass` | `OPBACK_OPENLIST_PASS` | 空 | OpenList 密码 |

优先级的完整顺序为 **命令行参数 > 配置文件 > 环境变量 > 内置默认值**。

以上 `-enroll-token` 只在启动时非空才覆盖已保存的设置；`-openlist-*` 会创建/更新名为「默认连接」的连接（同样仅在非空时生效）。

#### 配置文件（JSON）

`web.host/port` 与 `agent.host/port` 用于定义前后端监听地址；文件不存在或为空时不影响启动。样例见 `deploy/config.example.json`：

```json
{
  "web":   { "host": "0.0.0.0", "port": 8080 },
  "agent": { "host": "0.0.0.0", "port": 8081 },

  "data_dir": "/var/lib/opback",
  "agent_dir": "/opt/opback/bin",

  "agent_public_url": "http://panel.example.com:8081",
  "trusted_origins": ["https://panel.example.com"],
  "cookie_secure": false,

  "tls_cert": "",
  "tls_key": "",

  "enroll_token": "CHANGE-ME-ENROLL",

  "openlist": {
    "url": "https://your-openlist.example.com",
    "username": "admin",
    "password": "your-openlist-password"
  }
}
```

| 字段 | 说明 |
| --- | --- |
| `web` / `agent` | 前端 / 后端的 `host`（留空表示所有网卡）与 `port` |
| `data_dir` / `agent_dir` | 等价于 `-data` / `-agent-dir` |
| `agent_public_url` | 生成安装命令时对外使用的后端地址；留空则由请求 Host + 后端端口推导 |
| `trusted_origins` | 额外放行的跨源 Origin（用于反向代理或前后端分域部署） |
| `cookie_secure` | 会话 Cookie 是否带 `Secure`；启用 TLS 时自动为 `true` |
| `tls_cert` / `tls_key` | 同时设置则前端以 HTTPS 启动（后端仍为 HTTP，供内网 Agent 接入） |
| `enroll_token` | Agent 注册密钥 |
| `openlist` | 首次启动写入的「默认连接」凭证 |

> 重置类命令（`-reset-admin` / `-set-user` / `-set-password` / `-disable-2fa`）也会读取 `-config` / `-data`，以便定位同一个 `store.json`。

可在 Web UI「设置」中修改的项：

- Agent 注册密钥。
- **sync 间隔**（秒，默认 15）：Agent 与面板的心跳周期。
- **运行记录上限**（`max_runs`）：超出后自动裁剪最旧的记录。
- **Telegram 机器人**：开关、Bot Token、Chat ID、成功/失败推送开关，以及「发送测试消息」。

管理员用户名与密码可在右上角头像菜单的**个人中心**修改（默认 `admin` / `admin`）。

OpenList 连接已独立到左侧「OpenList 连接」页面管理（可配置多个）。

### Agent（`opback-agent`）

| 参数 | 环境变量 | 默认值 | 说明 |
| --- | --- | --- | --- |
| `-panel-url` | `OPBACK_PANEL_URL` | 空（**必填**） | 面板**后端**（Agent 接入）地址，默认端口 `8081` |
| `-name` | `OPBACK_NAME` | 主机名 | 节点显示名 |
| `-enroll-token` | `OPBACK_ENROLL_TOKEN` | 空 | 面板启用时必填 |
| `-state-dir` | `OPBACK_STATE_DIR` | `./agent-state` | 状态目录 |
| `-service-name` | `OPBACK_SERVICE_NAME` | `opback-agent` | systemd 服务名，收到远程卸载指令时用于停止并移除自身 |
| `-once` | — | `false` | 只跑一轮所有启用任务后退出 |

### 任务选项（`JobOptions`）

| 选项 | 说明 |
| --- | --- |
| `incremental` | 依据本地 manifest（size + mtime）跳过未变更文件 |
| `overwrite` | 允许覆盖目标端已存在的文件 |
| `follow_symlinks` | 遍历时跟随符号链接（含环检测） |
| `exclude` | 相对源根路径的 glob 列表，命中则跳过（`path/filepath.Match` 语义） |
| `include` | 非空时仅备份命中的相对路径 |
| `concurrency` | 并发上传的文件数 |
| `delete_extra` | 删除目标端「多余」文件：目标端存在但源端（经包含/排除筛选后）已不存在的文件 |
| `retention_days` | 保留天数：多余文件先移入回收站，超过该天数才真正删除。`0` 表示立即删除 |
| `pre_command` | 备份前在 Agent 上执行的 shell 命令（数据库转储、暂停服务等）；非零退出则本次不备份 |
| `post_command` | 备份结束后执行的 shell 命令；即使备份失败也会执行（恢复服务、清理临时转储） |
| `hook_timeout_seconds` | 单条前置/后置命令的超时，默认 `600` 秒 |
| `verify_consistency` | 上传后重新 stat 每个文件：上传期间被修改则重试一次，仍变化则把本次运行标记为 `partial` |

### 活跃文件与数据库的一致性备份

直接复制正在写入的数据库文件，可能得到「撕裂」的快照。两个任务选项用于解决这个问题：

- **`pre_command` / `post_command`**：在备份前后于 Agent 上执行 shell 命令。常见做法是把数据库
  转储到源目录（或暂停服务），备份该目录，再在后置命令中清理。每条命令都有超时
  （`hook_timeout_seconds`，默认 600 秒）。前置命令失败时不会上传任何文件；后置命令无论如何都会执行。
- **`verify_consistency`**：上传后重新读取文件大小与 mtime。若在上传期间发生变化，会自动重试一次；
  仍然变化则本次运行标记为 `partial` 并给出告警，而不是静默保留可能不一致的副本。持续变化的文件
  不会写入增量 manifest，下次运行会重新上传。

钩子通过系统 shell（Unix 为 `/bin/sh -c`，Windows 为 `cmd /C`）**以 Agent 运行身份**执行（通常是
root）。环境变量包含 `OPBACK_JOB_ID`、`OPBACK_JOB_NAME`、`OPBACK_SOURCE`、`OPBACK_DEST`、
`OPBACK_TRIGGER`、`OPBACK_HOOK`（`pre`/`post`），工作目录为源目录。请只配置可信命令。

```sh
# SQLite：先生成一致副本，备份暂存目录，再清理。
pre_command:  sqlite3 /var/lib/app/app.db ".backup '/var/backups/app/app.db'"
post_command: rm -f /var/backups/app/app.db
source:       /var/backups/app

# MySQL：在一致性事务中做逻辑转储。
pre_command:  mysqldump --single-transaction --routines --events app > /var/backups/app/app.sql
post_command: rm -f /var/backups/app/app.sql
source:       /var/backups/app

# PostgreSQL：自定义格式转储。
pre_command:  pg_dump -Fc app > /var/backups/app/app.dump
post_command: rm -f /var/backups/app/app.dump
source:       /var/backups/app

# 没有转储工具：暂停服务（会造成停机）。
pre_command:  systemctl stop myservice
post_command: systemctl start myservice
source:       /var/lib/myservice

# LVM/ZFS/Btrfs：创建快照，备份挂载后的快照，再删除。
pre_command:  lvcreate --size 1G --snapshot --name app-snap /dev/vg0/app && mount /dev/vg0/app-snap /mnt/app-snap
post_command: umount /mnt/app-snap && lvremove -f /dev/vg0/app-snap
source:       /mnt/app-snap
```

> 暂存目录（如 `/var/backups/app`）应仅用于该任务，并在 `pre_command` 中清理旧文件，否则历史转储也会被上传。
> 也可用 `include` / `exclude` 进一步限制范围。

---

## HTTP API

面板鉴权：管理接口需要登录会话（HMAC 签名 Cookie）。Agent 接口使用 `X-Node-Token` 头。

### 公共 / 鉴权

| 方法 | 路径 | 说明 |
| --- | --- | --- |
| `POST` | `/api/auth/login` | 登录；已开启 2FA 时返回 `need_2fa` 与一次性 `challenge`，需再调用 `/api/auth/login/2fa` |
| `POST` | `/api/auth/login/2fa` | 使用 `challenge` + 动态码（或恢复码）完成登录 |
| `POST` | `/api/auth/logout` | 退出登录 |
| `GET` | `/api/me` | 当前登录信息（含 `user`、`twofa`、`agent_url`、`version`） |
| `GET` | `/healthz` | 健康检查（前端/后端监听均提供，无需登录） |
| `GET` | `/install.sh` | 一键安装脚本（Linux + systemd），无需登录；由**后端**端口提供 |
| `GET` | `/downloads/agent/{os}/{arch}` | 下载对应平台的 agent 二进制，无需登录；由**后端**端口提供 |

### Agent 接口

| 方法 | 路径 | 说明 |
| --- | --- | --- |
| `POST` | `/api/agent/register` | 注册（校验 enroll token），返回 `node_id` 与令牌 |
| `POST` | `/api/agent/sync` | 心跳：上报运行结果，领取任务、命令与 OpenList 连接凭证 |
| `POST` | `/api/agent/uninstall` | Agent 自卸载前回调，删除对应节点记录（幂等） |

### 管理接口（需登录）

| 方法 | 路径 | 说明 |
| --- | --- | --- |
| `GET` | `/api/overview` | 概览统计 |
| `GET` | `/api/nodes` | 节点列表 |
| `GET` | `/api/nodes/{id}` | 节点详情 |
| `POST` | `/api/nodes` | 手动创建节点 |
| `DELETE` | `/api/nodes/{id}` | 删除节点（默认远程卸载，`?force=1` 仅删除记录） |
| `GET` | `/api/jobs` | 任务列表 |
| `POST` | `/api/jobs` | 创建任务 |
| `GET` | `/api/jobs/{id}` | 任务详情 |
| `PUT` | `/api/jobs/{id}` | 更新任务 |
| `DELETE` | `/api/jobs/{id}` | 删除任务 |
| `POST` | `/api/jobs/{id}/run` | 下发「立即运行」命令 |
| `GET` | `/api/runs` | 运行记录列表 |
| `GET` | `/api/runs/{id}` | 运行详情（含日志） |
| `GET` | `/api/settings` | 读取设置（含 `telegram`、`twofa_enabled`） |
| `PUT` | `/api/settings` | 更新设置（可仅提交 `telegram`） |
| `POST` | `/api/settings/password` | 修改管理员用户名 / 密码（**需** `current_password`；改密码需 ≥ 8 位并返回新会话） |
| `POST` | `/api/settings/2fa/setup` | 生成 2FA 密钥与 `otpauth` 链接（需当前密码） |
| `POST` | `/api/settings/2fa/enable` | 用动态码确认并开启 2FA，返回一次性恢复码 |
| `POST` | `/api/settings/2fa/disable` | 关闭 2FA（需当前密码 + 动态码或恢复码） |
| `POST` | `/api/settings/2fa/recovery` | 重新生成恢复码（需当前密码，旧的作废） |
| `POST` | `/api/settings/telegram/test` | 发送 Telegram 测试消息（可临时传 `bot_token`/`chat_id`） |
| `GET` | `/api/settings/telegram/chats` | 列出机器人见过的会话（优先返回后台机器人记录的会话，可选 `token`），用于自动获取 Chat ID |
| `GET` | `/api/sync-status` | 各任务最近同步状态聚合（供「任务」页与机器人 `/status` 共用） |
| `GET` | `/api/notifications` | 推送记录（可选 `limit`，最多保留 500 条，倒序） |
| `GET` | `/api/openlists` | OpenList 连接列表（密码脱敏，附 `password_set`） |
| `POST` | `/api/openlists` | 新建连接 |
| `GET` | `/api/openlists/{id}` | 连接详情 |
| `PUT` | `/api/openlists/{id}` | 更新连接（密码留空表示不修改） |
| `DELETE` | `/api/openlists/{id}` | 删除连接（被任务引用时返回 409，`?force=1` 强制删除并清空引用） |
| `POST` | `/api/openlists/{id}/test` | 测试连接并记录状态 |
| `GET` | `/api/openlist/list` | 浏览 OpenList 目录（可选 `openlist_id` 指定连接） |

对应 OpenList 底层调用：登录 `POST /api/auth/login`、上传 `PUT /api/fs/put`（`File-Path` 头做 URL 编码，支持分片路径）、列举 `POST /api/fs/list`（`per_page` 上限 100，自动分页）、建目录 `POST /api/fs/mkdir`。鉴权头为 `Authorization: <token>`（不带 `Bearer` 前缀）。API 文档见 <https://fox.oplist.org/>。

---

## 安全注意事项

本系统面向**内网 / 受信链路**，但已内置若干加固；完整审计见 [`SECURITY.zh-CN.md`](SECURITY.zh-CN.md)。要点：

1. **前后端端口分离**：Web UI 与管理 API 只在**前端端口**（默认 `:8080`）；Agent 接入、`/install.sh` 与二进制下载只在**后端端口**（默认 `:8081`）。建议只把后端端口暴露给待接入的机器，前端端口置于内网或 HTTPS 反代之后。
2. **务必修改默认口令** `admin/admin`，强烈建议开启 **2FA**，并设置非空的**注册密钥**（`enroll_token`）。未设置密钥时，同名注册不会接管已有节点，但任意机器仍可注册为新节点。
3. **会话与登录防护**：会话 Cookie 使用 `HttpOnly` / `SameSite=Lax` 的 HMAC 签名令牌；所有「写」类 `/api/*` 请求会校验 `Sec-Fetch-Site` / `Origin`（可用 `trusted_origins` 放行反代域名）；登录失败按来源 IP 限流（默认 5 分钟内 8 次失败后封禁 10 分钟）。
4. **改密/开关 2FA 会使全部会话失效**；管理员密码以 PBKDF2-HMAC-SHA256（210000 次迭代 + 随机盐）存储，旧的 SHA256 哈希会在下次登录时自动升级。
5. **响应头加固**：默认下发 `X-Content-Type-Options: nosniff`、`X-Frame-Options: DENY`、`Referrer-Policy: no-referrer`、`Cross-Origin-Opener-Policy` 与 CSP；启用 TLS（`tls_cert`/`tls_key`）时额外加 `Strict-Transport-Security`，并把 Cookie 标记为 `Secure`。
6. **凭证会被下发到 Agent**：OpenList 密码存于面板 `store.json`（权限 `0600`）并下发给对应节点，Agent 存于 `state.json`（`0600`）。请确保面板↔Agent 链路可信（内网、VPN 或 HTTPS 反代），并限制数据目录权限。
7. **Agent 权限**：以 root 运行 Agent 会使其可读取整机文件，请仅授予必要权限并谨慎配置源路径。任务的
   `pre_command` / `post_command` 钩子同样**以 Agent 运行身份**执行，因此面板管理员可以在每台已注册节点上执行任意 shell 命令。
8. **明文存储**：`store.json` 中的 OpenList 密码与 Telegram Bot Token 目前为明文（依赖文件系统权限保护）；Agent 侧尚未做端到端加密。

> 忘记密码或丢失 2FA 时，可用命令行重置（见[账户安全](#账户安全2fa-与重置)），无需登录。

---

## 限制与后续计划

当前 MVP 的已知限制：

- 目标端清理（`delete_extra`）是**可选**且默认关闭的：开启前请确认目标路径由本任务独占，避免误清理其他来源的文件。
- 回收站按「运行时间戳快照」组织，按整快照过期删除；不支持对单个文件单独设定过期时间。
- 增量判断仅用 size + mtime（秒），不做内容哈希比对；对时间戳敏感的场景需留意。
- 面板与 Agent 之间为**明文 JSON**，未做端到端加密。
- 通知目前仅支持 **Telegram**；通知记录保留最近 500 条。
- 单管理员账号（支持 2FA），无多用户与权限模型。

可考虑的后续增强：

- 目标端同名旧文件的历史版本保留（当前回收站按整批快照过期，不能为单个文件单独设定保留期）。
- 内容哈希校验与断点续传。
- 更多通知渠道（Webhook、邮件、企业微信 / 钉钉等）。
- 任务模板、批量下发、多用户与细粒度权限。

---

## 开发

```bash
make test    # 运行全部测试（含 e2e：真实备份流程 + 面板/Agent HTTP 全链路）
make vet     # go vet
make fmt     # gofmt -w .
make build   # 编译面板与 Agent
make cross   # 交叉编译多平台 Agent
make clean   # 清理 bin/
```

项目仅使用 Go 标准库，无第三方依赖，便于交叉编译为单文件二进制。

关键目录：

```
cmd/panel            面板入口
cmd/agent            Agent 入口
internal/config      JSON 配置文件（web.host/port、agent.host/port、TLS、enroll token 等）
internal/panel       面板：路由、鉴权、管理 API、Agent API、2FA、安全中间件、内嵌 Web UI
internal/agent       Agent：注册、sync、本地调度、备份引擎、增量 manifest
internal/openlist    OpenList 客户端（流式上传、分页列举、自动重登）
internal/store       JSON 持久化与 CRUD（PBKDF2 口令、2FA、会话 epoch）
internal/types       面板与 Agent 共享的数据结构与 sync 协议
internal/e2e         端到端与 HTTP 集成测试
deploy/              systemd 单元、env 与 JSON 配置示例
Dockerfile           Docker 镜像（面板 + 各平台 agent 二进制）
docker-compose.yml   仅部署面板的 Compose 示例
SECURITY.md          安全模型、加固清单与剩余风险
```
