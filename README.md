# opback

> **English** | [简体中文](README.zh-CN.md)

A central panel + per-host Agent that replaces hand-written `rclone` backup
scripts on every machine.

- **Panel**: runs on one host and serves a Web UI + API. A dashboard manages
  nodes, backup jobs and multiple OpenList connections, and shows detailed run
  history. An optional **Telegram bot** pushes success/failure and answers
  queries about task sync status.
- **Agent**: runs on each machine to be backed up. It syncs with the panel on a
  timer, receives jobs and commands, uploads local files to the configured
  OpenList, and reports results. It can also **uninstall itself** on command
  from the panel.
- **Security**: **split front-end/back-end ports** (Web UI + admin API on one
  port, Agent access on another), **2FA** with one-time recovery codes, login
  rate-limiting, `Origin`/`Sec-Fetch-Site` checks, PBKDF2 password hashing and
  hardened response headers. A CLI reset path exists if you lose the password or
  2FA device.
- **Deployment**: a single static binary, with **systemd**, **Docker/Compose**
  and **cron one-shot** options. `host`/`port` are defined in a JSON config file.

The panel only distributes configuration; **scheduling runs locally on each
Agent**. So a temporarily down panel does not stop already-scheduled jobs, and
the Agent reports missed results once the panel is reachable again.

---

## Table of contents

