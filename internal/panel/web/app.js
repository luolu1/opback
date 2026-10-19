/* OpenList 备份面板 - 单页应用（无第三方依赖） */

const $ = (sel) => document.querySelector(sel);
const view = $("#view");

const state = {
  current: "overview",
  nodes: [],
  jobs: [],
  runs: [],
  settings: null,
  overview: null,
  openlists: [],
  sync: null,
  me: null,
  challenge: null,
};

const TITLES = {
  overview: "概览",
  nodes: "节点",
  jobs: "备份任务",
  runs: "运行记录",
  openlists: "OpenList 连接",
  settings: "设置",
  profile: "个人中心",
};

/* ---------- 基础工具 ---------- */

function esc(s) {
  if (s === null || s === undefined) return "";
  return String(s)
    .replaceAll("&", "&amp;")
    .replaceAll("<", "&lt;")
    .replaceAll(">", "&gt;")
    .replaceAll('"', "&quot;")
    .replaceAll("'", "&#39;");
}

function fmtBytes(n) {
  n = Number(n) || 0;
  if (n < 1024) return n + " B";
  const units = ["KB", "MB", "GB", "TB", "PB"];
  let i = -1;
  do { n /= 1024; i++; } while (n >= 1024 && i < units.length - 1);
  return n.toFixed(n >= 10 ? 0 : 1) + " " + units[i];
}

function fmtTime(iso) {
  if (!iso) return "-";
  const d = new Date(iso);
  if (isNaN(d) || d.getFullYear() < 2000) return "从未";
  return d.toLocaleString("zh-CN", { hour12: false });
}

// fmtAgo renders a compact "3 分钟前" style relative time.
function fmtAgo(iso) {
  if (!iso) return "从未";
  const d = new Date(iso);
  if (isNaN(d) || d.getFullYear() < 2000) return "从未";
  const sec = (Date.now() - d.getTime()) / 1000;
  if (sec < 60) return "刚刚";
  if (sec < 3600) return Math.floor(sec / 60) + " 分钟前";
  if (sec < 86400) return Math.floor(sec / 3600) + " 小时前";
  return Math.floor(sec / 86400) + " 天前";
}

function fmtDuration(seconds) {
  seconds = Number(seconds);
  if (!isFinite(seconds) || seconds < 0) return "-";
  if (seconds < 60) return seconds.toFixed(0) + " 秒";
  const m = Math.floor(seconds / 60);
  const s = Math.round(seconds % 60);
  if (m < 60) return m + " 分 " + s + " 秒";
  const h = Math.floor(m / 60);
  return h + " 时 " + (m % 60) + " 分";
}

function runSeconds(run) {
  if (!run.started_at || !run.finished_at) return null;
  const s = (new Date(run.finished_at) - new Date(run.started_at)) / 1000;
  return isFinite(s) && s >= 0 ? s : null;
}

function fmtSpeed(bytes, seconds) {
  if (!seconds || seconds <= 0) return "-";
  return fmtBytes(bytes / seconds) + "/s";
}

// quoteArg wraps a value in single quotes when it contains characters that a
// shell would otherwise interpret, so install commands can be pasted safely.
function quoteArg(v) {
  v = String(v == null ? "" : v);
  if (v !== "" && /^[A-Za-z0-9._:\/@=-]+$/.test(v)) return v;
  return "'" + v.replaceAll("'", "'\\''") + "'";
}

// copyText copies text to the clipboard. navigator.clipboard needs a secure
// context (HTTPS or localhost); over plain HTTP we fall back to execCommand.
async function copyText(text) {
  if (navigator.clipboard && window.isSecureContext) {
    try {
      await navigator.clipboard.writeText(text);
      toast("已复制到剪贴板");
      return true;
    } catch { /* fall through to the legacy path */ }
  }
  const ta = document.createElement("textarea");
  ta.value = text;
  ta.setAttribute("readonly", "");
  ta.style.position = "fixed";
  ta.style.top = "-1000px";
  ta.style.opacity = "0";
  document.body.appendChild(ta);
  ta.select();
  ta.setSelectionRange(0, ta.value.length);
  let ok = false;
  try { ok = document.execCommand("copy"); } catch { ok = false; }
  document.body.removeChild(ta);
  toast(ok ? "已复制到剪贴板" : "复制失败，请手动选择文本", !ok);
  return ok;
}

function fmtInterval(sec) {
  sec = Number(sec) || 0;
  if (sec <= 0) return "手动";
  const m = sec / 60;
  if (m < 60) return m + " 分钟";
  const h = m / 60;
  if (h < 24) return h + " 小时";
  return h / 24 + " 天";
}

const STATUS_LABEL = { success: "成功", failed: "失败", partial: "部分失败", running: "进行中" };

function statusBadge(s) {
  const cls = s === "running" ? "running pulse" : esc(s);
  return `<span class="badge ${cls}"><span class="dot"></span>${esc(STATUS_LABEL[s] || s)}</span>`;
}

function fmtOpenListName(id) {
  if (!id) return "默认";
  const o = state.openlists.find((x) => x.id === id);
  return o ? o.name : id;
}

let toastTimer;
function toast(msg, isError) {
  const t = $("#toast");
  t.textContent = msg;
  t.className = "toast" + (isError ? " error" : "");
  clearTimeout(toastTimer);
  toastTimer = setTimeout(() => t.classList.add("hidden"), 3500);
}

/* ---------- SVG 图表 ---------- */

function donutSvg(segments, centerLabel, centerSub) {
  const total = segments.reduce((a, s) => a + s.value, 0);
  const size = 168, r = 62, cx = size / 2, cy = size / 2, sw = 20;
  const C = 2 * Math.PI * r;
  let off = 0;
  const arcs = segments.map((s) => {
    const frac = total > 0 ? s.value / total : 0;
    const len = frac * C;
    const el = `<circle cx="${cx}" cy="${cy}" r="${r}" fill="none" stroke="${s.color}"
      stroke-width="${sw}" stroke-dasharray="${len.toFixed(2)} ${(C - len).toFixed(2)}"
      stroke-dashoffset="${(-off).toFixed(2)}" transform="rotate(-90 ${cx} ${cy})">
      <title>${esc(s.label)}: ${s.value}</title></circle>`;
    off += len;
    return el;
  }).join("");
  return `<svg viewBox="0 0 ${size} ${size}" class="donut">
    <circle cx="${cx}" cy="${cy}" r="${r}" fill="none" stroke="var(--panel)" stroke-width="${sw}"/>
    ${arcs}
    <text x="${cx}" y="${cy - 2}" text-anchor="middle" class="donut-num">${esc(centerLabel)}</text>
    <text x="${cx}" y="${cy + 17}" text-anchor="middle" class="donut-sub">${esc(centerSub)}</text>
  </svg>`;
}

function barChartSvg(points) {
  const W = 640, H = 200, padX = 24, padTop = 16, padBottom = 26, gap = 6;
  const n = Math.max(1, points.length);
  const max = Math.max(1, ...points.map((p) => p.value));
  const bw = (W - padX * 2 - gap * (n - 1)) / n;
  const baseY = H - padBottom;
  const bars = points.map((p, i) => {
    const h = (p.value / max) * (baseY - padTop);
    const x = padX + i * (bw + gap);
    const y = baseY - h;
    const label = i % 2 === 0 ? `<text x="${(x + bw / 2).toFixed(1)}" y="${H - 8}" text-anchor="middle" class="bar-label">${esc(p.short)}</text>` : "";
    return `<rect x="${x.toFixed(1)}" y="${y.toFixed(1)}" width="${Math.max(1, bw).toFixed(1)}"
        height="${Math.max(1, h).toFixed(1)}" rx="3" class="bar">
        <title>${esc(p.label)} · ${esc(p.tip)}</title></rect>${label}`;
  }).join("");
  return `<svg viewBox="0 0 ${W} ${H}" class="bars">
    <defs>
      <linearGradient id="barGrad" x1="0" y1="0" x2="0" y2="1">
        <stop offset="0%" stop-color="var(--accent)"/>
        <stop offset="100%" stop-color="var(--violet)" stop-opacity="0.55"/>
      </linearGradient>
    </defs>
    <line x1="${padX}" y1="${baseY}" x2="${W - padX}" y2="${baseY}" class="chart-axis"/>
    ${bars}
  </svg>`;
}

