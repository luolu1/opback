# 故障排查：Agent 残留

> [English](agent-residue.md) | **简体中文**

删除节点、更新 Agent，或迁移/重命名项目（`openlist-backup` → `opback`）之后，
宿主机上可能残留部分 Agent 组件。本文帮助你**发现**并**彻底清除**这些残留，
让机器要么干净无 Agent，要么干净地**只运行一个** Agent。

一个 Agent 会涉及以下位置：

| 组件 | 路径 / 名称 | 说明 |
| --- | --- | --- |
| systemd 单元 | `/etc/systemd/system/opback-agent.service` | 名称来自 `-service-name`（默认 `opback-agent`） |
| 二进制 | `/usr/local/bin/opback-agent` | 安装脚本使用的路径 |
| 状态目录 | `/var/lib/opback-agent/` | `state.json`（节点 ID + 令牌）与增量索引 |
| 运行进程 | `opback-agent` | `systemctl status opback-agent` |
| 可选 cron | 如 `*/30 * * * * /opt/opback/bin/opback-agent -once` | 仅在 `-once`/cron 模式下存在 |
| 面板记录 | 节点 + 任务 | 服务端，位于 `store.json` |

> **状态目录**决定了 Agent 的「身份」：删除它会触发重新注册与一次全量上传。
> 若只是想更新二进制，可以保留状态目录。

## 0. 重命名历史（残留的来源）

项目已从 **openlist-backup** 重命名为 **opback**。重命名之前安装的 Agent
会以旧名称存在：

| 新名称（`opback`） | 旧名称（`openlist-backup`） |
| --- | --- |
| `/etc/systemd/system/opback-agent.service` | `/etc/systemd/system/openlist-backup-agent.service` |
| `/usr/local/bin/opback-agent` | `/usr/local/bin/openlist-backup-agent` |
| `/var/lib/opback-agent/` | `/var/lib/openlist-backup-agent/` |

旧单元**不会**被 `systemctl status opback-agent` 发现，它会继续以旧身份运行/上报。
排查时请务必**同时**搜索新旧两种名称。

## 1. 检测残留（只读）

在怀疑有残留的机器上执行以下命令，均不会修改系统：

```bash
echo '== systemd 单元（存在 / 已启用 / 失败）=='
systemctl list-unit-files | grep -Ei 'opback|openlist-backup' || echo '  (none)'
systemctl list-units --all  | grep -Ei 'opback|openlist-backup' || echo '  (none)'
systemctl --failed          | grep -Ei 'opback|openlist-backup' || echo '  (none)'

echo '== 磁盘上的单元文件 =='
ls -l /etc/systemd/system/ | grep -Ei 'opback|openlist-backup' || echo '  (none)'

echo '== 运行中的进程 =='
pgrep -af 'opback-agent|openlist-backup-agent' || echo '  (none)'

echo '== 已安装的二进制 =='
ls -l /usr/local/bin/ 2>/dev/null | grep -Ei 'opback|openlist-backup' || echo '  (none)'
ls -l /opt/opback/bin/ 2>/dev/null            | grep -Ei 'opback|openlist-backup' || echo '  (none)'

echo '== 状态目录 =='
ls -ld /var/lib/opback-agent /var/lib/openlist-backup-agent 2>/dev/null || echo '  (none)'

echo '== 定时器与 cron =='
systemctl list-timers --all 2>/dev/null | grep -Ei 'opback|openlist-backup' || echo '  (no timers)'
crontab -l 2>/dev/null                  | grep -Ei 'opback|openlist-backup' || echo '  (no user cron)'
grep -RIl -E 'opback|openlist-backup' /etc/cron.d /etc/cron.daily /etc/cron.hourly 2>/dev/null || echo '  (no cron.d)'
```

**如何判断**

- 单元被列出，但 `list-units` 显示为 `activating`/`auto-restart` 或 `failed`
  → 有损坏/重复的 Agent 在反复重启，视为残留。
