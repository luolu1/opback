# Troubleshooting: leftover Agent files ("agent residue")

> **English** | [简体中文](agent-residue.zh-CN.md)

When you remove a node, update the Agent, or rename/migrate the project
(`openlist-backup` → `opback`), some pieces of an Agent can survive on a host.
This guide helps you **detect** and **completely remove** such residue so that a
host is either cleanly empty or cleanly running exactly one Agent.

An Agent touches the following locations:

| Component | Path / name | Notes |
| --- | --- | --- |
| systemd unit | `/etc/systemd/system/opback-agent.service` | name comes from `-service-name` (default `opback-agent`) |
| binary | `/usr/local/bin/opback-agent` | path used by the installer |
| state dir | `/var/lib/opback-agent/` | `state.json` (node id + token) and increment manifests |
| live process | `opback-agent` | `systemctl status opback-agent` |
| optional cron | e.g. `*/30 * * * * /opt/opback/bin/opback-agent -once` | only if you deployed in `-once`/cron mode |
| panel record | Node + Jobs | server side, in `store.json` |

> Removing the **state dir** is what makes an Agent "brand new" — it will
> re-register and trigger a full upload. Clearing it is optional when you only
> want to update the binary.

## 0. Renaming history (why legacy residue exists)

The project was renamed from **openlist-backup** to **opback**. An Agent
installed before the rename leaves everything under the old names:

| New (`opback`) | Legacy (`openlist-backup`) |
| --- | --- |
| `/etc/systemd/system/opback-agent.service` | `/etc/systemd/system/openlist-backup-agent.service` |
| `/usr/local/bin/opback-agent` | `/usr/local/bin/openlist-backup-agent` |
| `/var/lib/opback-agent/` | `/var/lib/openlist-backup-agent/` |

A legacy unit will **not** be found by `systemctl status opback-agent`, and it
keeps running/reporting under the old identity. Always grep for **both** names.

## 1. Detect residue (read-only)

Run this on the host you suspect. Nothing here changes the system.

```bash
echo '== systemd units (present / enabled / failed) =='
systemctl list-unit-files | grep -Ei 'opback|openlist-backup' || echo '  (none)'
systemctl list-units --all  | grep -Ei 'opback|openlist-backup' || echo '  (none)'
systemctl --failed          | grep -Ei 'opback|openlist-backup' || echo '  (none)'

echo '== unit files on disk =='
ls -l /etc/systemd/system/ | grep -Ei 'opback|openlist-backup' || echo '  (none)'

echo '== running processes =='
pgrep -af 'opback-agent|openlist-backup-agent' || echo '  (none)'

echo '== installed binaries =='
ls -l /usr/local/bin/ 2>/dev/null | grep -Ei 'opback|openlist-backup' || echo '  (none)'
ls -l /opt/opback/bin/ 2>/dev/null            | grep -Ei 'opback|openlist-backup' || echo '  (none)'

echo '== state dirs =='
ls -ld /var/lib/opback-agent /var/lib/openlist-backup-agent 2>/dev/null || echo '  (none)'

echo '== timers & cron =='
systemctl list-timers --all 2>/dev/null | grep -Ei 'opback|openlist-backup' || echo '  (no timers)'
crontab -l 2>/dev/null                  | grep -Ei 'opback|openlist-backup' || echo '  (no user cron)'
grep -RIl -E 'opback|openlist-backup' /etc/cron.d /etc/cron.daily /etc/cron.hourly 2>/dev/null || echo '  (no cron.d)'
```

**How to read the result**

- Unit listed but `list-units` shows it as `activating`/`auto-restart` or
  `failed` → a broken/duplicate Agent is looping. Treat it as residue.
- Two different unit names (new **and** legacy) both present → a failed upgrade;
  the old one must be removed.
- A process is running but no unit owns it → an orphan started by hand or left
  by a partial uninstall.

## 2. Remove residue (destructive)

Remove the **legacy** name first (if present), then the current name. Adjust
paths if you installed with custom `-state-dir` / `-service-name`.

```bash
# --- current name ---
sudo systemctl disable --now opback-agent 2>/dev/null
sudo rm -f /etc/systemd/system/opback-agent.service
sudo rm -f /usr/local/bin/opback-agent
sudo rm -rf /var/lib/opback-agent

# --- legacy name (from before the rename) ---
sudo systemctl disable --now openlist-backup-agent 2>/dev/null
sudo rm -f /etc/systemd/system/openlist-backup-agent.service
sudo rm -f /usr/local/bin/openlist-backup-agent
sudo rm -rf /var/lib/openlist-backup-agent

# --- reload systemd and clear any failed state ---
sudo systemctl daemon-reload
sudo systemctl reset-failed 2>/dev/null

# --- kill any orphan process that no unit owns ---
sudo pkill -f 'opback-agent' 2>/dev/null || true
sudo pkill -f 'openlist-backup-agent' 2>/dev/null || true

# --- cron residue (only if you used -once/cron) ---
crontab -l 2>/dev/null | grep -v -E 'opback|openlist-backup' | crontab -   # review first!
```

> The `crontab` line rewrites your user crontab. **Inspect `crontab -l` first**
> and only run it if the grep actually lists an Agent entry.

### "I deleted the node from the panel, but the host still runs an Agent"

Deleting a node in the panel enqueues an **uninstall** command. The Agent
executes it on its next `sync` (≤ sync interval, default 15 s), notifies the
panel, then spawns a **detached** cleanup process (the unit uses
`KillMode=process` so it survives the service stop). If the Agent is **offline**,
the command is never delivered — use **force delete** in the panel and clean the
host with section 2. Also check for a half-finished detached helper:

```bash
ps -ef | grep -i 'opback' | grep -v grep
```

## 3. Panel-side residue

The Web UI keeps a record of every node and job. After a host is cleaned, an
orphaned node simply shows **offline** — it is harmless but noisy.

- **Node**: `Nodes` page → delete the row. Offline nodes: use **force** delete
  (otherwise the panel waits for an uninstall acknowledgement).
- **Jobs**: deleting a node also deletes its jobs (run history is kept). If a
  job was reassigned manually, review the `Jobs` page.
- **OpenList connections** are shared and are *not* touched by node deletion.

## 4. Docker residue (panel host)

Only the **panel** runs in Docker; Agents are systemd services. Panels can still
leave containers/images/volumes behind:

```bash
cd /opt/opback
docker compose ps -a
docker ps -a     | grep -Ei 'opback' || echo '(no containers)'
docker images    | grep -Ei 'opback' || echo '(no images)'
docker volume ls | grep -Ei 'opback' || echo '(no volumes)'

# stop and remove the container + network:
docker compose down
# also drop the locally built image:
docker compose down --rmi local
# drop a named volume too (run history lives in ./data when bind-mounted):
docker volume rm opback_data 2>/dev/null || true
```

> With this repo the data is **bind-mounted** to `./data`, so removing the
> container does **not** delete your data — delete `./data` manually only if you
> really want to start over.

## 5. Verify the host is clean

Re-run section 1. A clean host prints `(none)` for units, processes, binaries
and state dirs. Then start a fresh Agent if needed:

```bash
curl -fsSL 'http://<PANEL>:8081/install.sh' | sudo bash -s -- \
  --panel-url 'http://<PANEL>:8081' --name 'web-1' --enroll-token '<TOKEN>'
```

Confirm exactly **one** unit exists and the node goes **online** with a
connection IP in the panel.