function ringSvg(frac, centerLabel, centerSub) {
  frac = Math.max(0, Math.min(1, frac || 0));
  const size = 150, r = 58, cx = size / 2, cy = size / 2, sw = 12;
  const C = 2 * Math.PI * r;
  const len = frac * C;
  return `<svg viewBox="0 0 ${size} ${size}" class="ring">
    <circle cx="${cx}" cy="${cy}" r="${r}" fill="none" stroke="var(--panel)" stroke-width="${sw}"/>
    <circle cx="${cx}" cy="${cy}" r="${r}" fill="none" stroke="url(#ringGrad)" stroke-width="${sw}"
      stroke-linecap="round" stroke-dasharray="${len.toFixed(2)} ${(C - len).toFixed(2)}"
      transform="rotate(-90 ${cx} ${cy})"/>
    <defs>
      <linearGradient id="ringGrad" x1="0" y1="0" x2="1" y2="1">
        <stop offset="0%" stop-color="var(--green)"/>
        <stop offset="100%" stop-color="var(--accent)"/>
      </linearGradient>
    </defs>
    <text x="${cx}" y="${cy - 2}" text-anchor="middle" class="ring-num">${esc(centerLabel)}</text>
    <text x="${cx}" y="${cy + 18}" text-anchor="middle" class="ring-sub">${esc(centerSub)}</text>
  </svg>`;
}

function gauge(label, value, total, cls) {
  const pct = total > 0 ? Math.round((value / total) * 100) : 0;
  return `<div class="gauge">
    <div class="top"><span>${esc(label)}</span><b>${value} / ${total}</b></div>
    <div class="progress ${cls || ""}"><div style="width:${pct}%"></div></div>
  </div>`;
}

/* ---------- API ---------- */

async function api(method, path, body) {
  const opts = { method, headers: {} };
  if (body !== undefined) {
    opts.headers["Content-Type"] = "application/json";
    opts.body = JSON.stringify(body);
  }
  const res = await fetch(path, opts);
  if (res.status === 401) {
    showLogin();
    throw new Error("未登录");
  }
  const text = await res.text();
  let data = null;
  if (text) { try { data = JSON.parse(text); } catch { data = text; } }
  if (!res.ok) {
    const msg = data && data.error ? data.error : "请求失败 (" + res.status + ")";
    throw new Error(msg);
  }
  return data;
}

/* ---------- 弹窗 ---------- */

function openModal(title, html) {
  $("#modal-title").textContent = title;
  $("#modal-body").innerHTML = html;
  $("#modal").classList.remove("hidden");
}
function closeModal() {
  $("#modal").classList.add("hidden");
  $("#modal-body").innerHTML = "";
}

/* ---------- 登录 ---------- */

function showLogin() {
  $("#app").classList.add("hidden");
  $("#login").classList.remove("hidden");
  resetLoginForm();
}
function showApp() {
  $("#login").classList.add("hidden");
  $("#app").classList.remove("hidden");
  loadMe();
  render();
}

function resetLoginForm() {
  state.challenge = null;
  const label = $("#login-2fa-label");
  if (label) label.classList.add("hidden");
  const code = $("#login-2fa");
  if (code) code.value = "";
  const btn = $("#login-submit");
  if (btn) btn.textContent = "登录";
}

// setUser reflects the signed-in account in the header menu.
function setUser(user) {
  state.me = user || null;
  const name = (user && user.user) || "admin";
  const nameEl = $("#user-name");
  if (nameEl) nameEl.textContent = name;
  const av = $("#user-avatar");
  if (av) av.textContent = name.slice(0, 1).toUpperCase();
}

async function loadMe() {
  try { setUser(await api("GET", "/api/me")); } catch { /* ignore */ }
}

// agentURL is the base URL agents dial. It is the back-end listener, which may
// sit on a different host/port than the web UI, so it comes from /api/me.
function agentURL() {
  return (state.me && state.me.agent_url) || location.origin;
}

$("#login-form").addEventListener("submit", async (e) => {
  e.preventDefault();
  $("#login-error").textContent = "";
  try {
    if (state.challenge) {
      const done = await api("POST", "/api/auth/login/2fa", {
        challenge: state.challenge,
        code: $("#login-2fa").value.trim(),
      });
      resetLoginForm();
      setUser(done);
      showApp();
      return;
    }
    const resp = await api("POST", "/api/auth/login", {
      username: $("#login-user").value,
      password: $("#login-pass").value,
    });
    if (resp && resp.need_2fa) {
      state.challenge = resp.challenge;
      $("#login-2fa-label").classList.remove("hidden");
      $("#login-submit").textContent = "验证";
      $("#login-2fa").focus();
      return;
    }
    resetLoginForm();
    setUser(resp);
    showApp();
  } catch (err) {
    $("#login-error").textContent = err.message;
  }
});

async function logout() {
  try { await api("POST", "/api/auth/logout", {}); } catch { /* ignore */ }
  showLogin();
}

/* ---------- 右上角用户菜单 ---------- */

const userPop = $("#user-pop");
$("#user-btn").addEventListener("click", (e) => {
  e.stopPropagation();
  userPop.classList.toggle("hidden");
});
document.addEventListener("click", () => userPop.classList.add("hidden"));
userPop.querySelectorAll("[data-action]").forEach((a) => {
  a.addEventListener("click", (e) => {
    e.preventDefault();
    userPop.classList.add("hidden");
    const act = a.dataset.action;
    if (act === "profile") { state.current = "profile"; render(); }
    else if (act === "logout") logout();
  });
});

/* ---------- 导航 ---------- */

document.querySelectorAll(".nav a").forEach((a) => {
  a.addEventListener("click", () => {
    state.current = a.dataset.view;
    render();
  });
});

$("#refresh").addEventListener("click", () => {
  state.settings = null;
  render();
  toast("已刷新");
});

$("#modal-close").addEventListener("click", closeModal);
$("#modal").addEventListener("click", (e) => {
  if (e.target.id === "modal") closeModal();
});

setInterval(() => {
  const el = $("#clock");
  if (el) el.textContent = new Date().toLocaleString("zh-CN", { hour12: false });
}, 1000);

/* ---------- 渲染入口 ---------- */

async function render() {
  document.querySelectorAll(".nav a").forEach((a) => {
    a.classList.toggle("active", a.dataset.view === state.current);
  });
  const title = $("#view-title");
  if (title) title.textContent = TITLES[state.current] || "";
  view.innerHTML = `<div class="empty">加载中…</div>`;
  try {
    if (state.current === "overview") await renderOverview();
    else if (state.current === "nodes") await renderNodes();
    else if (state.current === "jobs") await renderJobs();
    else if (state.current === "runs") await renderRuns();
    else if (state.current === "openlists") await renderOpenLists();
    else if (state.current === "settings") await renderSettings();
    else if (state.current === "profile") await renderProfile();
  } catch (err) {
    view.innerHTML = `<div class="empty">加载失败：${esc(err.message)}</div>`;
  }
}

/* ---------- 概览 / 仪表盘 ---------- */

