# Security

> **English** | [简体中文](SECURITY.zh-CN.md)

This document records opback's **security model, implemented hardening** and
**residual risk**. The system targets **internal / trusted links**, but applies a
"minimal viable hardening" policy with several layers of protection.

---

## 1. Deployment model and trust boundary

```
             front-end port (:8080)            back-end port (:8081)
   admin browser ─────────────┐        ┌─────────────── agents on hosts
                               ▼        ▼
                          ┌─────────────────────┐
                          │        Panel        │
                          │  session cookie (HMAC)
                          │  store.json (0600)  │
                          └─────────────────────┘
```

- **Front-end port** (`web`): Web UI and admin API. Prefer LAN-only or an HTTPS
  reverse proxy.
- **Back-end port** (`agent`): `/api/agent/*`, `/install.sh`,
  `/downloads/agent/*`. Must be reachable by the machines you enroll.
- The two ports use **separate route tables**: the admin API is not present on the
  back-end listener, and Agent access / installer is not present on the front-end
  listener (the front-end explicitly returns 404 for `/install.sh` and
  `/downloads/`).
- Trust boundary: the network between panel and Agent should be trusted (LAN, VPN
  or TLS reverse proxy). Panel↔Agent traffic is currently **plaintext JSON** with
  no end-to-end encryption.

---

## 2. Authentication and sessions

### Admin (browser)

- A successful login issues an **HMAC-SHA256 signed token** in an `HttpOnly`
  cookie (`SameSite=Lax`, `Secure` when TLS is enabled).
- The token payload carries issue/expiry times (default **7 days**) and is bound
  to a **SessionEpoch**.
- **Changing the password, enabling or disabling 2FA** increments `SessionEpoch`,
  immediately invalidating **all** issued sessions.
- Renaming the user does **not** invalidate sessions (so an admin can rename and
  keep working).
- Passwords and 2FA challenges use **different token kinds** (a `kind` field); a
  challenge token cannot be used as a session token.
- Sessions may be carried by cookie or the `Authorization` header.

### 2FA (TOTP, RFC 6238)

- Standard TOTP: SHA-1, 6 digits, 30-second step, accepting **±1 window** of
  clock skew.
- Enabling generates a secret plus an `otpauth://` link and issues **8 recovery
  codes** (`XXXXX-XXXXX`) once.
- Recovery codes are stored as `HMAC(SessionSecret, normalized-code)` and are
  **single-use** (removed from storage when consumed).
- Two-step login: `POST /api/auth/login` returns `need_2fa` + a 5-minute
  one-time `challenge` when 2FA is on; `POST /api/auth/login/2fa` accepts a TOTP
  code or a recovery code.
- Disabling 2FA / regenerating recovery codes needs the current password, and
  (for disabling) a valid TOTP or recovery code.

### Agent side

- Register `POST /api/agent/register`: validates `enroll_token` (if configured),
  returns `node_id` and a node token.
- Heartbeat `POST /api/agent/sync`: authenticated by the `X-Node-Token` header;
  token comparison is **constant-time**.
- **Anti-takeover**: when `enroll_token` is not configured, a same-named
  registration does **not** reuse/take over an existing node — it creates a new
  node. An existing record is only reused when the caller presents that node's
  current token (which is then rotated). So even with an empty enrollment token,
  an attacker cannot impersonate an existing node by name.

---

## 3. Password storage

- The admin password is stored with **PBKDF2-HMAC-SHA256** (210000 iterations,
  random salt), format `pbkdf2$<iter>$<hex>`.
- Legacy salted SHA-256 hashes still verify and are **silently upgraded to PBKDF2
  on the next successful login** (the upgrade does not change SessionEpoch).
- `store.json` is written with mode `0600`.

---

## 4. Web security

- **CSRF / cross-origin**: every **mutating** `/api/*` request checks `Origin`
  and `Sec-Fetch-Site`. Same-origin passes; cross-origin is rejected unless it
  matches `trusted_origins` (for reverse proxy / split-domain deployments).
- **Login rate limiting**: failures are counted per source IP; default **8
  failures in 5 minutes → 10-minute ban** (in-memory).
- **Security headers**: `X-Content-Type-Options: nosniff`, `X-Frame-Options:
  DENY`, `Referrer-Policy: no-referrer`, `Cross-Origin-Opener-Policy:
  same-origin`, and a `Content-Security-Policy` (the embedded UI is self-hosted
  with no inline scripts); under TLS, `Strict-Transport-Security` is added.
- **Password comparison** is constant-time to avoid timing side channels.
- The front-end loads no third-party resources beyond `nosniff` (no CDN, no
  external fonts or scripts).

---

## 5. Transport and file permissions

- The panel is plaintext HTTP by default; production should enable built-in TLS
  (`tls_cert`/`tls_key`) or front it with HTTPS. With built-in TLS the cookie is
  automatically `Secure`.
- `store.json` (panel) and `state.json` (Agent) are both written with mode `0600`.
- OpenList passwords and the Telegram bot token are currently **plaintext** in
  `store.json`, protected only by filesystem permissions.

---

## 6. Reset flow (no login required)

If you lose the password or 2FA device, run this on the panel host (it modifies
the data and exits without starting the service):

| Command | Effect |
| --- | --- |
| `-reset-admin` | reset the username to `admin`, generate/print a random password, and disable 2FA |
| `-set-user <name>` | change the admin username only |
| `-set-password <pw>` | change the admin password only |
| `-disable-2fa` | disable 2FA only |

Resetting the password or disabling 2FA increments `SessionEpoch`, invalidating
existing sessions. Make sure the panel process is not running (`store.json` is
guarded by a file lock).

---

## 7. Residual risk and assumptions

1. Panel↔Agent traffic is not end-to-end encrypted; credentials are delivered to
   each Agent (stored in `state.json`, mode `0600`). Keep the link trusted.
2. OpenList passwords and the Telegram bot token in `store.json` are plaintext,
   protected only by file permissions; there is no encryption at rest.
3. Single admin, no multi-user or fine-grained permission model.
4. With an empty `enroll_token`, any machine that can reach the back-end can
   still register as a **new** node (it cannot take over an existing node, but it
   can consume node slots). Always set one in production.
5. `/install.sh` and `/downloads/agent/*` are **public and unauthenticated** on
   the back-end port (they serve a static script/binary). They leak no
   credentials, but do not expose the back-end port to the public internet.
6. Password strength is not complexity-checked (only length ≥ 8); 2FA recovery
   codes are the admin's responsibility.
7. The Agent executes **no user-provided commands**, so panel access does not
   give remote code execution on enrolled nodes. However, the Agent needs the
   privileges required to read the configured sources (often root), so a
   compromised Agent or panel can read and exfiltrate any file those privileges
   allow. With `staging` enabled, selected files are also copied into a local
   temporary directory (mode `0700`, removed after each run); a crash can leave
   a copy containing sensitive data behind.

---

## 8. Reporting vulnerabilities

If you find a security issue, please do not open a public issue. Contact the
repository maintainers privately first, including reproduction steps, impact and
version (`opback-panel -version`).