- [Architecture](#architecture)
- [Features](#features)
- [Quick start](#quick-start)
- [Usage](#usage)
- [Deployment](#deployment)
- [Configuration](#configuration)
- [HTTP API](#http-api)
- [Security notes](#security-notes)
- [Troubleshooting: leftover Agent files](docs/agent-residue.md)
- [Limitations & roadmap](#limitations--roadmap)
- [Development](#development)

---

## Architecture

```
                 ┌────────────────────────────────────────┐
                 │                 Panel                   │
                 │  front-end :8080  Web UI + admin API    │
                 │  back-end  :8081  /api/agent/* + installer│
                 │  JSON persistence (store.json)          │
                 └──────────────┬─────────────────────────┘
             register / sync  ▲  │  jobs, commands, OpenList creds
                              │  ▼
        ┌─────────────────────┼─────────────────────┐
        │                     │                     │
   ┌────┴─────┐          ┌────┴─────┐          ┌────┴─────┐
   │  Agent   │          │  Agent   │          │  Agent   │
   │ local    │          │ local    │          │ local    │
   │ schedule │          │ schedule │          │ schedule │
   └────┬─────┘          └────┬─────┘          └────┬─────┘
        │  upload (PUT /api/fs/put)               │
        └───────────────────┬───────────────────┘
                            ▼
                    ┌──────────────┐
                    │   OpenList   │
                    └──────────────┘
```

> **Split ports**: the Web UI and admin API listen on the **front-end port**
> (default `:8080`); Agent access and the one-line installer listen on the
> **back-end port** (default `:8081`). They can bind to different interfaces so
> you can expose only the back-end to the machines you enroll, or put the
> front-end behind an HTTPS reverse proxy. Addresses come from the config file
> (`web.host/port`, `agent.host/port`) and can be overridden with
> `-web-addr` / `-agent-addr`.

Workflow:

1. On first start the Agent calls `POST /api/agent/register` with the enrollment
   token, receives a `node_id` and a long-lived token, and saves them to local
   `state.json`.
2. Every sync interval (15 s by default) the Agent calls
   `POST /api/agent/sync`, reporting recent run results (`status=running` for
   progress, a terminal status when finished).
3. The panel returns the jobs, one-shot commands and OpenList credentials that
   apply to that node.
4. The Agent runs jobs locally, uploads via OpenList's `PUT /api/fs/put`, and
   reports the outcome on the next sync.

### Incremental backup

Each job stores a manifest of `size` + `mtime`. When `incremental` is on, files
whose size and mtime are unchanged are skipped — only new/changed files are
re-uploaded.

### Destination cleanup & retention (trash)

With `delete_extra` enabled (default **off**), files that exist on the
destination but no longer exist on the source are removed. Removed files first
move to a trash directory `<dest>/.olb-trash/<timestamp>/…`. `retention_days`
controls how long a snapshot lives before permanent deletion (`0` = delete
immediately; clamped to 0–3650).

### Multiple OpenList connections

Connections are managed on their own page. Each job references one
`openlist_id`; credentials are delivered to the owning node during sync. Deleting
a connection that is still referenced returns `409` unless `?force=1` is used
(which clears the references).

### Remote node uninstall

Deleting a node enqueues an `uninstall` command. The Agent, on its next sync,
notifies the panel (`POST /api/agent/uninstall`) and then spawns a detached
cleanup process that stops/removes its own systemd unit, binary and state. If the
node is offline you can force-delete it and clean the host manually — see the
[residue guide](docs/agent-residue.md).

### Telegram bot (optional)

Push backup success/failure to a chat and answer commands `/status`, `/tasks`,
`/sync`, `/id`, `/help`. Without a configured chat ID only `/id` works (so you
can discover the chat ID). Notification history (up to 500, newest first) is
visible in the Web UI.

### Account security: 2FA and reset

Enable TOTP 2FA (RFC 6238) in the profile; one-time recovery codes are issued.
Changing the password or toggling 2FA invalidates all sessions. If you are locked
out, reset from the CLI without logging in (see [Configuration](#configuration)).

---

## Quick start

### Prerequisites

- Go 1.24+ (build only; the runtime needs just the compiled binary).
- A reachable OpenList with a known URL, username and password.

### 1. Build

```bash
git clone <repo> opback
cd opback
make build
# artifacts:
#   bin/opback-panel   panel
#   bin/opback-agent   Agent
```

To cross-compile Agent binaries for several platforms at once:

```bash
make cross
# bin/opback-agent-linux-amd64
# bin/opback-agent-linux-arm64
# bin/opback-agent-linux-arm
# bin/opback-agent-windows-amd64.exe
# bin/opback-agent-darwin-arm64
```

> These binaries are what the panel's **one-line installer** (`/install.sh`)
> serves per platform. Without `make cross` (or the Docker build, which does it
> for you), the installer returns "no agent binary for this platform".

### 2. Start the panel

The **front-end** (Web UI + admin API) and **back-end** (Agent access +
installer) use split ports: default `:8080` and `:8081`.

The recommended way is a JSON config file that spells out the listen addresses:

```bash
cp deploy/config.example.json config.json
# edit web.host/web.port and agent.host/agent.port
./bin/opback-panel -config config.json -data ./panel-data -agent-dir ./bin
```

Or use flags / environment variables only:

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

Precedence is **flags > config file > environment > built-in default**.
`-enroll-token` can also be changed in the Web UI under **Settings**;
`-openlist-*` is a first-run convenience that creates/updates a connection named
"default connection", after which you manage connections on the **OpenList**
page. `-agent-dir` must contain the `make cross` artifacts so the installer can
serve them.

Open `http://<panel>:8080`, log in with the default **admin / admin**, and
**change the password immediately** (and enable **2FA** in the profile).

### 3. Enroll an Agent (on each machine to back up)

In the panel open **Nodes → Add node**, then click **Copy install command** and
paste it onto the target host (root + systemd required). The generated command
already points at the **back-end port**:

```bash
curl -fsSL 'http://<panel>:8081/install.sh' | sudo bash -s -- \
  --panel-url 'http://<panel>:8081' \
  --name 'web-1' \
  --enroll-token 'CHANGE-ME-ENROLL'
```

The script: detects OS/arch → downloads the matching Agent to
`/usr/local/bin/opback-agent` (atomic same-dir replace) → writes
`/etc/systemd/system/opback-agent.service` → `systemctl enable` + `systemctl
restart`. Inspect it with `systemctl status opback-agent` /
`journalctl -u opback-agent -f`.

> **Smooth in-place update**: the same command can be re-run. On re-run, any
> omitted argument (e.g. `--name`) is read back from the existing unit, the
> original node ID and token are preserved, and **no re-registration** happens —
> so the node is not duplicated. That is why after shipping a new panel/Agent you
> only need to re-run the install command on each host. The button in the node
> list is labelled **Install/Update** for the same reason.

Prefer not to use the script? Place the binary yourself and run it directly:

```bash
./bin/opback-agent \
  -panel-url 'http://<panel>:8081' \
  -enroll-token 'CHANGE-ME-ENROLL' \
  -name "$(hostname)" \
  -state-dir ./agent-state
```

Once the Agent completes its first sync the node turns **online** in the panel
(nodes that never enrolled stay offline).

### 4. Create a backup job

On the **Jobs** page create a job:

- **Node**: the target Agent.
- **Source path**: a local directory, e.g. `/etc/nginx`.
- **Destination path**: a directory in OpenList, e.g. `/backups/web1/nginx`.
- **Interval (seconds)**: how often to run; `0` means manual only.
- **Options**: incremental, overwrite, follow symlinks, include/exclude globs,
  concurrency, destination cleanup & retention.

After saving, the Agent picks the job up on its next sync (≤ 15 s) and runs it on
schedule. **Run now** dispatches a one-shot command without waiting.

---

## Usage

### Panel pages

The left sidebar is vertical and contains:

- **Overview**: a dashboard with a status donut, a 14-day volume bar chart, and
  gauges for node-online / job-enabled / connection-health rates, plus the most
  recent runs. Top cards summarise online nodes, enabled jobs, cumulative and
  24-hour backup volume, and connection health.
- **Nodes**: registered Agents, online status (by `last_seen`), **Install/Update**
  command, view token, delete node (remote uninstall).
- **Jobs**: create/edit/delete jobs, pick the **OpenList connection**, choose the
  destination with an embedded browser, see the most recent sync per job (same
  data as the Telegram `/status`), view a job's run history, and **Run now**.
- **Runs**: status, job, node, connection, source/destination, trigger, start/end,
  duration, average speed, file/byte counts, messages and logs; click for details.
- **OpenList connections**: manage multiple servers; saving tests the connection
  and stores its status; edit/test/delete (in-use connections need force).
- **Settings**: enrollment token, sync interval, run-history cap, the **Telegram
  bot** (enable, token, chat ID, success/failure push, test message) and push
  history.
- **Profile** (top-right avatar menu): account info, change username/password
  (current password required), **2FA** (enable/disable/regenerate recovery
  codes), sign out.

> **Refresh** and **Sign out** live in the top-right corner. Refresh re-fetches
> the current page immediately; the avatar menu opens the profile or signs out.

### One-shot mode (cron friendly)

To avoid a long-running daemon, run the Agent once and let system cron drive it:

```bash
./bin/opback-agent -panel-url http://panel:8081 -once
```

`-once` runs every enabled job on that node a single time and exits (it does not
depend on the local interval).

### Agent state files

Under `-state-dir`:

- `state.json`: `node_id`, access token, last run time per job.
- Increment manifests: size + mtime of uploaded files.

Deleting this directory makes the Agent re-register and triggers a full upload.

---

## Deployment

Three options:

1. **Binary / systemd**: both panel and Agent run as systemd services
   (recommended for production; details below).
2. **Docker / Compose**: the panel runs in a container (below); Agents still run
   under systemd on each host.
3. **Portable binary / cron**: run the artifacts directly; the Agent can also run
   under cron with `-once`.

> The `deploy/` directory ships copy-paste examples: `opback-panel.service`,
> `opback-agent.service`, `panel.env.example`, `agent.env.example`,
> `config.example.json`. Their contents are repeated below.

### systemd: panel

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
# front-end :8080 (Web UI + admin API), back-end :8081 (Agent access + installer).
# You can also use -config /etc/opback/config.json for host/port.
ExecStart=/opt/opback/bin/opback-panel -config /etc/opback/config.json -web-addr :8080 -agent-addr :8081 -data /var/lib/opback
Restart=on-failure
RestartSec=3
# Prefer environment variables for secrets (via EnvironmentFile, mode 0600)
# EnvironmentFile=/etc/opback/panel.env

[Install]
WantedBy=multi-user.target
```

### systemd: Agent

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
# Let the detached cleanup process survive when the panel removes this node.
KillMode=process
EnvironmentFile=/etc/opback/agent.env

[Install]
WantedBy=multi-user.target
```

> To back up system directories (`/etc`, `/root`), the Agent needs read access —
> commonly run it as root and scope the source paths carefully.

`agent.env` (`chmod 600`):

```
# Points at the panel BACK-END (Agent) port, not the Web UI port.
OPBACK_PANEL_URL=http://panel.example.com:8081
OPBACK_ENROLL_TOKEN=CHANGE-ME-ENROLL
OPBACK_NAME=web1
OPBACK_STATE_DIR=/var/lib/opback-agent
```

Enable:

```bash
sudo systemctl daemon-reload
sudo systemctl enable --now opback-panel
sudo systemctl enable --now opback-agent
```

### Docker Compose: panel

The repo ships a `Dockerfile` (multi-stage, also cross-compiles Agent binaries),
`docker-compose.yml`, and a root `config.json` for host/port. Defaults:

- `config.json` is mounted read-only at `/app/config.json` and passed via
  `-config`, so **host/port come from the config file**.
- Persistent data is bind-mounted at `./data` (container `/data`); Agent
  binaries live in the container at `/app/bin`.
- Ports: host `18820` (front-end) / `18821` (back-end), mapped to the
  container's `8080` / `8081`. Edit `docker-compose.yml` to change the host
  side.
- Secrets (enrollment token, OpenList credentials) are injected via environment
  / `.env` and never baked into the image.

```bash
cd /opt/opback
cp .env.example .env      # set OPBACK_ENROLL_TOKEN
docker compose up -d --build
docker compose logs -f panel
```

Log in with `admin / admin` as before. Agent install commands are still generated
by the **Nodes** page; `install.sh` serves the Agent binaries from inside the
container.

> Expose only the back-end `18821` to the machines you enroll; keep `18820` on
> your LAN or behind an HTTPS reverse proxy.

### Agent under cron

```
*/30 * * * * /opt/opback/bin/opback-agent -panel-url http://panel:8081 -once >> /var/log/opback-agent.log 2>&1
```

---

## Configuration

### Panel (`opback-panel`)

| Flag | Env | Default | Description |
| --- | --- | --- | --- |
| `-config` | `OPBACK_CONFIG` | `./config.json` (optional) | JSON config path; a missing/empty file is ignored |
| `-web-addr` | `OPBACK_WEB_ADDR` | `:8080` | **front-end** listen: Web UI + admin API |
| `-agent-addr` | `OPBACK_AGENT_ADDR` | `:8081` | **back-end** listen: Agent access + `/install.sh` + binary download |
| `-addr` | `OPBACK_ADDR` | — | deprecated alias of `-web-addr` |
| `-data` | `OPBACK_DATA` | `./panel-data` | data dir (`store.json`) |
| `-agent-dir` | `OPBACK_AGENT_DIR` | `./bin` | directory of per-platform Agent binaries for the installer |
| `-enroll-token` | `OPBACK_ENROLL_TOKEN` | empty | Agent enrollment secret; empty allows open registration (not recommended) |
| `-openlist-url` | `OPBACK_OPENLIST_URL` | empty | OpenList URL (trailing `/` trimmed) |
| `-openlist-user` | `OPBACK_OPENLIST_USER` | empty | OpenList username |
| `-openlist-pass` | `OPBACK_OPENLIST_PASS` | empty | OpenList password |

Full precedence: **flag > config file > environment > built-in default**.
`-enroll-token` overwrites the saved setting only when non-empty; `-openlist-*`
creates/updates the "default connection", also only when non-empty.

#### Config file (JSON)

`web.host/port` and `agent.host/port` define the listen addresses; a missing or
empty file does not block startup. See `deploy/config.example.json`:

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

| Field | Description |
| --- | --- |
| `web` / `agent` | front-end / back-end `host` (empty = all interfaces) and `port` |
| `data_dir` / `agent_dir` | same as `-data` / `-agent-dir` |
| `agent_public_url` | back-end URL used when generating install commands; empty derives it from the request Host + back-end port |
| `trusted_origins` | extra allowed cross-origin `Origin`s (reverse proxy / split-domain) |
| `cookie_secure` | whether the session cookie carries `Secure`; forced `true` under TLS |
| `tls_cert` / `tls_key` | when both set, the front-end serves HTTPS (back-end stays HTTP for Agents) |
| `enroll_token` | Agent enrollment secret |
| `openlist` | credentials for the "default connection" written on first start |

> Reset commands (`-reset-admin` / `-set-user` / `-set-password` /
> `-disable-2fa`) also read `-config` / `-data` to locate the same `store.json`.

Editable in the Web UI under **Settings**:

- Agent enrollment token.
- **sync interval** (seconds, default 15).
- **run-history cap** (`max_runs`); oldest records are trimmed beyond it.
- **Telegram bot**: enable, bot token, chat ID, success/failure push, and **Send
  test message**.

The admin username/password are changed in the top-right **Profile** menu
(default `admin` / `admin`). OpenList connections now have their own left-side
page and can be multiple.

### Agent (`opback-agent`)

| Flag | Env | Default | Description |
| --- | --- | --- | --- |
| `-panel-url` | `OPBACK_PANEL_URL` | empty (**required**) | panel **back-end** (Agent) URL, default port `8081` |
| `-name` | `OPBACK_NAME` | hostname | node display name |
| `-enroll-token` | `OPBACK_ENROLL_TOKEN` | empty | required when the panel enables it |
| `-state-dir` | `OPBACK_STATE_DIR` | `./agent-state` | state directory |
| `-service-name` | `OPBACK_SERVICE_NAME` | `opback-agent` | systemd unit name used to stop/remove itself on remote uninstall |
| `-once` | — | `false` | run every enabled job once and exit |

### Job options (`JobOptions`)

| Option | Description |
| --- | --- |
| `incremental` | skip unchanged files via the local manifest (size + mtime) |
| `overwrite` | allow overwriting existing destination files |
| `follow_symlinks` | follow symlinks while walking (with cycle detection) |
| `exclude` | glob list relative to the source root; matches are skipped (`path/filepath.Match`) |
| `include` | when non-empty, only matching relative paths are backed up |
| `concurrency` | number of concurrent uploads |
| `delete_extra` | delete "extra" destination files: present on the destination but absent from the (filtered) source |
| `retention_days` | extra files move to trash first, then are permanently deleted after this many days; `0` = delete immediately |

---

## HTTP API

Panel auth: admin endpoints need a login session (HMAC-signed cookie). Agent
endpoints use the `X-Node-Token` header.

### Public / auth

| Method | Path | Description |
| --- | --- | --- |
| `POST` | `/api/auth/login` | login; with 2FA returns `need_2fa` + a one-time `challenge`, then call `/api/auth/login/2fa` |
| `POST` | `/api/auth/login/2fa` | finish login with `challenge` + TOTP (or recovery code) |
| `POST` | `/api/auth/logout` | sign out |
| `GET` | `/api/me` | current session info (`user`, `twofa`, `agent_url`, `version`) |
| `GET` | `/healthz` | health check (both listeners, no auth) |
| `GET` | `/install.sh` | one-line installer (Linux + systemd), no auth; served on the **back-end** port |
| `GET` | `/downloads/agent/{os}/{arch}` | download an Agent binary, no auth; **back-end** port |

### Agent endpoints

| Method | Path | Description |
| --- | --- | --- |
| `POST` | `/api/agent/register` | register (validates the enroll token), returns `node_id` + token |
| `POST` | `/api/agent/sync` | heartbeat: report runs, receive jobs, commands and OpenList creds |
| `POST` | `/api/agent/uninstall` | callback before self-uninstall; deletes the node record (idempotent) |

### Admin endpoints (login required)

| Method | Path | Description |
| --- | --- | --- |
| `GET` | `/api/overview` | overview stats |
| `GET` | `/api/nodes` | node list |
| `GET` | `/api/nodes/{id}` | node detail |
| `POST` | `/api/nodes` | create a node manually |
| `DELETE` | `/api/nodes/{id}` | delete a node (remote uninstall by default, `?force=1` = record only) |
| `GET` | `/api/jobs` | job list |
| `POST` | `/api/jobs` | create a job |
| `GET` | `/api/jobs/{id}` | job detail |
| `PUT` | `/api/jobs/{id}` | update a job |
| `DELETE` | `/api/jobs/{id}` | delete a job |
| `POST` | `/api/jobs/{id}/run` | dispatch "run now" |
| `GET` | `/api/runs` | run list |
| `GET` | `/api/runs/{id}` | run detail (with logs) |
| `GET` | `/api/settings` | read settings (`telegram`, `twofa_enabled`, …) |
| `PUT` | `/api/settings` | update settings (may submit only `telegram`) |
| `POST` | `/api/settings/password` | change admin user/password (**needs** `current_password`; new password ≥ 8 chars, returns a new session) |
| `POST` | `/api/settings/2fa/setup` | generate a 2FA secret + `otpauth` URL (needs current password) |
| `POST` | `/api/settings/2fa/enable` | confirm with a TOTP code, enable 2FA, return one-time recovery codes |
| `POST` | `/api/settings/2fa/disable` | disable 2FA (needs current password + TOTP or recovery code) |
| `POST` | `/api/settings/2fa/recovery` | regenerate recovery codes (needs current password; old ones are void) |
| `POST` | `/api/settings/telegram/test` | send a Telegram test message (may temporarily pass `bot_token`/`chat_id`) |
| `GET` | `/api/settings/telegram/chats` | list chats the bot has seen (prefers chats recorded by the background bot; optional `token`) |
| `GET` | `/api/sync-status` | aggregated latest sync per job (used by the Jobs page and bot `/status`) |
| `GET` | `/api/notifications` | push history (optional `limit`, up to 500, newest first) |
| `GET` | `/api/openlists` | connection list (password masked, with `password_set`) |
| `POST` | `/api/openlists` | create a connection |
| `GET` | `/api/openlists/{id}` | connection detail |
| `PUT` | `/api/openlists/{id}` | update a connection (empty password = unchanged) |
| `DELETE` | `/api/openlists/{id}` | delete a connection (409 if referenced; `?force=1` clears references) |
| `POST` | `/api/openlists/{id}/test` | test and record status |
| `GET` | `/api/openlist/list` | browse OpenList directories (optional `openlist_id`) |

Underlying OpenList calls: login `POST /api/auth/login`, upload
`PUT /api/fs/put` (`File-Path` header URL-encoded, supports sharded paths), list
`POST /api/fs/list` (`per_page` capped at 100, auto-paginated), mkdir
`POST /api/fs/mkdir`. The auth header is `Authorization: <token>` (no `Bearer`
prefix). API docs: <https://fox.oplist.org/>.

---

## Security notes

This system targets **internal / trusted links**, but ships several hardening
measures; see the full audit in [`SECURITY.md`](SECURITY.md). Key points:

1. **Split ports**: Web UI + admin API only on the **front-end port** (default
   `:8080`); Agent access, `/install.sh` and binary download only on the
   **back-end port** (default `:8081`). Expose the back-end only to machines you
   enroll; keep the front-end on a LAN or behind an HTTPS reverse proxy.
2. **Change the default `admin/admin` password**, enable **2FA**, and set a
   non-empty **enrollment token**. Without a token a same-named registration
   cannot take over an existing node, but any machine can still register as a new
   node.
3. **Session & login protection**: session cookies are `HttpOnly` /
   `SameSite=Lax` HMAC-signed tokens; all mutating `/api/*` requests check
   `Sec-Fetch-Site` / `Origin` (allow a reverse-proxy domain via
   `trusted_origins`); failed logins are rate-limited per source IP (8 failures
   in 5 minutes → 10-minute ban).
4. **Changing the password or toggling 2FA invalidates all sessions**; the admin
   password is stored as PBKDF2-HMAC-SHA256 (210000 iterations + random salt),
   and legacy SHA256 hashes are upgraded transparently on the next login.
5. **Hardened response headers**: `X-Content-Type-Options: nosniff`,
   `X-Frame-Options: DENY`, `Referrer-Policy: no-referrer`,
   `Cross-Origin-Opener-Policy` and a CSP; under TLS
   (`tls_cert`/`tls_key`) `Strict-Transport-Security` is added and cookies are
   marked `Secure`.
6. **Credentials reach the Agent**: OpenList passwords live in the panel's
   `store.json` (mode `0600`) and are delivered to the owning node, where they
   live in `state.json` (`0600`). Keep the panel↔Agent link trusted (LAN, VPN or
   HTTPS reverse proxy) and restrict the data directory.
7. **Agent privileges**: running as root lets the Agent read the whole host;
   grant only what is necessary and scope source paths carefully.
8. **Plaintext at rest**: OpenList passwords and the Telegram bot token in
   `store.json` are currently plaintext (protected only by file permissions);
   there is no end-to-end encryption on the Agent side.

> Locked out? Reset from the CLI without logging in — see
> [Configuration](#configuration).

---

## Troubleshooting: leftover Agent files

Removing a node, updating the Agent, or migrating from the old project name can
leave pieces behind. See the dedicated guide:

- English: [docs/agent-residue.md](docs/agent-residue.md)
- 简体中文: [docs/agent-residue.zh-CN.md](docs/agent-residue.zh-CN.md)

---

## Limitations & roadmap

Known MVP limitations:

- Destination cleanup (`delete_extra`) is **optional and off by default**: only
  enable it when the destination path is exclusively owned by that job.
- The trash is organised as per-run timestamp snapshots and expires as a whole;
  there is no per-file retention.
- Incremental detection uses size + mtime (seconds) only, no content hash.
- Panel↔Agent traffic is **plaintext JSON**, with no end-to-end encryption.
- Notifications are **Telegram-only**; history keeps the latest 500 entries.
- Single admin account (with 2FA); no multi-user or permission model.

Possible enhancements:

- Per-file version history on the destination.
- Content-hash verification and resumable uploads.
- More notification channels (Webhook, email, WeCom/DingTalk).
- Job templates, batch rollout, multi-user and fine-grained permissions.

---

## Development

```bash
make test    # all tests (incl. e2e: real backup flow + panel/Agent HTTP)
make vet     # go vet
make fmt     # gofmt -w .
make build   # build panel and Agent
make cross   # cross-compile Agent for multiple platforms
make clean   # clean bin/
```

The project uses only the Go standard library, so it cross-compiles easily into a
single static binary.

Key directories:

```
cmd/panel            panel entrypoint
cmd/agent            Agent entrypoint
internal/config      JSON config (web.host/port, agent.host/port, TLS, enroll token, …)
internal/panel       panel: routing, auth, admin API, Agent API, 2FA, security middleware, embedded Web UI
internal/agent       Agent: register, sync, local scheduling, backup engine, incremental manifest
internal/openlist    OpenList client (streaming upload, paginated list, auto re-login)
internal/store       JSON persistence & CRUD (PBKDF2 passwords, 2FA, session epoch)
internal/types       shared data structures and sync protocol
internal/e2e         end-to-end and HTTP integration tests
deploy/              systemd units, env and JSON config examples
docs/                troubleshooting guides (EN + ZH)
Dockerfile           Docker image (panel + per-platform Agent binaries)
docker-compose.yml   Compose example (panel only)
config.json          Compose config (host/port + container paths)
SECURITY.md          security model, hardening checklist and residual risk
```