async function renderOverview() {
  const [ov, openlists] = await Promise.all([
    api("GET", "/api/overview"),
    api("GET", "/api/openlists").catch(() => []),
  ]);
  state.overview = ov;
  state.openlists = openlists || [];

  const sc = ov.status_counts || {};
  const succeeded = (sc.success || 0) + (sc.partial || 0);
  const totalRuns = succeeded + (sc.failed || 0) + (sc.running || 0);

  const donutSegments = [
    { label: "成功", value: sc.success || 0, color: "var(--green)" },
    { label: "部分失败", value: sc.partial || 0, color: "var(--amber)" },
    { label: "失败", value: sc.failed || 0, color: "var(--red)" },
    { label: "进行中", value: sc.running || 0, color: "var(--accent)" },
  ];

  const series = ov.series || [];
  const maxBytes = Math.max(1, ...series.map((p) => p.bytes || 0));
  const barPoints = series.map((p) => {
    const d = new Date(p.date + "T00:00:00Z");
    const short = (d.getUTCMonth() + 1) + "/" + d.getUTCDate();
    return {
      short,
      label: p.date,
      value: p.bytes || 0,
      tip: `成功 ${p.success} · 失败 ${p.failed} · ${fmtBytes(p.bytes || 0)}`,
    };
  });

  const onlineFrac = ov.nodes_total > 0 ? ov.nodes_online / ov.nodes_total : 0;
  const connOK = ov.openlist_ok || 0;
  const connErr = ov.openlist_err || 0;

  view.innerHTML = `
    <div class="cards">
      <div class="card stat green">
        <div class="label">在线节点</div>
        <div class="value">${ov.nodes_online}<small> / ${ov.nodes_total}</small></div>
      </div>
      <div class="card stat">
        <div class="label">已启用任务</div>
        <div class="value">${ov.jobs_enabled}<small> / ${ov.jobs_total}</small></div>
      </div>
      <div class="card stat violet">
        <div class="label">累计备份数据</div>
        <div class="value">${fmtBytes(ov.bytes_total)}</div>
      </div>
      <div class="card stat ${ov.bytes_24h > 0 ? "green" : ""}">
        <div class="label">最近 24 小时</div>
        <div class="value">${fmtBytes(ov.bytes_24h)}</div>
      </div>
      <div class="card stat ${connErr > 0 ? "red" : "green"}">
        <div class="label">OpenList 连接</div>
        <div class="value">${connOK}<small> 正常 / ${ov.openlists} 个</small></div>
      </div>
    </div>

    <div class="dash-row">
      <div class="panel-card">
        <h3>运行状态分布</h3>
        <div class="chart-flex">
          ${donutSvg(donutSegments, String(totalRuns), "总运行次数")}
          <div class="legend">
            ${donutSegments.map((s) => `<div class="rowitem">
              <span class="swatch" style="background:${s.color}"></span>
              <span class="name">${esc(s.label)}</span>
              <span class="val">${s.value}</span>
            </div>`).join("")}
          </div>
        </div>
      </div>

      <div class="panel-card">
        <h3>节点与连接健康度</h3>
        <div class="ring-wrap">
          ${ringSvg(onlineFrac, Math.round(onlineFrac * 100) + "%", "节点在线率")}
          <div class="gauges grow">
            ${gauge("节点在线", ov.nodes_online, ov.nodes_total, "green")}
            ${gauge("任务启用", ov.jobs_enabled, ov.jobs_total, "")}
            ${gauge("连接正常", connOK, ov.openlists, "violet")}
          </div>
        </div>
      </div>
    </div>

    <div class="panel-card" style="margin-bottom:20px">
      <h3>近 14 天备份量（按成功数据量）</h3>
      ${series.length ? barChartSvg(barPoints) : `<div class="empty">暂无数据</div>`}
      <div class="muted" style="font-size:12px;margin-top:6px">单日峰值 ${fmtBytes(maxBytes)}</div>
    </div>

    <div class="section-title">最近运行</div>
    <div class="table-wrap">${runsTable(ov.recent_runs || [])}</div>
  `;
  bindRunsTable();
}

/* ---------- 运行记录表格 ---------- */

function runsTable(runs) {
  if (!runs || runs.length === 0) return `<div class="empty">暂无运行记录</div>`;
  return `<table><thead><tr>
      <th>状态</th><th>任务</th><th>节点</th><th>OpenList</th><th>触发</th>
      <th>开始时间</th><th>耗时</th><th>速度</th><th>文件</th><th>数据量</th><th>信息</th>
    </tr></thead><tbody>
    ${runs.map((r) => {
      const sec = runSeconds(r);
      const dur = r.status === "running" ? "进行中" : (sec === null ? "-" : fmtDuration(sec));
      const speed = sec ? fmtSpeed(r.bytes_done, sec) : "-";
      return `<tr class="clickable" data-run="${esc(r.id)}">
        <td>${statusBadge(r.status)}</td>
        <td><b>${esc(r.job_name || r.job_id)}</b></td>
        <td>${esc(r.node_name || "-")}</td>
        <td>${esc(r.openlist_name || "默认")}</td>
        <td>${r.trigger === "manual" ? "手动" : "定时"}</td>
        <td class="mono">${fmtTime(r.started_at)}</td>
        <td>${dur}</td>
        <td>${speed}</td>
        <td>${r.files_done} 传 / ${r.files_skipped} 跳 / ${r.files_failed} 失败</td>
        <td>${fmtBytes(r.bytes_done)}</td>
        <td class="muted cell-wrap">${esc(r.message || "")}</td>
      </tr>`;
    }).join("")}
    </tbody></table>`;
}

function bindRunsTable() {
  view.querySelectorAll("tr[data-run]").forEach((tr) => {
    tr.addEventListener("click", () => showRun(tr.dataset.run));
  });
}

async function showRun(id) {
  let run = null;
  try { run = await api("GET", "/api/runs/" + encodeURIComponent(id)); } catch { /* ignore */ }
  if (!run) return;
  const sec = runSeconds(run);
  const dur = run.status === "running" ? "进行中" : (sec === null ? "-" : fmtDuration(sec));
  const speed = sec ? fmtSpeed(run.bytes_done, sec) : "-";
  openModal("运行详情", `
    <div class="detail-grid">
      <div class="field"><span>任务</span><b>${esc(run.job_name || run.job_id)}</b></div>
      <div class="field"><span>状态</span><div>${statusBadge(run.status)}</div></div>
      <div class="field"><span>节点</span><b>${esc(run.node_name || "-")}</b></div>
      <div class="field"><span>OpenList 连接</span><b>${esc(run.openlist_name || "默认")}</b></div>
      <div class="field"><span>源路径</span><b class="mono">${esc(run.source || "-")}</b></div>
      <div class="field"><span>目标路径</span><b class="mono">${esc(run.dest || "-")}</b></div>
      <div class="field"><span>触发方式</span><b>${run.trigger === "manual" ? "手动" : "定时"}</b></div>
      <div class="field"><span>耗时</span><b>${dur}</b></div>
      <div class="field"><span>开始时间</span><b>${fmtTime(run.started_at)}</b></div>
      <div class="field"><span>结束时间</span><b>${run.finished_at ? fmtTime(run.finished_at) : "-"}</b></div>
      <div class="field"><span>平均速度</span><b>${speed}</b></div>
      <div class="field"><span>文件统计</span><b>${run.files_done} 上传 / ${run.files_skipped} 跳过 / ${run.files_failed} 失败（共 ${run.files_total}）</b></div>
      <div class="field"><span>数据量</span><b>${fmtBytes(run.bytes_done)} 已传 / ${fmtBytes(run.bytes_total)} 总计</b></div>
      <div class="field"><span>运行 ID</span><b class="mono">${esc(run.id)}</b></div>
    </div>
    <div class="field"><span>信息</span><b>${esc(run.message || "-")}</b></div>
    <div class="field"><span>日志</span><pre class="log">${esc((run.log || []).join("\n") || "无")}</pre></div>
  `);
}

/* ---------- 节点 ---------- */

async function renderNodes() {
  const nodes = await api("GET", "/api/nodes");
  state.nodes = nodes;
  view.innerHTML = `
    <div class="toolbar">
      <div class="grow muted">共 ${nodes.length} 个节点</div>
      <button class="primary" id="add-node">添加节点</button>
    </div>
    ${nodes.length === 0 ? `<div class="empty">还没有节点。点击“添加节点”或在目标机器上运行 agent。</div>` : `
    <div class="table-wrap"><table><thead><tr>
      <th>名称</th><th>状态</th><th>主机</th><th>连接 IP</th><th>系统</th><th>Agent 版本</th><th>任务数</th><th>最后心跳</th><th></th>
    </tr></thead><tbody>
    ${nodes.map((n) => {
      let status;
      if (n.uninstalling) status = `<span class="badge warn"><span class="dot"></span>卸载中</span>`;
      else status = `<span class="badge ${n.status}"><span class="dot"></span>${n.status === "online" ? "在线" : "离线"}</span>`;
      const del = n.uninstalling
        ? `<button class="small danger" data-force="${esc(n.id)}" data-name="${esc(n.name)}">强制删除</button>`
        : `<button class="small danger" data-del="${esc(n.id)}" data-name="${esc(n.name)}" data-status="${esc(n.status)}">删除</button>`;
      return `<tr>
        <td><b>${esc(n.name)}</b></td>
        <td>${status}</td>
        <td class="mono">${esc(n.hostname || "-")}</td>
        <td class="mono">${esc(n.ip || "-")}</td>
        <td class="mono">${esc(n.os || "-")}/${esc(n.arch || "-")}</td>
        <td class="mono">${esc(n.agent_version || "-")}</td>
        <td>${n.job_count}</td>
        <td class="muted">${fmtTime(n.last_seen)}</td>
        <td class="row">
          <button class="small primary" data-install="${esc(n.id)}">安装/更新</button>
          <button class="small" data-token="${esc(n.id)}">令牌</button>
          ${del}
        </td>
      </tr>`;
    }).join("")}
    </tbody></table></div>`}
  `;

  const add = $("#add-node");
  if (add) add.addEventListener("click", addNode);
  view.querySelectorAll("[data-install]").forEach((b) => b.addEventListener("click", () => showInstall(b.dataset.install)));
  view.querySelectorAll("[data-token]").forEach((b) => b.addEventListener("click", () => showNodeToken(b.dataset.token)));
  view.querySelectorAll("[data-del]").forEach((b) => b.addEventListener("click", () => uninstallNode(b.dataset.del, b.dataset.name, b.dataset.status)));
  view.querySelectorAll("[data-force]").forEach((b) => b.addEventListener("click", () => forceDeleteNode(b.dataset.force, b.dataset.name)));
}