- 同时存在新旧两个单元名 → 升级失败，需要删除旧的。
- 有进程在跑、却没有对应单元 → 手工启动或卸载不彻底留下的孤儿进程。

## 2. 清除残留（破坏性操作）

先删旧名称（若存在），再删当前名称。若安装时用过自定义
`-state-dir` / `-service-name`，请相应调整路径。

```bash
# --- 当前名称 ---
sudo systemctl disable --now opback-agent 2>/dev/null
sudo rm -f /etc/systemd/system/opback-agent.service
sudo rm -f /usr/local/bin/opback-agent
sudo rm -rf /var/lib/opback-agent

# --- 旧名称（重命名之前）---
sudo systemctl disable --now openlist-backup-agent 2>/dev/null
sudo rm -f /etc/systemd/system/openlist-backup-agent.service
sudo rm -f /usr/local/bin/openlist-backup-agent
sudo rm -rf /var/lib/openlist-backup-agent

# --- 重载 systemd 并清除失败状态 ---
sudo systemctl daemon-reload
sudo systemctl reset-failed 2>/dev/null

# --- 杀掉没有单元归属的孤儿进程 ---
sudo pkill -f 'opback-agent' 2>/dev/null || true
sudo pkill -f 'openlist-backup-agent' 2>/dev/null || true

# --- cron 残留（仅在用过 -once/cron 时）---
crontab -l 2>/dev/null | grep -v -E 'opback|openlist-backup' | crontab -   # 请先检查！
```

> 上面这条 `crontab` 会重写当前用户 crontab。**请先执行 `crontab -l` 确认**
> 其中确实存在 Agent 条目再运行。

### 「我在面板删了节点，机器上却还在跑 Agent」

面板删除节点会下发一条 **uninstall** 指令。Agent 在下一次 `sync`（≤ sync 间隔，
默认 15 秒）执行：先回调面板，再派生一个**脱离父进程**的清理进程（单元使用
`KillMode=process`，使其在服务停止后仍存活）。若 Agent **离线**，指令永远送不到——
请在面板使用**强制删除**，再按第 2 节清理机器。也可检查是否有未完成的清理进程：

```bash
ps -ef | grep -i 'opback' | grep -v grep
```

## 3. 面板侧残留

Web UI 会保留所有节点与任务记录。机器清理干净后，孤立节点只会显示为**离线**，
无害但碍眼。

- **节点**：在「节点」页删除该行。离线节点请用**强制删除**（否则面板会一直等待
  uninstall 回执）。
- **任务**：删除节点会同时删除它的任务（运行历史保留）。若任务被手工改派过，
  请在「任务」页复查。
- **OpenList 连接**为共享资源，不会因删除节点而变动。

## 4. Docker 残留（面板宿主）

只有**面板**跑在 Docker 里，Agent 是 systemd 服务。但面板仍可能留下容器/镜像/卷：

```bash
cd /opt/opback
docker compose ps -a
docker ps -a     | grep -Ei 'opback' || echo '(no containers)'
docker images    | grep -Ei 'opback' || echo '(no images)'
docker volume ls | grep -Ei 'opback' || echo '(no volumes)'

# 停止并删除容器 + 网络：
docker compose down
# 连同本地构建的镜像一起删除：
docker compose down --rmi local
# 若还用了命名卷（本仓库数据是 bind mount，一般不需要）：
docker volume rm opback_data 2>/dev/null || true
```

> 本仓库的数据以 **bind mount** 挂到 `./data`，删除容器**不会**删除数据；
> 只有确实想推倒重来时才手动删除 `./data`。

## 5. 验证已清理干净

重新执行第 1 节。干净机器在单元、进程、二进制、状态目录各处的输出均为 `(none)`。
之后如需重新接入：

```bash
curl -fsSL 'http://<面板地址>:8081/install.sh' | sudo bash -s -- \
  --panel-url 'http://<面板地址>:8081' --name 'web-1' --enroll-token '<密钥>'
```

确认**只有一个**单元，且节点在面板中变为**在线**并显示连接 IP。