async function uninstallNode(id, name, status) {
  const offline = status !== "online";
  const msg = offline
    ? `节点「${name}」当前显示为离线，可能无法收到卸载指令。\n\n仍要尝试远程卸载吗？（若失败，可稍后使用“强制删除”）`
    : `确定要卸载节点「${name}」吗？\n\n将同时删除其备份任务，并让目标机器上的 agent 自动卸载（移除服务与程序）。`;
  if (!confirm(msg)) return;
  try {
    const res = await api("DELETE", "/api/nodes/" + encodeURIComponent(id));
    if (res && res.online === false) {
      toast("已下发卸载指令，但节点当前离线；可稍后强制删除", true);
    } else {
      toast("已下发卸载指令，等待 agent 确认");
    }
    render();
  } catch (err) { toast(err.message, true); }
}

async function forceDeleteNode(id, name) {
  if (!confirm(`强制删除节点「${name}」？\n\n只会从面板移除记录，不会卸载目标机器上的 agent。`)) return;
  try {
    await api("DELETE", "/api/nodes/" + encodeURIComponent(id) + "?force=1");
    toast("节点已删除");
    render();
  } catch (err) { toast(err.message, true); }
}

async function addNode() {
  const name = prompt("新节点名称（例如：web-1）");
  if (!name) return;
  try {
    const node = await api("POST", "/api/nodes", { name });
    render();
    showInstall(node.id);
  } catch (err) { toast(err.message, true); }
}

async function ensureSettings() {
  if (!state.settings) {
    try { state.settings = await api("GET", "/api/settings"); } catch { /* ignore */ }
  }
  return state.settings || {};
}

// installCommand builds the one-liner that installs the agent as a systemd
// service on the target machine. The agent talks to the back-end listener, so
// the command points at the agent URL rather than the web UI origin.
function installCommand(node, enrollToken) {
  const panel = agentURL();
  let cmd = `curl -fsSL ${panel}/install.sh | sudo bash -s -- --panel-url ${quoteArg(panel)} --name ${quoteArg(node.name)}`;
  if (enrollToken) cmd += ` --enroll-token ${quoteArg(enrollToken)}`;
  return cmd;
}

// updateCommand re-runs the installer without --name/--enroll-token. The
// installer reads those back from the existing systemd unit, so this performs
// an in-place upgrade (new binary + restart) that keeps the node identity and
// its backup jobs. Used to roll out a new agent build from the panel.
function updateCommand() {
  const panel = agentURL();
  return `curl -fsSL ${panel}/install.sh | sudo bash -s -- --panel-url ${quoteArg(panel)}`;
}

async function showInstall(id) {
  let node;
  try { node = await api("GET", "/api/nodes/" + encodeURIComponent(id)); }
  catch (err) { toast(err.message, true); return; }
  if (!state.me || !state.me.agent_url) await loadMe();

  const set = await ensureSettings();
  const panel = agentURL();
  const cmd = installCommand(node, set.enroll_token);
  const updateCmd = updateCommand();
  const manual = `opback-agent -panel-url ${quoteArg(panel)} -name ${quoteArg(node.name)}` +
    (set.enroll_token ? ` -enroll-token ${quoteArg(set.enroll_token)}` : "");

  openModal("安装 / 更新节点：" + node.name, `
    <p class="muted">新节点首次安装：在目标 Linux 机器上以 root 执行下面这一行命令，会自动下载 agent 并注册为 systemd 服务（开机自启）：</p>
    <pre class="log" id="install-cmd">${esc(cmd)}</pre>
    <div class="row"><button class="primary small" id="copy-install">复制安装命令</button></div>
    <p class="muted">平滑更新（已安装的机器）：重新执行下面这条命令即可就地覆盖升级到面板上的最新 agent —— 替换二进制并重启服务，保留节点身份与备份任务：</p>
    <pre class="log" id="update-cmd">${esc(updateCmd)}</pre>
    <div class="row"><button class="small" id="copy-update">复制更新命令</button></div>
    ${set.enroll_token ? "" : `<p class="muted">提示：面板当前未设置「注册密钥」，任何主机都能注册节点；建议到「设置」里配置注册密钥。</p>`}
    <p class="muted">远程卸载：在面板删除该节点时，agent 会自动停止、移除 systemd 服务与程序文件。</p>
    <p class="muted">若不便使用一键脚本，可在目标机手动放置 agent 后以如下命令运行：</p>
    <pre class="log">${esc(manual)}</pre>
    <p class="muted">本节点令牌（agent 首次注册后自动获取，通常无需手动配置）：</p>
    <pre class="log">${esc(node.token)}</pre>
  `);
  const btn = $("#copy-install");
  if (btn) btn.addEventListener("click", () => copyText(cmd));
  const ubtn = $("#copy-update");
  if (ubtn) ubtn.addEventListener("click", () => copyText(updateCmd));
}

async function showNodeToken(id) {
  try {
    const n = await api("GET", "/api/nodes/" + encodeURIComponent(id));
    openModal("节点令牌：" + n.name, `
      <p class="muted">该令牌用于 agent 与面板通信，请妥善保管。</p>
      <pre class="log">${esc(n.token)}</pre>
      <p class="muted">如需重新生成，请删除并重新添加该节点。</p>
    `);
  } catch (err) { toast(err.message, true); }
}

/* ---------- 任务 ---------- */

async function renderJobs() {
  const [jobs, nodes, openlists, sync] = await Promise.all([
    api("GET", "/api/jobs"),
    api("GET", "/api/nodes"),
    api("GET", "/api/openlists").catch(() => []),
    api("GET", "/api/sync-status").catch(() => null),
  ]);
  state.jobs = jobs;
  state.nodes = nodes;
  state.openlists = openlists || [];
  state.sync = sync;
  const nodeName = (id) => (nodes.find((n) => n.id === id) || {}).name || id;
  const syncMap = {};
  if (sync && sync.items) for (const it of sync.items) syncMap[it.job_id] = it;

  const summary = sync
    ? `<span class="badge success"><span class="dot"></span>成功 ${sync.success}</span>
       <span class="badge failed"><span class="dot"></span>失败 ${sync.failed}</span>
       <span class="badge offline"><span class="dot"></span>未运行 ${sync.never}</span>`
    : "";

  view.innerHTML = `
    <div class="toolbar">
      <div class="grow muted">共 ${jobs.length} 个任务 ${summary}</div>
      <button class="primary" id="add-job" ${nodes.length === 0 ? "disabled" : ""}>新建任务</button>
    </div>
    ${nodes.length === 0 ? `<div class="empty">请先在“节点”页面添加节点。</div>` : ""}
    ${jobs.length === 0 ? `<div class="empty">还没有备份任务。</div>` : `
    <div class="table-wrap"><table><thead><tr>
      <th>名称</th><th>节点</th><th>OpenList</th><th>源路径</th><th>目标路径</th><th>计划</th><th>最近同步</th><th>启用</th><th></th>
    </tr></thead><tbody>
    ${jobs.map((j) => {
      const it = syncMap[j.id];
      let syncCell = `<span class="muted">尚未运行</span>`;
      if (it && it.has_run) {
        syncCell = `${statusBadge(it.status)}<div class="muted" style="font-size:12px;margin-top:2px">${fmtAgo(it.started_at)} · ${fmtBytes(it.bytes_done)}</div>`;
      } else if (!it) {
        syncCell = `<span class="muted">-</span>`;
      }
      return `<tr>
      <td><b>${esc(j.name)}</b></td>
      <td>${esc(nodeName(j.node_id))}</td>
      <td>${esc(fmtOpenListName(j.openlist_id))}</td>
      <td class="mono">${esc(j.source)}</td>
      <td class="mono">${esc(j.dest)}${j.options && j.options.delete_extra ? ` <span class="badge" title="删除目标端多余文件，保留 ${j.options.retention_days || 0} 天">清理</span>` : ""}</td>
      <td>${fmtInterval(j.interval)}</td>
      <td>${syncCell}</td>
      <td>${j.enabled ? '<span class="badge success"><span class="dot"></span>启用</span>' : '<span class="badge offline"><span class="dot"></span>停用</span>'}</td>
      <td class="row">
        <button class="small" data-run="${esc(j.id)}">立即运行</button>
        <button class="small" data-hist="${esc(j.id)}" data-name="${esc(j.name)}">记录</button>
        <button class="small" data-edit="${esc(j.id)}">编辑</button>
        <button class="small danger" data-del="${esc(j.id)}" data-name="${esc(j.name)}">删除</button>
      </td>
    </tr>`;
    }).join("")}
    </tbody></table></div>`}
  `;

  const add = $("#add-job");
  if (add) add.addEventListener("click", () => jobForm(null));
  view.querySelectorAll("[data-edit]").forEach((b) => b.addEventListener("click", () => jobForm(b.dataset.edit)));
  view.querySelectorAll("[data-hist]").forEach((b) => b.addEventListener("click", () => showJobHistory(b.dataset.hist, b.dataset.name)));
  view.querySelectorAll("[data-run]").forEach((b) => b.addEventListener("click", async () => {
    try {
      await api("POST", "/api/jobs/" + encodeURIComponent(b.dataset.run) + "/run", {});
      toast("已下发运行指令，agent 将在下次同步时执行");
    } catch (err) { toast(err.message, true); }
  }));
  view.querySelectorAll("[data-del]").forEach((b) => b.addEventListener("click", async () => {
    if (!confirm(`确定删除任务「${b.dataset.name}」？`)) return;
    await api("DELETE", "/api/jobs/" + encodeURIComponent(b.dataset.del));
    toast("任务已删除");
    render();
  }));
}

async function showJobHistory(id, name) {
  let runs = [];
  try { runs = await api("GET", "/api/runs?job_id=" + encodeURIComponent(id) + "&limit=30"); }
  catch (err) { toast(err.message, true); return; }
  const body = runs.length
    ? `<div class="table-wrap">${runsTable(runs)}</div>`
    : `<div class="empty">该任务还没有运行记录</div>`;
  openModal("同步记录：" + name, body);
  $("#modal-body").querySelectorAll("tr[data-run]").forEach((tr) => {
    tr.addEventListener("click", () => showRun(tr.dataset.run));
  });
}

function presetOptions(interval) {
  const presets = [
    [0, "手动触发"], [300, "每 5 分钟"], [900, "每 15 分钟"], [1800, "每 30 分钟"],
    [3600, "每小时"], [21600, "每 6 小时"], [43200, "每 12 小时"], [86400, "每天"],
  ];
  const known = presets.some(([v]) => v === interval);
  let html = presets.map(([v, label]) => `<option value="${v}">${label}</option>`).join("");
  html += `<option value="custom" ${!known ? "selected" : ""}>自定义…</option>`;
  return html;
}

async function jobForm(id) {
  const nodes = state.nodes;
  const openlists = state.openlists;
  let job = {
    name: "", node_id: nodes[0] ? nodes[0].id : "", openlist_id: openlists[0] ? openlists[0].id : "",
    source: "", dest: "/backups",
    interval: 0, enabled: true,
    options: { incremental: true, overwrite: true, follow_symlinks: false, concurrency: 2, include: [], exclude: [], delete_extra: false, retention_days: 7, staging: false, staging_dir: "", verify_consistency: true },
  };
  if (id) job = await api("GET", "/api/jobs/" + encodeURIComponent(id));

  const opts = job.options || {};

  if (openlists.length === 0) {
    openModal(id ? "编辑任务" : "新建任务", `
      <div class="empty">尚未配置 OpenList 连接。请先前往「OpenList 连接」添加一个连接。</div>
      <div class="actions"><button type="button" class="primary" id="f-goto-ol">去添加连接</button></div>
    `);
    $("#f-goto-ol").addEventListener("click", () => { closeModal(); state.current = "openlists"; render(); });
    return;
  }

  const openlistOpts = openlists.map((o) =>
    `<option value="${esc(o.id)}" ${o.id === job.openlist_id ? "selected" : ""}>${esc(o.name)}（${esc(o.base_url)}）</option>`).join("");

  openModal(id ? "编辑任务" : "新建任务", `
    <div class="grid2">
      <label class="field"><span>任务名称</span><input id="f-name" value="${esc(job.name)}" placeholder="例如：网站数据" /></label>
      <label class="field"><span>节点</span>
        <select id="f-node">${nodes.map((n) => `<option value="${esc(n.id)}" ${n.id === job.node_id ? "selected" : ""}>${esc(n.name)}（${n.status === "online" ? "在线" : "离线"}）</option>`).join("")}</select>
      </label>
    </div>
    <label class="field"><span>OpenList 连接（上传目标服务器）</span>
      <select id="f-openlist">${openlistOpts}</select>
    </label>
    <label class="field"><span>源路径（agent 所在机器上的本地路径）</span><input id="f-source" value="${esc(job.source)}" placeholder="/var/www 或 D:\\data" /></label>
    <label class="field"><span>目标路径（OpenList 中的路径）</span>
      <div class="row">
        <input id="f-dest" class="grow" value="${esc(job.dest)}" placeholder="/backups/web-1" />
        <button type="button" id="f-browse">浏览</button>
      </div>
    </label>
    <div id="oa-browser" class="hidden"></div>
    <div class="grid2">
      <label class="field"><span>执行计划</span>
        <select id="f-preset">${presetOptions(job.interval)}</select>
      </label>
      <label class="field"><span>间隔（秒，0 表示仅手动）</span><input id="f-interval" type="number" min="0" value="${esc(job.interval)}" /></label>
    </div>
    <div class="grid2">
      <label class="field"><span>并发上传数</span><input id="f-conc" type="number" min="1" max="16" value="${esc(opts.concurrency || 2)}" /></label>
      <div class="field"><span>选项</span>
        <label class="checkbox"><input type="checkbox" id="f-enabled" ${job.enabled ? "checked" : ""}/> 启用任务</label>
        <label class="checkbox"><input type="checkbox" id="f-incr" ${opts.incremental ? "checked" : ""}/> 增量备份（跳过未变更文件）</label>
        <label class="checkbox"><input type="checkbox" id="f-ow" ${opts.overwrite ? "checked" : ""}/> 覆盖目标同名文件</label>
        <label class="checkbox"><input type="checkbox" id="f-sym" ${opts.follow_symlinks ? "checked" : ""}/> 跟随符号链接</label>
      </div>
    </div>
    <div class="grid2">
      <label class="field"><span>包含规则（每行一个，留空表示全部）</span><textarea id="f-include" rows="3" placeholder="*.conf">${esc((opts.include || []).join("\n"))}</textarea></label>
      <label class="field"><span>排除规则（每行一个）</span><textarea id="f-exclude" rows="3" placeholder="node_modules&#10;*.log">${esc((opts.exclude || []).join("\n"))}</textarea></label>
    </div>
    <div class="grid2">
      <div class="field"><span>目标端清理</span>
        <label class="checkbox"><input type="checkbox" id="f-delextra" ${opts.delete_extra ? "checked" : ""}/> 删除目标端多余文件</label>
        <span class="muted" style="font-size:12px">源端已删除的文件也会从目标端移除（建议配合保留天数）</span>
      </div>
      <label class="field"><span>保留天数（0 表示立即删除）</span><input id="f-retention" type="number" min="0" max="3650" value="${esc(opts.retention_days != null ? opts.retention_days : 7)}" /></label>
    </div>
    <div class="grid2">
      <div class="field"><span>活跃写入</span>
        <label class="checkbox"><input type="checkbox" id="f-staging" ${opts.staging ? "checked" : ""}/> 先复制到临时目录再上传</label>
        <span class="muted" style="font-size:12px">适合数据库、日志等持续写入的文件/目录：先做一份临时副本，再从副本上传，上传耗时再长也不会读取正在变化的源文件。会额外占用临时磁盘空间。</span>
      </div>
      <div class="field"><span>一致性</span>
        <label class="checkbox"><input type="checkbox" id="f-verify" ${opts.verify_consistency ? "checked" : ""}/> 校验复制/上传期间文件是否被修改</label>
        <span class="muted" style="font-size:12px">检测到变化会自动重试一次；仍然变化则标记为「部分成功」，不写入增量索引。启用上面的暂存时校验复制阶段，否则校验上传阶段。</span>
      </div>
    </div>
    <label class="field"><span>临时目录（留空使用系统临时目录）</span><input id="f-staging-dir" value="${esc(opts.staging_dir || "")}" placeholder="/tmp 或 D:\\Temp" /></label>
    <div class="actions">
      <button type="button" id="f-cancel">取消</button>
      <button type="button" class="primary" id="f-save">保存</button>
    </div>
  `);

  const presetSel = $("#f-preset");
  const intervalInput = $("#f-interval");
  presetSel.value = String(job.interval);
  if (![...presetSel.options].some((o) => o.value === String(job.interval))) presetSel.value = "custom";
  presetSel.addEventListener("change", () => {
    if (presetSel.value !== "custom") intervalInput.value = presetSel.value;
  });

  $("#f-cancel").addEventListener("click", closeModal);
  $("#f-browse").addEventListener("click", () => openBrowser("/", $("#f-openlist").value, (p) => { $("#f-dest").value = p; }));
  $("#f-save").addEventListener("click", async () => {
    const payload = {
      name: $("#f-name").value.trim(),
      node_id: $("#f-node").value,
      openlist_id: $("#f-openlist").value,
      source: $("#f-source").value.trim(),
      dest: $("#f-dest").value.trim(),
      interval: parseInt($("#f-interval").value || "0", 10),
      enabled: $("#f-enabled").checked,
      options: {
        incremental: $("#f-incr").checked,
        overwrite: $("#f-ow").checked,
        follow_symlinks: $("#f-sym").checked,
        concurrency: parseInt($("#f-conc").value || "2", 10),
        include: splitLines($("#f-include").value),
        exclude: splitLines($("#f-exclude").value),
        delete_extra: $("#f-delextra").checked,
        retention_days: parseInt($("#f-retention").value || "0", 10),
        staging: $("#f-staging").checked,
        staging_dir: $("#f-staging-dir").value.trim(),
        verify_consistency: $("#f-verify").checked,
      },
    };
    try {
      if (id) await api("PUT", "/api/jobs/" + encodeURIComponent(id), payload);
      else await api("POST", "/api/jobs", payload);
      closeModal();
      toast("已保存");
      render();
    } catch (err) { toast(err.message, true); }
  });
}

function splitLines(s) {
  return (s || "").split("\n").map((x) => x.trim()).filter(Boolean);
}

/* ---------- OpenList 目录浏览器（内嵌于表单） ---------- */

function openBrowser(startPath, openlistId, onPick) {
  const box = $("#oa-browser");
  box.classList.remove("hidden");
  const qs = openlistId ? "&openlist_id=" + encodeURIComponent(openlistId) : "";

  const load = async (p) => {
    box.innerHTML = `<div class="empty">加载中…</div>`;
    let data;
    try { data = await api("GET", "/api/openlist/list?path=" + encodeURIComponent(p) + qs); }
    catch (err) { box.innerHTML = `<div class="empty">无法读取目录：${esc(err.message)}</div>`; return; }
    const parts = p.split("/").filter(Boolean);
    let acc = "";
    const crumbs = [`<a data-go="/">根目录</a>`];
    for (const seg of parts) {
      acc += "/" + seg;
      crumbs.push(`<span>/</span><a data-go="${esc(acc)}">${esc(seg)}</a>`);
    }
    const dirs = (data.dirs || []).map((d) => `<div data-go="${esc(joinPath(p, d.name))}">📁 ${esc(d.name)}</div>`).join("") || `<div class="muted" style="cursor:default">（没有子目录）</div>`;
    box.innerHTML = `
      <div class="breadcrumb">${crumbs.join(" ")}</div>
      <div class="dirlist">${dirs}</div>
      <div class="actions" style="margin-top:10px">
        <button type="button" id="oab-cancel">收起</button>
        <button type="button" class="primary" id="oab-pick">使用当前目录：${esc(p)}</button>
      </div>
    `;
    box.querySelectorAll("[data-go]").forEach((el) => el.addEventListener("click", () => load(el.dataset.go)));
    $("#oab-cancel").addEventListener("click", () => box.classList.add("hidden"));
    $("#oab-pick").addEventListener("click", () => { onPick(p); box.classList.add("hidden"); });
  };
  load(startPath);
}

function joinPath(a, b) {
  a = "/" + String(a).split("/").filter(Boolean).join("/");
  if (a === "/") return "/" + b;
  return a + "/" + b;
}

/* ---------- 运行记录 ---------- */

async function renderRuns() {
  const runs = await api("GET", "/api/runs?limit=200");
  await Promise.all([api("GET", "/api/openlists").catch(() => [])]).then(([ols]) => { state.openlists = ols || []; });
  state.runs = runs;
  view.innerHTML = `
    <div class="toolbar"><div class="grow muted">最近 ${runs.length} 条运行记录</div></div>
    <div class="table-wrap">${runsTable(runs)}</div>
  `;
  bindRunsTable();
}

/* ---------- OpenList 连接 ---------- */

function connStatusBadge(o) {
  if (o.status === "ok") return `<span class="badge ok"><span class="dot"></span>连接正常</span>`;
  if (o.status === "error") return `<span class="badge error"><span class="dot"></span>连接失败</span>`;
  return `<span class="badge unknown"><span class="dot"></span>未测试</span>`;
}

async function renderOpenLists() {
  const [openlists, jobs] = await Promise.all([
    api("GET", "/api/openlists"),
    api("GET", "/api/jobs").catch(() => []),
  ]);
  state.openlists = openlists || [];
  state.jobs = jobs || [];
  const jobCount = (id) => jobs.filter((j) => j.openlist_id === id).length;

  view.innerHTML = `
    <div class="toolbar">
      <div class="grow muted">共 ${openlists.length} 个 OpenList 连接</div>
      <button class="primary" id="add-ol">添加连接</button>
    </div>
    ${openlists.length === 0 ? `<div class="empty">还没有 OpenList 连接。点击“添加连接”配置一个 OpenList 服务器。</div>` : `
    <div class="conn-grid">
      ${openlists.map((o) => `<div class="conn">
        <div class="conn-head">
          <b>${esc(o.name)}</b>
          <div class="spacer"></div>
          ${connStatusBadge(o)}
        </div>
        <div class="url">${esc(o.base_url)}</div>
        <div class="meta">
          <span>用户：${esc(o.username || "（无）")}</span>
          <span>密码：${o.password_set ? "已设置" : "未设置"}</span>
          <span>任务：${jobCount(o.id)} 个</span>
        </div>
        <div class="meta">${o.last_check ? "检测于 " + fmtTime(o.last_check) : "尚未检测"}${o.message ? " · " + esc(o.message) : ""}</div>
        <div class="conn-actions">
          <button class="small" data-test="${esc(o.id)}">测试连接</button>
          <button class="small" data-edit="${esc(o.id)}">编辑</button>
          <button class="small danger" data-del="${esc(o.id)}" data-name="${esc(o.name)}" data-count="${jobCount(o.id)}">删除</button>
        </div>
      </div>`).join("")}
    </div>`}
  `;

  const add = $("#add-ol");
  if (add) add.addEventListener("click", () => openListForm(null));
  view.querySelectorAll("[data-edit]").forEach((b) => b.addEventListener("click", () => openListForm(b.dataset.edit)));
  view.querySelectorAll("[data-test]").forEach((b) => b.addEventListener("click", async () => {
    toast("正在测试连接…");
    try {
      await api("POST", "/api/openlists/" + encodeURIComponent(b.dataset.test) + "/test", {});
      toast("连接成功 ✔");
    } catch (err) { toast("连接失败：" + err.message, true); }
    render();
  }));
  view.querySelectorAll("[data-del]").forEach((b) => b.addEventListener("click", () => deleteOpenList(b.dataset.del, b.dataset.name, Number(b.dataset.count) || 0)));
}

async function openListForm(id) {
  let ol = { name: "", base_url: "", username: "" };
  if (id) {
    try { ol = await api("GET", "/api/openlists/" + encodeURIComponent(id)); }
    catch (err) { toast(err.message, true); return; }
  }
  openModal(id ? "编辑连接" : "添加 OpenList 连接", `
    <label class="field"><span>名称</span><input id="ol-name" value="${esc(ol.name)}" placeholder="例如：主存储" /></label>
    <label class="field"><span>服务地址</span><input id="ol-url" value="${esc(ol.base_url)}" placeholder="https://openlist.example.com" /></label>
    <div class="grid2">
      <label class="field"><span>用户名</span><input id="ol-user" value="${esc(ol.username)}" /></label>
      <label class="field"><span>密码${id ? "（留空表示不修改）" : ""}</span><input id="ol-pass" type="password" placeholder="${ol.password_set ? "已设置" : "未设置"}" /></label>
    </div>
    <div class="actions">
      <button type="button" id="ol-cancel">取消</button>
      <button type="button" class="primary" id="ol-save">保存</button>
    </div>
  `);
  $("#ol-cancel").addEventListener("click", closeModal);
  $("#ol-save").addEventListener("click", async () => {
    const payload = {
      name: $("#ol-name").value.trim(),
      base_url: $("#ol-url").value.trim(),
      username: $("#ol-user").value.trim(),
      password: $("#ol-pass").value,
    };
    try {
      let saved;
      if (id) saved = await api("PUT", "/api/openlists/" + encodeURIComponent(id), payload);
      else saved = await api("POST", "/api/openlists", payload);
      closeModal();
      toast("已保存，正在测试连接…");
      render();
      try {
        await api("POST", "/api/openlists/" + encodeURIComponent(saved.id) + "/test", {});
        toast("连接成功 ✔");
      } catch (err) { toast("已保存，但连接失败：" + err.message, true); }
      render();
    } catch (err) { toast(err.message, true); }
  });
}

async function deleteOpenList(id, name, count) {
  let force = false;
  if (count > 0) {
    if (!confirm(`连接「${name}」正被 ${count} 个任务使用。\n\n强制删除后，这些任务将失去连接配置，需要重新指定。确定继续吗？`)) return;
    force = true;
  } else if (!confirm(`确定删除连接「${name}」？`)) {
    return;
  }
  try {
    await api("DELETE", "/api/openlists/" + encodeURIComponent(id) + (force ? "?force=1" : ""));
    toast("连接已删除");
    render();
  } catch (err) { toast(err.message, true); }
}

/* ---------- 设置 ---------- */

function firstLine(s) {
  s = String(s || "").replace(/<[^>]*>/g, "").replace(/\s+/g, " ").trim();
  return s.length > 90 ? s.slice(0, 90) + "…" : s;
}

function levelBadge(level) {
  const map = { success: ["success", "成功"], error: ["error", "失败"], info: ["", "信息"] };
  const [cls, label] = map[level] || ["", level || "信息"];
  return `<span class="badge ${cls}"><span class="dot"></span>${esc(label)}</span>`;
}

function notificationsTable(list) {
  if (!list || list.length === 0) return `<div class="empty">还没有推送记录</div>`;
  return `<table><thead><tr>
      <th>时间</th><th>类型</th><th>级别</th><th>结果</th><th>内容</th>
    </tr></thead><tbody>
    ${list.map((n) => `<tr>
      <td class="mono">${fmtTime(n.created_at)}</td>
      <td>${esc(n.title || "-")}</td>
      <td>${levelBadge(n.level)}</td>
      <td>${n.ok ? '<span class="badge success"><span class="dot"></span>已发送</span>' : '<span class="badge error"><span class="dot"></span>失败</span>'}</td>
      <td class="muted cell-wrap">${esc((n.error ? n.error + " · " : "") + firstLine(n.text))}</td>
    </tr>`).join("")}
    </tbody></table>`;
}

async function renderSettings() {
  const [s, notifications] = await Promise.all([
    api("GET", "/api/settings"),
    api("GET", "/api/notifications?limit=50").catch(() => []),
  ]);
  state.settings = s;
  const tg = s.telegram || {};

  view.innerHTML = `
    <div class="section-title">Agent 与同步</div>
    <div class="card col">
      <label class="field"><span>注册密钥（agent 注册时需匹配；留空表示不校验）</span><input id="s-enroll" value="${esc(s.enroll_token)}" /></label>
      <div class="grid2">
        <label class="field"><span>同步间隔（秒）</span><input id="s-sync" type="number" min="5" value="${esc(s.sync_interval)}" /></label>
        <label class="field"><span>运行记录保留条数</span><input id="s-maxruns" type="number" min="50" value="${esc(s.max_runs)}" /></label>
      </div>
      <div class="row"><div class="grow"></div><button id="s-save-gen" class="primary">保存</button></div>
    </div>

    <div class="section-title">Telegram 机器人推送</div>
    <div class="card col">
      <label class="checkbox"><input type="checkbox" id="t-enabled" ${tg.enabled ? "checked" : ""}/> 启用 Telegram 推送</label>
      <div class="grid2">
        <label class="field"><span>Bot Token</span><input id="t-token" value="${esc(tg.bot_token || "")}" placeholder="123456:ABC-DEF..." /></label>
        <label class="field"><span>Chat ID</span>
          <span class="input-row">
            <input id="t-chat" value="${esc(tg.chat_id || "")}" placeholder="例如 123456789" />
            <button type="button" id="t-fetch" title="先在 Telegram 给机器人发一条消息，再点此自动获取">获取</button>
          </span>
        </label>
      </div>
      <div class="row" style="gap:20px">
        <label class="checkbox"><input type="checkbox" id="t-succ" ${tg.notify_success ? "checked" : ""}/> 备份成功时推送</label>
        <label class="checkbox"><input type="checkbox" id="t-fail" ${tg.notify_failure ? "checked" : ""}/> 备份失败时推送</label>
      </div>
      <div class="row"><div class="grow"></div>
        <button id="t-test">发送测试消息</button>
        <button id="t-save" class="primary">保存</button>
      </div>
      <p class="muted" style="font-size:12px;margin:0">
        在 Telegram 找 <b>@BotFather</b> 创建机器人拿到 Bot Token；填好并保存后，直接在 Telegram 里给机器人发送 <span class="mono">/id</span>，它会回复本会话的 Chat ID，填到上面即可（也可点「获取」从最近消息里挑选，选中即自动保存）。
        配置好后可用 <span class="mono">/status</span> 看各任务最近同步状态、<span class="mono">/tasks</span> 看任务清单、<span class="mono">/sync</span> 看历史同步成功率；输入 <span class="mono">/</span> 会看到全部可用命令。
      </p>
    </div>

    <div class="section-title">推送记录</div>
    <div class="table-wrap">${notificationsTable(notifications)}</div>

    <p class="muted" style="margin-top:18px">提示：账户安全（用户名 / 密码）已移至右上角头像菜单的「个人中心」；OpenList 连接在左侧「OpenList 连接」页面管理。</p>
  `;

  $("#s-save-gen").addEventListener("click", async () => {
    try {
      await api("PUT", "/api/settings", {
        enroll_token: $("#s-enroll").value.trim(),
        sync_interval: parseInt($("#s-sync").value || "15", 10),
        max_runs: parseInt($("#s-maxruns").value || "1000", 10),
      });
      toast("已保存");
      render();
    } catch (err) { toast(err.message, true); }
  });

  $("#t-save").addEventListener("click", async () => {
    try {
      await saveTelegramSettings();
      toast("Telegram 设置已保存");
      render();
    } catch (err) { toast(err.message, true); }
  });

  $("#t-test").addEventListener("click", async () => {
    const btn = $("#t-test");
    btn.disabled = true;
    toast("正在发送测试消息…");
    try {
      await api("POST", "/api/settings/telegram/test", {
        bot_token: $("#t-token").value.trim(),
        chat_id: $("#t-chat").value.trim(),
      });
      toast("测试消息已发送 ✔");
    } catch (err) { toast("发送失败：" + err.message, true); }
    btn.disabled = false;
    render();
  });

  $("#t-fetch").addEventListener("click", fetchTelegramChats);
}

// chatLabel picks the friendliest available name for a Telegram chat.
function chatLabel(c) {
  if (c.name) return c.name;
  return c.username ? "@" + c.username : "聊天 " + c.id;
}

// saveTelegramSettings persists the Telegram form. Picking a Chat ID from the
// list calls this too, so a fresh choice takes effect right away instead of
// waiting for the operator to press 保存 afterwards.
async function saveTelegramSettings() {
  await api("PUT", "/api/settings", {
    telegram: {
      enabled: $("#t-enabled").checked,
      bot_token: $("#t-token").value.trim(),
      chat_id: $("#t-chat").value.trim(),
      notify_success: $("#t-succ").checked,
      notify_failure: $("#t-fail").checked,
    },
  });
}

// fetchTelegramChats asks the panel to list chats that recently messaged the
// bot, so the operator can pick a Chat ID instead of looking it up elsewhere.
async function fetchTelegramChats() {
  const btn = $("#t-fetch");
  btn.disabled = true;
  const old = btn.textContent;
  btn.textContent = "获取中…";
  try {
    const token = $("#t-token").value.trim();
    const resp = await api("GET", "/api/settings/telegram/chats?token=" + encodeURIComponent(token));
    const chats = (resp && resp.chats) || [];
    if (chats.length === 0) {
      openModal("获取 Chat ID",
        `<div class="empty">还没有记录到任何会话。<br/>请先在 Telegram 里给机器人发送 <span class="mono">/id</span>，它会直接回复本会话的 Chat ID，把它填到上面的输入框即可；之后再点「获取」也能从列表里挑选。</div>`);
    } else {
      const rows = chats.map((c) => `<div data-chat="${esc(c.id)}">
        <b>${esc(chatLabel(c))}</b>
        ${c.type ? `<span class="badge">${esc(c.type)}</span>` : ""}
        <span class="muted mono">${esc(c.id)}</span>
        ${c.last_message ? `<div class="muted" style="font-size:12px">${esc(c.last_message)}</div>` : ""}
      </div>`).join("");
      openModal("获取 Chat ID", `<p class="muted">点击选择要接收通知的会话：</p><div class="dirlist">${rows}</div>`);
      $("#modal-body").querySelectorAll("[data-chat]").forEach((el) => {
        el.addEventListener("click", async () => {
          $("#t-chat").value = el.dataset.chat;
          closeModal();
          try {
            await saveTelegramSettings();
            toast("已保存 Chat ID：" + el.dataset.chat);
            render();
          } catch (err) {
            toast("已填入 Chat ID：" + el.dataset.chat + "，但保存失败：" + err.message, true);
          }
        });
      });
    }
  } catch (err) {
    toast("获取失败：" + err.message, true);
  }
  btn.disabled = false;
  btn.textContent = old;
}

/* ---------- 个人中心 ---------- */

// askPassword opens a small modal that collects the current password and runs
// onOk with it. Used before sensitive credential operations.
function askPassword(title, hint, onOk) {
  openModal(title, `
    <p class="muted">${esc(hint || "请输入当前密码以继续")}</p>
    <label class="field"><span>当前密码</span><input id="ask-pass" type="password" autocomplete="current-password" /></label>
    <div class="row"><div class="grow"></div><button id="ask-ok" class="primary">确定</button></div>
  `);
  const run = () => onOk($("#ask-pass").value);
  $("#ask-ok").addEventListener("click", run);
  $("#ask-pass").addEventListener("keydown", (e) => { if (e.key === "Enter") run(); });
  setTimeout(() => { const el = $("#ask-pass"); if (el) el.focus(); }, 30);
}

function showRecoveryCodes(codes) {
  openModal("两步验证恢复码", `
    <p class="muted">请立即妥善保存以下一次性恢复码，每个仅可使用一次。关闭后无法再次查看。</p>
    <pre class="log" style="font-size:15px;letter-spacing:1px">${esc(codes.join("\n"))}</pre>
    <div class="row"><button id="copy-recovery">复制全部</button><div class="grow"></div><button id="recovery-ok" class="primary">我已保存</button></div>
  `);
  $("#copy-recovery").addEventListener("click", () => copyText(codes.join("\n")));
  $("#recovery-ok").addEventListener("click", closeModal);
}

function enableTwoFA() {
  askPassword("启用两步验证", "启用后登录需要额外输入验证器 App 生成的动态码。请先输入当前密码。", async (pw) => {
    if (!pw) { toast("请输入当前密码", true); return; }
    try {
      const res = await api("POST", "/api/settings/2fa/setup", { password: pw });
      openModal("绑定验证器", `
        <p class="muted">在验证器 App（Google Authenticator、Authy、1Password 等）中手动添加以下密钥：</p>
        <pre class="log" style="font-size:16px;letter-spacing:2px;user-select:all">${esc(res.secret)}</pre>
        <p class="muted">或导入 otpauth 链接：</p>
        <pre class="log" style="white-space:pre-wrap">${esc(res.otpauth)}</pre>
        <label class="field"><span>输入 App 当前生成的 6 位验证码</span><input id="tf-code" inputmode="numeric" autocomplete="one-time-code" placeholder="6 位数字" /></label>
        <div class="row"><div class="grow"></div><button id="tf-enable" class="primary">确认启用</button></div>
      `);
      setTimeout(() => { const el = $("#tf-code"); if (el) el.focus(); }, 30);
      $("#tf-enable").addEventListener("click", async () => {
        try {
          const out = await api("POST", "/api/settings/2fa/enable", { password: pw, code: $("#tf-code").value.trim() });
          showRecoveryCodes(out.recovery_codes || []);
          await loadMe();
          render();
        } catch (err) { toast(err.message, true); }
      });
    } catch (err) { toast(err.message, true); }
  });
}

function disableTwoFA() {
  askPassword("关闭两步验证", "关闭后登录将只需密码。请输入当前密码以确认。", async (pw) => {
    try {
      await api("POST", "/api/settings/2fa/disable", { password: pw });
      toast("已关闭两步验证");
      await loadMe();
      render();
    } catch (err) { toast(err.message, true); }
  });
}

function regenerateRecovery() {
  askPassword("重新生成恢复码", "旧恢复码将立即失效。请输入当前密码以确认。", async (pw) => {
    try {
      const out = await api("POST", "/api/settings/2fa/recovery", { password: pw });
      showRecoveryCodes(out.recovery_codes || []);
    } catch (err) { toast(err.message, true); }
  });
}

async function renderProfile() {
  const [me, s] = await Promise.all([
    api("GET", "/api/me").catch(() => state.me || {}),
    api("GET", "/api/settings").catch(() => ({})),
  ]);
  state.settings = s;
  if (me && me.user) state.me = me;
  const user = (me && me.user) || (state.me && state.me.user) || "admin";
  const twoFA = !!(s && s.twofa_enabled);

  view.innerHTML = `
    <div class="profile-head">
      <div class="avatar xl">${esc(user.slice(0, 1).toUpperCase())}</div>
      <div class="grow">
        <div class="profile-name">${esc(user)}</div>
        <div class="muted">管理员 · 拥有全部管理权限</div>
      </div>
      <button id="p-logout" class="danger">退出登录</button>
    </div>

    <div class="section-title">账户安全</div>
    <div class="card col">
      <div class="grid2">
        <label class="field"><span>用户名</span><input id="p-user" value="${esc(user)}" /></label>
        <label class="field"><span>当前密码</span><input id="p-cur" type="password" autocomplete="current-password" placeholder="修改密码/用户名时需填写" /></label>
      </div>
      <div class="grid2">
        <label class="field"><span>新密码</span><input id="p-pass" type="password" placeholder="至少 8 位" /></label>
        <label class="field"><span>确认新密码</span><input id="p-pass2" type="password" placeholder="再次输入" /></label>
      </div>
      <div class="row"><div class="grow"></div><button id="p-save" class="primary">保存修改</button></div>
      <p class="muted" style="font-size:12px;margin:0">留空密码表示只修改用户名；修改密码后当前会话保持登录，其他会话将失效。</p>
    </div>

    <div class="section-title">两步验证（2FA）</div>
    <div class="card col">
      ${twoFA ? `
        <p class="muted" style="margin:0">状态：<span class="badge success"><span class="dot"></span>已启用</span> 登录时需要验证器动态码或恢复码。</p>
        <div class="row">
          <button id="tf-regen">重新生成恢复码</button>
          <div class="grow"></div>
          <button id="tf-disable" class="danger">关闭两步验证</button>
        </div>
      ` : `
        <p class="muted" style="margin:0">状态：<span class="badge offline"><span class="dot"></span>未启用</span> 建议启用以提升账户安全性。</p>
        <div class="row"><div class="grow"></div><button id="tf-enable-start" class="primary">启用两步验证</button></div>
      `}
      <p class="muted" style="font-size:12px;margin:0">若无法登录，可在面板机执行 <code>opback-panel -reset-admin</code> 离线重置用户名、密码与 2FA。</p>
    </div>

    <div class="section-title">Telegram 推送</div>
    <div class="card col">
      <p class="muted" style="margin:0">备份成功 / 失败通知与任务查询命令在「设置」页面配置。</p>
      <div class="row"><div class="grow"></div><button id="p-goto-tg">前往设置</button></div>
    </div>
  `;

  $("#p-logout").addEventListener("click", logout);
  $("#p-goto-tg").addEventListener("click", () => { state.current = "settings"; render(); });
  $("#p-save").addEventListener("click", async () => {
    const pass = $("#p-pass").value;
    const pass2 = $("#p-pass2").value;
    if (pass && pass !== pass2) { toast("两次输入的密码不一致", true); return; }
    try {
      const out = await api("POST", "/api/settings/password", {
        username: $("#p-user").value.trim(),
        current_password: $("#p-cur").value,
        password: pass,
      });
      toast(pass ? "用户名与密码已更新" : "用户名已更新");
      $("#p-cur").value = ""; $("#p-pass").value = ""; $("#p-pass2").value = "";
      if (out && out.user) setUser(out);
      else loadMe();
    } catch (err) { toast(err.message, true); }
  });

  if (twoFA) {
    $("#tf-disable").addEventListener("click", disableTwoFA);
    $("#tf-regen").addEventListener("click", regenerateRecovery);
  } else {
    $("#tf-enable-start").addEventListener("click", enableTwoFA);
  }
}

/* ---------- 启动 ---------- */

(async function boot() {
  try {
    const me = await api("GET", "/api/me");
    if (me && me.user) { showApp(); return; }
  } catch { /* 未登录 */ }
  showLogin();
})();
