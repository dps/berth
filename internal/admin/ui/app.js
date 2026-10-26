// Berth admin page. Plain modules, no build step: berthd serves these files
// as they are. Everything shown comes from /api/*, which berthd answers only
// for the @owners group.

import { bars, lines, spark, hideTip } from "/charts.js";

const $ = (sel, root = document) => root.querySelector(sel);
const main = $("#main");

// ---------- formatting ----------

// esc makes untrusted text (paths, referrers, app output) safe in HTML.
const esc = (s) => String(s ?? "").replace(/[&<>"']/g, (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" })[c]);

function num(n) {
  n = n || 0;
  if (n < 1000) return String(Math.round(n));
  if (n < 1e6) return (n / 1e3).toFixed(n < 1e4 ? 1 : 0).replace(/\.0$/, "") + "k";
  return (n / 1e6).toFixed(1).replace(/\.0$/, "") + "M";
}
function bytes(b) {
  b = b || 0;
  const u = ["B", "KB", "MB", "GB", "TB"];
  let i = 0;
  while (b >= 1024 && i < u.length - 1) { b /= 1024; i++; }
  return (i === 0 ? b : b.toFixed(b < 10 ? 1 : 0)) + " " + u[i];
}
function ms(v) {
  if (!v) return "–";
  if (v < 1) return "<1 ms";
  if (v < 1000) return Math.round(v) + " ms";
  return (v / 1000).toFixed(v < 10000 ? 2 : 1) + " s";
}
function pct(v) {
  if (!isFinite(v)) return "–";
  if (v === 0) return "0%";
  if (v < 0.001) return "<0.1%";
  return (v * 100).toFixed(v < 0.1 ? 1 : 0) + "%";
}
function ago(t) {
  if (!t || t.startsWith?.("0001")) return "–";
  const s = (Date.now() - new Date(t).getTime()) / 1000;
  if (s < 45) return "just now";
  if (s < 3600) return Math.round(s / 60) + "m ago";
  if (s < 86400 * 2) return Math.round(s / 3600) + "h ago";
  return Math.round(s / 86400) + "d ago";
}
function since(t) {
  if (!t || t.startsWith?.("0001")) return "–";
  let s = Math.max(0, (Date.now() - new Date(t).getTime()) / 1000);
  const d = Math.floor(s / 86400); s -= d * 86400;
  const h = Math.floor(s / 3600); s -= h * 3600;
  const m = Math.floor(s / 60);
  if (d) return `${d}d ${h}h`;
  if (h) return `${h}h ${m}m`;
  return `${m}m`;
}
const clock = (t) => new Date(t * 1000).toLocaleTimeString([], { hour: "2-digit", minute: "2-digit" });
const day = (t) => new Date(t * 1000).toLocaleDateString([], { weekday: "short", day: "numeric", month: "short" });
const stamp = (t) => new Date(t).toLocaleString([], { month: "short", day: "numeric", hour: "2-digit", minute: "2-digit", second: "2-digit" });
let regionNames;
try { regionNames = new Intl.DisplayNames([navigator.language || "en"], { type: "region" }); } catch {}
const flag = (cc) => cc && cc.length === 2 ? String.fromCodePoint(...[...cc.toUpperCase()].map((c) => 0x1f1a5 + c.charCodeAt(0))) : "";
const country = (cc) => (regionNames?.of(cc) || cc);

// delta compares now with the period before; higherIsGood picks the color.
function delta(cur, prev, higherIsGood = true) {
  if (!prev) return ""; // nothing to compare with (no traffic, or no history yet)
  const d = (cur - prev) / prev;
  if (Math.abs(d) < 0.01) return `<span class="delta flat">±0%</span>`;
  const up = d > 0;
  const good = up === higherIsGood;
  return `<span class="delta ${up ? "up" : "down"} ${good ? "good" : "bad"}" title="vs the period before">${up ? "▲" : "▼"} ${pct(Math.abs(d))}</span>`;
}

// status renders an icon + label (never color alone).
function status(level, label) {
  const ico = { good: "✓", warning: "!", serious: "!", critical: "✕", idle: "–" }[level] || "•";
  return `<span class="status ${level}"><span class="ico" aria-hidden="true">${ico}</span>${esc(label)}</span>`;
}
function appState(a) {
  switch (a.state) {
    case "running": return status("good", "Running");
    case "deploying": return status("warning", "Deploying");
    case "crashed": return status("critical", "Crashed");
    case "stopped": return status("idle", "Stopped");
    case "new": return status("idle", "Not deployed");
    default: return status("idle", a.state || "–");
  }
}
const codeClass = (s) => "c" + Math.floor(s / 100);
const repoURL = (repo) => /^[\w.-]+\/[\w.-]+$/.test(repo || "") ? "https://github.com/" + repo : "";

// ---------- data ----------

let overview = null;
let lastOK = 0;

async function api(path) {
  const res = await fetch(path, { headers: { Accept: "application/json" }, redirect: "manual", cache: "no-store" });
  if (res.type === "opaqueredirect" || res.status === 401 || res.status === 302) throw new Error("signed-out");
  if (!res.ok) throw new Error((await res.text()).trim() || res.statusText);
  lastOK = Date.now();
  return res.json();
}

function tickUpdated() {
  const s = lastOK ? Math.round((Date.now() - lastOK) / 1000) : null;
  $("#updated").textContent = s === null ? "Loading…" : s < 3 ? "Live" : `Updated ${s}s ago`;
  $("#live-dot").classList.toggle("stale", !lastOK || s > 30);
}
setInterval(tickUpdated, 1000);

function showError(err) {
  if (err.message === "signed-out") {
    main.innerHTML = `<div class="error-box"><b>You've been signed out.</b> <a class="link" href="/">Sign in again</a></div>`;
    return;
  }
  main.innerHTML = `<div class="error-box"><b>Couldn't load this page.</b><div class="muted">${esc(err.message)}</div></div>`;
}

// ---------- shell ----------

function chrome(ov) {
  const d = ov.status.system.domain || "";
  $("#domain").textContent = d;
  document.title = d ? `Berth · ${d}` : "Berth admin";
  const email = ov.viewer?.email || "";
  $("#email").textContent = email;
  $("#avatar").textContent = (ov.viewer?.name || email || "?").trim()[0]?.toUpperCase() || "?";
  $("#nav-ai").classList.toggle("hidden", !ov.ai);
}

function markNav(name) {
  for (const a of document.querySelectorAll("#nav a")) a.classList.toggle("active", a.dataset.nav === name);
}

const theme = $("#theme");
theme.addEventListener("click", () => {
  const dark = matchMedia("(prefers-color-scheme: dark)").matches;
  const cur = document.documentElement.dataset.theme || (dark ? "dark" : "light");
  const next = cur === "dark" ? "light" : "dark";
  document.documentElement.dataset.theme = next;
  try { localStorage.setItem("berth-theme", next); } catch {}
  redraw();
});

// ---------- routing ----------

let timers = [];
let redraw = () => {};
function clearTimers() { timers.forEach(clearInterval); timers = []; }
function every(msec, fn) { timers.push(setInterval(() => { if (!document.hidden) fn(); }, msec)); }

async function route() {
  clearTimers();
  hideTip();
  redraw = () => {};
  const h = location.hash.replace(/^#/, "") || "/";
  const parts = h.split("/").filter(Boolean).map(decodeURIComponent);
  try {
    if (parts[0] === "apps" && parts[1]) await appPage(parts[1]);
    else if (parts[0] === "events") await eventsPage();
    else if (parts[0] === "ai") await aiPage();
    else await overviewPage();
  } catch (e) {
    showError(e);
  }
  window.scrollTo({ top: 0 });
}
window.addEventListener("hashchange", route);
let resizeT;
window.addEventListener("resize", () => { clearTimeout(resizeT); resizeT = setTimeout(() => redraw(), 120); });

// Apps get a fixed color by name order, so a filter or refresh never
// repaints them. Past seven apps, the rest share "Other".
function appColors(names) {
  const sorted = [...names].sort();
  const map = {};
  sorted.forEach((n, i) => { map[n] = i < 7 ? i + 1 : 0; });
  return map;
}

// ---------- overview ----------

async function overviewPage() {
  markNav("overview");
  overview = await api("/api/overview");
  chrome(overview);
  renderOverview();
  every(10000, async () => {
    try { overview = await api("/api/overview"); chrome(overview); renderOverview(); } catch {}
  });
}

function healthTiles(sys, ai) {
  const mem = sys.mem_physical ? sys.mem_committed / sys.mem_physical : 0;
  const disk = sys.disk_free_bytes;
  const tiles = [
    { label: "Containers", value: sys.container_running ? status("good", "Running") : status("critical", "Down"), foot: "Apple container system" },
    { label: "Proxy", value: sys.caddy_up ? status("good", "Up") : status("critical", "Down"), foot: "Caddy" },
    {
      label: "Tunnel",
      value: sys.tunnel_mode === "off" ? status("idle", "Off") : sys.tunnel_connected ? status("good", "Connected") : status("critical", "Down"),
      foot: sys.tunnel_mode === "off" ? "LAN only" : `Cloudflare (${esc(sys.tunnel_mode)})`,
    },
    {
      label: "Disk free",
      value: `<span class="num">${bytes(disk)}</span>`,
      foot: disk < 2 * 2 ** 30 ? status("critical", "Deploys refused") : disk < 10 * 2 ** 30 ? status("warning", "Getting low") : status("good", "Plenty"),
    },
    {
      label: "Memory committed",
      value: `<span class="num">${bytes(sys.mem_committed)}</span> <small>of ${bytes(sys.mem_physical)}</small>`,
      foot: `<div class="meter ${mem > 0.8 ? "hot" : ""}" style="width:100%"><span style="width:${Math.min(100, mem * 100).toFixed(1)}%"></span></div>`,
    },
  ];
  if (ai) {
    const loaded = sys.ai_loaded?.length ? `${esc(sys.ai_loaded.join(", "))} · ${bytes(sys.ai_mem)}` : "No model in memory";
    tiles.push({ label: "AI gateway", value: sys.ai === "down" ? status("critical", "Ollama down") : status("good", "Up"), foot: loaded });
  } else {
    tiles.push({ label: "berthd", value: status("good", "Up " + esc(sys.uptime)), foot: "version " + esc(sys.version) });
  }
  return tiles.map((t) => `<div class="card tile"><div class="label">${t.label}</div><div class="value">${t.value}</div><div class="foot">${t.foot}</div></div>`).join("");
}

function renderOverview() {
  const ov = overview;
  const sys = ov.status.system;
  const apps = ov.status.apps || [];
  const sums = ov.summaries || {};
  // Every app as one: latency percentiles can't be added up per app.
  const all = ov.all || { totals: {}, previous: {}, series: [] };
  const tot = all.totals, prev = all.previous;
  // Parents first, each followed by its dev version.
  const byName = Object.fromEntries(apps.map((a) => [a.name, a]));
  const ordered = [];
  for (const a of [...apps].sort((x, y) => x.name.localeCompare(y.name))) {
    if (a.name.endsWith("-dev") && byName[a.name.slice(0, -4)]) continue;
    ordered.push(a);
    if (byName[a.name + "-dev"]) ordered.push(byName[a.name + "-dev"]);
  }
  const colors = appColors(apps.map((a) => a.name));
  const warnings = sys.warnings || [];
  const events = ov.events || [];
  const services = ov.status.services || [];

  main.innerHTML = `
    <div class="page-head">
      <div><h1>Overview</h1><div class="sub">${esc(sys.domain || "LAN only")} · ${apps.length} app${apps.length === 1 ? "" : "s"} · berthd ${esc(sys.version)}, up ${esc(sys.uptime)}</div></div>
    </div>
    ${warnings.length ? `<div class="banner" role="alert">${status("warning", "")}<div><strong>Needs attention</strong><ul>${warnings.map((w) => `<li>${esc(w)}</li>`).join("")}</ul></div></div>` : ""}
    <div class="grid health">${healthTiles(sys, ov.ai)}</div>

    <div class="section">
      <h2>Traffic · last 24 hours</h2>
      <div class="grid kpis">
        ${kpi("Requests", num(tot.req), delta(tot.req, prev.req))}
        ${kpi("Visitors", num(tot.visitors), delta(tot.visitors, prev.visitors), "people, not bots")}
        ${kpi("Server errors", num(tot.s5), delta(tot.s5, prev.s5, false), tot.req ? pct(tot.s5 / tot.req) + " of requests" : "")}
        ${kpi("p50 latency", ms(tot.p50), delta(tot.p50, prev.p50, false), "half of requests are faster")}
        ${kpi("p95 latency", ms(tot.p95), delta(tot.p95, prev.p95, false), tot.p99 ? `p99 ${ms(tot.p99)}` : "")}
        ${kpi("Data served", bytes(tot.bytes), delta(tot.bytes, prev.bytes))}
      </div>
      <div class="grid cols-2" style="margin-top:16px">
        <div class="card">
          <div class="chart-head"><h2>Requests per hour, by app</h2><div class="legend" id="ov-legend"></div></div>
          <div class="chart" id="ov-chart"></div>
        </div>
        <div class="card">
          <div class="chart-head"><h2>Latency, all apps</h2><div class="legend">
            <span><i class="line" style="background:var(--muted)"></i>p50</span><span><i class="line" style="background:var(--ink-2)"></i>p95</span>
          </div></div>
          <div class="chart" id="ov-lat"></div>
        </div>
      </div>
    </div>

    <div class="section">
      <h2>Apps <span class="count">${apps.length}</span></h2>
      <div class="card flush"><div class="table-wrap"><table>
        <thead><tr><th>App</th><th>State</th><th>Release</th><th class="r">Requests 24h</th><th class="hide-sm"></th><th class="r">5xx</th><th class="r">p50</th><th class="r">p95</th><th class="r">Visitors</th><th>Memory</th><th>Up</th></tr></thead>
        <tbody>${ordered.map((a) => appRow(a, sums[a.name], byName)).join("") || `<tr><td colspan="11" class="empty">No apps yet. <span class="mono">berth create myapp --repo you/myapp</span></td></tr>`}</tbody>
      </table></div></div>
    </div>

    <div class="section grid cols-2">
      <div class="card flush">
        <h2>Services <span class="count">${services.length}</span></h2>
        <div class="table-wrap"><table>
          <thead><tr><th>Service</th><th>State</th><th>Memory</th><th>Linked to</th><th>Backup</th></tr></thead>
          <tbody>${services.map(serviceRow).join("") || `<tr><td colspan="5" class="empty">No databases. <span class="mono">berth pg:create mydb</span></td></tr>`}</tbody>
        </table></div>
      </div>
      <div class="card">
        <h2>Recent events <a class="link" href="#/events" style="margin-left:auto;font-weight:500">All events →</a></h2>
        ${eventList(events.slice(0, 12), true)}
      </div>
    </div>`;

  for (const tr of main.querySelectorAll("tr[data-app]")) {
    tr.addEventListener("click", (e) => { if (!e.target.closest("a")) location.hash = "#/apps/" + encodeURIComponent(tr.dataset.app); });
    const s = sums[tr.dataset.app];
    tr.querySelector(".spark-cell")?.appendChild(spark(s?.spark || [], s?.totals?.s5 ? "s1" : "s1"));
  }

  // Requests per hour, stacked by app, for the last 24 hours.
  const end = Math.floor(new Date(ov.now).getTime() / 3600000) * 3600;
  const points = [];
  for (let i = 0; i < 24; i++) points.push({ t: end - (23 - i) * 3600 });
  const series = [];
  const other = { key: "_other", label: "Other", cls: "sm", color: "var(--axis)" };
  for (const name of Object.keys(colors).sort()) {
    const s = sums[name];
    if (!s || !s.totals.req) continue;
    const c = colors[name];
    const key = c ? name : "_other";
    s.spark.forEach((v, i) => { if (points[i]) points[i][key] = (points[i][key] || 0) + v; });
    if (c) series.push({ key: name, label: name, cls: "s" + c, color: `var(--series-${c})` });
    else if (!series.includes(other)) series.push(other);
  }
  if (series.includes(other)) { series.splice(series.indexOf(other), 1); series.push(other); }
  $("#ov-legend").innerHTML = series.length > 1 ? series.map((s) => `<span><i style="background:${s.color}"></i>${esc(s.label)}</span>`).join("") : "";
  const fmtWhen = (t) => `${day(t)} ${clock(t)}–${clock(t + 3600)}`;
  const draw = () => {
    bars($("#ov-chart"), {
      points, series: series.length ? series : [{ key: "none", label: "Requests", cls: "s1", color: "var(--series-1)" }],
      fmtX: clock, fmtWhen, fmtY: num,
    });
    lines($("#ov-lat"), {
      points: all.series, fmtX: clock, fmtWhen, fmtY: (v) => (v ? ms(v) : "0"),
      series: [
        { key: "p50", label: "p50", cls: "sl", color: "var(--muted)" },
        { key: "p95", label: "p95", cls: "sn", color: "var(--ink-2)" },
      ],
    });
  };
  draw();
  redraw = draw;
}

function kpi(label, value, deltaHTML, foot = "") {
  return `<div class="card tile"><div class="label">${label}</div><div class="value num">${value}</div><div class="foot">${deltaHTML}${deltaHTML && foot ? " · " : ""}${foot}</div></div>`;
}

function memCell(a) {
  const limit = parseMem(a.memory_limit);
  const used = a.memory_bytes || 0;
  if (!used) return `<span class="muted">${esc(a.memory_limit || "–")}</span>`;
  const f = limit ? used / limit : 0;
  return `<div class="cell-meter"><span class="num">${bytes(used)}</span> <span class="muted">/ ${esc(a.memory_limit)}</span><div class="meter ${f > 0.85 ? "hot" : ""}"><span style="width:${Math.min(100, f * 100).toFixed(0)}%"></span></div></div>`;
}
function parseMem(s) {
  const m = /^(\d+(?:\.\d+)?)\s*([KMG]?)/i.exec(s || "");
  if (!m) return 0;
  return parseFloat(m[1]) * ({ K: 1024, M: 1024 ** 2, G: 1024 ** 3, "": 1024 ** 2 }[m[2].toUpperCase()]);
}

function appRow(a, s, byName) {
  const t = s?.totals || {};
  const dev = a.name.endsWith("-dev") && byName[a.name.slice(0, -4)];
  const rel = a.release ? `<span class="mono">v${a.release}</span> <span class="muted mono">${esc((a.sha || "").slice(0, 7))}</span><div class="sub-line" title="${esc(a.message)}">${esc(a.message)}</div>` : `<span class="muted">–</span>`;
  const deploy = a.last_deploy && a.last_deploy.status === "failed" ? `<div>${status("critical", `v${a.last_deploy.release} failed`)}</div>` : "";
  return `<tr class="clickable ${dev ? "dev" : ""}" data-app="${esc(a.name)}">
    <td><a class="app-name" href="#/apps/${encodeURIComponent(a.name)}">${dev ? "↳ " : ""}${esc(a.name)}</a>${a.urls?.[0] ? `<div class="sub-line"><a class="link" href="${esc(a.urls[0])}" target="_blank" rel="noopener">${esc(a.urls[0].replace(/^https?:\/\//, ""))}</a></div>` : ""}</td>
    <td>${appState(a)}${deploy}</td>
    <td>${rel}</td>
    <td class="r num">${num(t.req)}${s?.previous?.req ? `<div class="sub-line">${delta(t.req || 0, s.previous.req)}</div>` : ""}</td>
    <td class="spark-cell hide-sm"></td>
    <td class="r num">${t.s5 ? `<span class="code c5">${num(t.s5)}</span>` : `<span class="muted">0</span>`}</td>
    <td class="r num">${ms(t.p50)}</td>
    <td class="r num">${ms(t.p95)}</td>
    <td class="r num">${num(t.visitors)}</td>
    <td>${memCell(a)}</td>
    <td class="num">${a.state === "running" ? since(a.since) : "–"}</td>
  </tr>`;
}

// Dumps are named YYYYMMDD-HHMMSS.<ext>.
function backupAge(path) {
  const m = /(\d{4})(\d{2})(\d{2})-(\d{2})(\d{2})(\d{2})\.\w+$/.exec(path || "");
  if (!m) return `<span class="muted">none yet</span>`;
  const t = new Date(+m[1], m[2] - 1, +m[3], +m[4], +m[5], +m[6]);
  return `<span title="${esc(path.split("/").pop())}">${ago(t.toISOString())}</span>`;
}

function serviceRow(s) {
  return `<tr>
    <td><b>${esc(s.kind)}:${esc(s.name)}</b><div class="sub-line mono">${esc(s.image)}</div></td>
    <td>${s.running ? status("good", "Running") : status("critical", "Down")}</td>
    <td class="num">${s.memory_bytes ? bytes(s.memory_bytes) : "–"}</td>
    <td><div class="chips">${(s.links || []).map((l) => `<a class="pill" href="#/apps/${encodeURIComponent(l)}">${esc(l)}</a>`).join("") || `<span class="muted">–</span>`}</div></td>
    <td>${backupAge(s.last_backup)}</td>
  </tr>`;
}

// ---------- events ----------

const evIcons = {
  "deploy.ok": ["good", "✓"], "deploy.failed": ["critical", "✕"], crash: ["critical", "✕"], "deploy.start": ["info", "↑"],
  rollback: ["warning", "↺"], restart: ["info", "↻"], webhook: ["info", "⚡"], network: ["warning", "!"], destroy: ["", "✕"],
  "service.start": ["good", "▶"], stop: ["", "■"], "dev.sleep": ["", "z"], "dev.create": ["info", "+"], create: ["info", "+"], backup: ["good", "✓"],
};
function eventList(events, compact) {
  if (!events.length) return `<div class="empty">Nothing yet.</div>`;
  return `<ul class="events">${events.map((e) => {
    const [cls, ico] = evIcons[e.kind] || ["", "•"];
    return `<li><span class="ev-ico ${cls}" aria-hidden="true">${ico}</span>
      <div class="what">${e.app ? `<a class="link" href="#/apps/${encodeURIComponent(e.app)}"><b>${esc(e.app)}</b></a> ` : ""}<span class="muted">${esc(e.kind)}</span><div class="ink2">${esc(e.message)}</div></div>
      <time title="${esc(new Date(e.time).toLocaleString())}">${compact ? ago(e.time) : stamp(e.time)}</time></li>`;
  }).join("")}</ul>`;
}

async function eventsPage() {
  markNav("events");
  if (!overview) { overview = await api("/api/overview"); chrome(overview); }
  let data = await api("/api/events?n=500");
  const apps = [...new Set(data.events.map((e) => e.app).filter(Boolean))].sort();
  const kinds = [...new Set(data.events.map((e) => e.kind))].sort();
  main.innerHTML = `
    <div class="page-head"><div><h1>Events</h1><div class="sub">Deploys, crashes, webhooks and everything else berthd recorded</div></div></div>
    <div class="filters">
      <select id="f-app" aria-label="App"><option value="">All apps</option>${apps.map((a) => `<option>${esc(a)}</option>`).join("")}</select>
      <select id="f-kind" aria-label="Kind"><option value="">All kinds</option>${kinds.map((k) => `<option>${esc(k)}</option>`).join("")}</select>
      <input type="search" id="f-q" placeholder="Search messages" aria-label="Search messages">
    </div>
    <div class="card" id="ev-list"></div>`;
  const draw = () => {
    const a = $("#f-app").value, k = $("#f-kind").value, q = $("#f-q").value.toLowerCase();
    const list = data.events.filter((e) => (!a || e.app === a) && (!k || e.kind === k) && (!q || (e.message || "").toLowerCase().includes(q)));
    $("#ev-list").innerHTML = eventList(list.slice(0, 300), false);
  };
  for (const id of ["#f-app", "#f-kind", "#f-q"]) $(id).addEventListener("input", draw);
  draw();
  every(15000, async () => { try { data = await api("/api/events?n=500"); draw(); } catch {} });
}

// ---------- app page ----------

let appRange = "24h";
let appTab = "live";

async function appPage(name) {
  markNav("");
  if (!overview) { overview = await api("/api/overview").catch(() => null); if (overview) chrome(overview); }
  let det = await api(`/api/apps/${encodeURIComponent(name)}?range=${appRange}`);
  let liveSeq = det.report.seq;
  let live = det.report.recent || [];
  renderApp(name, det, live);

  const refresh = async () => {
    try {
      det = await api(`/api/apps/${encodeURIComponent(name)}?range=${appRange}`);
      renderAppTop(det);
    } catch {}
  };
  every(10000, refresh);
  every(3000, async () => {
    if (appTab !== "live") return;
    try {
      const r = await api(`/api/apps/${encodeURIComponent(name)}/live?since=${liveSeq}`);
      if (r.seq !== liveSeq && r.requests.length) {
        live = [...r.requests, ...live].slice(0, 100);
        liveSeq = r.seq;
        renderLive(live, r.requests.length);
      }
    } catch {}
  });
  every(5000, () => { if (appTab === "logs") loadLogs(name, false); });
  current = { name, refresh, tab: () => renderTab(name, det, live) };
}

// One delegated handler for the app page's range and tab controls.
let current = null;
main.addEventListener("click", async (e) => {
  if (!current || !location.hash.startsWith("#/apps/")) return;
  const rb = e.target.closest("[data-range]");
  if (rb) {
    appRange = rb.dataset.range;
    main.querySelectorAll("[data-range]").forEach((b) => b.classList.toggle("on", b === rb));
    $("#app-top")?.classList.add("fade");
    await current.refresh();
    $("#app-top")?.classList.remove("fade");
  }
  const tb = e.target.closest("[data-tab]");
  if (tb) {
    appTab = tb.dataset.tab;
    main.querySelectorAll("[data-tab]").forEach((b) => b.classList.toggle("on", b === tb));
    current.tab();
  }
});

function renderApp(name, det, live) {
  const a = det.app, info = det.info;
  const repo = repoURL(info.repo);
  const sha = (a.sha || "").slice(0, 7);
  const parent = info.dev_of;
  main.innerHTML = `
    <div class="crumbs"><a href="#/">Overview</a> / ${parent ? `<a href="#/apps/${encodeURIComponent(parent)}">${esc(parent)}</a> / ` : ""}${esc(name)}</div>
    <div class="app-head">
      <div>
        <h1>${esc(name)} ${appState(a)}</h1>
        <div class="meta">
          ${(a.urls || []).map((u) => `<a class="link" href="${esc(u)}" target="_blank" rel="noopener">${esc(u.replace(/^https?:\/\//, ""))} ↗</a>`).join("")}
          ${info.repo ? `<span>${repo ? `<a href="${repo}" target="_blank" rel="noopener">${esc(info.repo)}</a>` : esc(info.repo)}${info.branch ? ` @ ${esc(info.branch)}` : ""}</span>` : parent ? `<span>dev version of <a href="#/apps/${encodeURIComponent(parent)}">${esc(parent)}</a> · deployed with <span class="mono">berth dev:push</span></span>` : `<span class="muted">push-only (no repo)</span>`}
          ${a.release ? `<span><span class="mono">v${a.release}</span> · ${repo && sha ? `<a class="mono" href="${repo}/commit/${esc(a.sha)}" target="_blank" rel="noopener">${esc(sha)}</a>` : `<span class="mono">${esc(sha)}</span>`} · ${esc(a.message || "")}</span>` : ""}
          ${a.state === "running" ? `<span>up ${since(a.since)}</span>` : ""}
          ${info.dev ? `<a class="link" href="#/apps/${encodeURIComponent(info.dev)}">dev version →</a>` : ""}
        </div>
      </div>
      <div class="seg" role="group" aria-label="Time range">${["1h", "24h", "7d"].map((r) => `<button data-range="${r}" class="${r === appRange ? "on" : ""}">${r}</button>`).join("")}</div>
    </div>
    <div id="app-top"></div>
    <div class="tabs" role="tablist">
      ${[["live", "Live requests"], ["releases", "Releases"], ["logs", "Logs"], ["config", "Setup"], ["events", "Events"]].map(([k, l]) => `<button role="tab" data-tab="${k}" class="${appTab === k ? "on" : ""}">${l}</button>`).join("")}
    </div>
    <div id="tab"></div>`;
  renderAppTop(det);
  renderTab(name, det, live);
}

const rangeLabel = { "1h": "last hour", "24h": "last 24 hours", "7d": "last 7 days" };

function renderAppTop(det) {
  const r = det.report, t = r.totals, p = r.previous;
  const errRate = t.req ? t.s5 / t.req : 0, prevErr = p.req ? p.s5 / p.req : 0;
  const people = t.req - t.bots;
  const top = $("#app-top");
  if (!top) return;
  top.innerHTML = `
    <div class="grid kpis">
      ${kpi("Requests", num(t.req), delta(t.req, p.req), rangeLabel[appRange])}
      ${kpi("Visitors", num(t.visitors), delta(t.visitors, p.visitors), people > 0 ? pct(t.mobile / people) + " mobile" : "")}
      ${kpi("Server errors", pct(errRate), delta(errRate, prevErr, false), `${num(t.s5)} 5xx · ${num(t.s4)} 4xx`)}
      ${kpi("p95 latency", ms(t.p95), delta(t.p95, p.p95, false), `p50 ${ms(t.p50)} · p99 ${ms(t.p99)}`)}
      ${kpi("Data served", bytes(t.bytes), delta(t.bytes, p.bytes))}
      ${kpi("Bots & scripts", pct(t.req ? t.bots / t.req : 0), "", `${num(t.bots)} requests`)}
    </div>
    <div class="grid cols-2" style="margin-top:16px">
      <div class="card">
        <div class="chart-head"><h2>Requests</h2><div class="legend">
          <span><i style="background:var(--series-1)"></i>OK (1xx–3xx)</span><span><i style="background:var(--warning)"></i>4xx</span><span><i style="background:var(--critical)"></i>5xx</span>
        </div></div>
        <div class="chart" id="c-req"></div>
      </div>
      <div class="card">
        <div class="chart-head"><h2>Latency</h2><div class="legend">
          <span><i class="line" style="background:var(--series-1)"></i>p50</span><span><i class="line" style="background:var(--series-2)"></i>p95</span>
        </div></div>
        <div class="chart" id="c-lat"></div>
      </div>
    </div>
    <div class="grid cols-2" style="margin-top:16px">
      ${topCard("Pages", r.paths, (k) => `<span class="mono">${esc(k)}</span>`, t.req)}
      ${topCard("Referrers", r.referrers, (k) => esc(k), t.req, "No other sites have sent visitors yet")}
      ${topCard("Countries", r.countries, (k) => `${flag(k)} ${esc(country(k))}`, t.req, "Countries come from Cloudflare; none recorded")}
      ${topCard("Browsers", r.browsers, (k) => esc(k), t.req)}
    </div>`;
  const step = r.step;
  const fmtX = step >= 3600 ? (x) => (new Date(x * 1000).getHours() === 0 ? day(x) : clock(x)) : clock;
  const fmtWhen = (x) => step >= 86400 ? day(x) : `${day(x)} ${clock(x)}–${clock(x + step)}`;
  const draw = () => {
    bars($("#c-req"), {
      points: r.series, fmtX, fmtWhen, fmtY: num,
      series: [
        { key: "s2", label: "OK", cls: "s1", color: "var(--series-1)" },
        { key: "s4", label: "4xx", cls: "sw", color: "var(--warning)" },
        { key: "s5", label: "5xx", cls: "sc", color: "var(--critical)" },
      ],
    });
    lines($("#c-lat"), {
      points: r.series, fmtX, fmtWhen, fmtY: (v) => (v ? ms(v) : "0"),
      series: [
        { key: "p50", label: "p50", cls: "s1", color: "var(--series-1)" },
        { key: "p95", label: "p95", cls: "s2", color: "var(--series-2)" },
      ],
    });
  };
  draw();
  redraw = draw;
}

function topCard(title, rows, label, total, empty = "Nothing yet") {
  const max = Math.max(...rows.map((r) => r.n), 1);
  return `<div class="card"><h2>${title}</h2>${rows.length ? `<ul class="toplist">${rows.map((r) => `
    <li title="${esc(r.key)}: ${r.n}"><span class="bar" style="width:${((r.n / max) * 100).toFixed(1)}%"></span><span>${label(r.key)}</span><span class="n">${num(r.n)}${total ? ` <span class="muted">${pct(r.n / total)}</span>` : ""}</span></li>`).join("")}</ul>` : `<div class="toplist"><div class="none">${empty}</div></div>`}</div>`;
}

function renderTab(name, det, live) {
  const tab = $("#tab");
  if (!tab) return;
  if (appTab === "live") return renderLive(live, 0);
  if (appTab === "releases") {
    tab.innerHTML = `<div class="card flush"><div class="table-wrap"><table>
      <thead><tr><th>Release</th><th>Status</th><th>Commit</th><th>Trigger</th><th>When</th><th class="r">Build</th><th class="r">Release cmd</th></tr></thead>
      <tbody>${det.releases.map((r) => {
        const st = r.current ? status("good", "Live") : r.status === "failed" ? status("critical", `Failed${r.failed_stage ? " (" + r.failed_stage + ")" : ""}`) : r.status === "running" ? status("good", "Running") : status("idle", r.status);
        return `<tr><td class="mono">v${r.n}</td><td>${st}${r.error ? `<div class="sub-line" title="${esc(r.error)}">${esc(r.error)}</div>` : ""}</td>
          <td><span class="mono">${esc((r.sha || "").slice(0, 7))}</span><div class="sub-line" title="${esc(r.message)}">${esc(r.message)}</div></td>
          <td><span class="pill">${esc(r.trigger || "–")}</span></td><td title="${esc(new Date(r.created_at).toLocaleString())}">${ago(r.created_at)}</td>
          <td class="r num">${r.build_secs ? r.build_secs.toFixed(1) + " s" : "–"}</td>
          <td class="r num" title="${esc(r.release_cmd || "")}">${r.release_cmd ? (r.release_secs || 0).toFixed(1) + " s" : "–"}</td></tr>`;
      }).join("") || `<tr><td colspan="7" class="empty">No releases yet</td></tr>`}</tbody></table></div></div>
      <p class="muted">Roll back from a terminal: <span class="mono">berth rollback ${esc(name)} [n]</span></p>`;
    return;
  }
  if (appTab === "logs") {
    tab.innerHTML = `<div class="card"><div class="chart-head"><h2>App output <span class="count">last 300 lines, refreshing</span></h2>
      <label class="muted"><input type="checkbox" id="follow" checked> Follow</label></div><pre class="logs" id="logs">Loading…</pre></div>`;
    loadLogs(name, true);
    return;
  }
  if (appTab === "events") {
    tab.innerHTML = `<div class="card">${eventList(det.events, false)}</div>`;
    return;
  }
  // Setup.
  const info = det.info;
  const access = info.access ? policy(info.access) : info.private ? "LAN only (private)" : "Public";
  const ai = info.ai ? `${esc((info.ai.models || []).join(", ") || "any model")}${info.ai.monthly_tokens ? ` · ${num(info.ai.monthly_tokens)} tokens a month` : ""}${info.ai.rpm ? ` · ${info.ai.rpm} requests a minute` : ""}` : `<span class="muted">off (berth ai:enable ${esc(name)})</span>`;
  tab.innerHTML = `<div class="grid cols-2">
    <div class="card"><h2>How it runs</h2><dl class="facts">
      <dt>Repo</dt><dd>${info.repo ? esc(info.repo) : "–"}${info.path ? ` <span class="muted">(${esc(info.path)})</span>` : ""}</dd>
      <dt>Branch</dt><dd>${esc(info.branch || "–")}</dd>
      <dt>Who can visit</dt><dd>${access}${info.basic_auth ? " · password" : ""}</dd>
      <dt>Domains</dt><dd>${(info.domains || []).map(esc).join(", ") || "–"}</dd>
      <dt>Services</dt><dd><div class="chips">${(info.links || []).map((l) => `<span class="pill">${esc(l)}</span>`).join("") || "–"}</div></dd>
      <dt>Storage</dt><dd>${(info.mounts || []).map((m) => `<span class="mono">${esc(m)}</span>`).join(", ") || "–"}</dd>
      <dt>AI</dt><dd>${ai}</dd>
      <dt>Memory</dt><dd>${esc(det.app.memory_limit || "–")}</dd>
      <dt>Created</dt><dd>${ago(info.created)}</dd>
    </dl></div>
    <div class="card"><h2>Config variables <span class="count">${info.env_keys.length} · values hidden</span></h2>
      <div class="chips">${info.env_keys.map((k) => `<span class="pill mono">${esc(k)}</span>`).join("") || `<span class="muted">none</span>`}</div>
      <p class="muted">See values from a terminal: <span class="mono">berth config:show ${esc(name)}</span></p>
      ${det.ai_usage?.length ? `<h2 style="margin-top:18px">AI this month</h2><table><thead><tr><th>Model</th><th class="r">Requests</th><th class="r">Tokens in</th><th class="r">Tokens out</th></tr></thead><tbody>${det.ai_usage.map((u) => `<tr><td class="mono">${esc(u.model || "(refused)")}</td><td class="r num">${num(u.requests)}</td><td class="r num">${num(u.in)}</td><td class="r num">${num(u.out)}</td></tr>`).join("")}</tbody></table>` : ""}
    </div></div>`;
}

function policy(a) {
  const rule = (lvl, allow) => lvl === "allow" ? `only ${esc((allow || []).join(", "))}` : lvl === "signed-in" ? "anyone signed in" : esc(lvl);
  const paths = (a.paths || []).map((p) => `<div><span class="mono">${esc(p.path)}</span>: ${rule(p.level, p.allow)}</div>`).join("");
  return `${rule(a.level, a.allow)}${paths}`;
}

function renderLive(live, fresh) {
  const tab = $("#tab");
  if (!tab || appTab !== "live") return;
  tab.innerHTML = `<div class="card flush"><div class="table-wrap"><table>
    <thead><tr><th>Time</th><th>Status</th><th>Request</th><th class="r">Duration</th><th class="r hide-sm">Size</th><th>From</th><th class="hide-sm">Browser</th></tr></thead>
    <tbody>${live.map((r, i) => `<tr class="${i < fresh ? "fresh" : ""}">
      <td class="num muted" title="${esc(new Date(r.t).toLocaleString())}">${new Date(r.t).toLocaleTimeString()}</td>
      <td><span class="code ${codeClass(r.status)}">${r.status}</span></td>
      <td><span class="muted mono">${esc(r.method)}</span> <span class="mono">${esc(r.path)}</span>${r.referrer ? `<div class="sub-line">from ${esc(r.referrer)}</div>` : ""}</td>
      <td class="r num">${ms(r.ms)}</td>
      <td class="r num hide-sm">${bytes(r.bytes)}</td>
      <td>${r.country ? `<span title="${esc(country(r.country))}">${flag(r.country)}</span> ` : ""}<span class="mono muted">${esc(r.ip)}</span></td>
      <td class="hide-sm">${esc(r.browser)}${r.mobile ? " · mobile" : ""}${r.bot ? ` <span class="pill">bot</span>` : ""}</td>
    </tr>`).join("") || `<tr><td colspan="7" class="empty">No requests yet. New ones appear here as they arrive.</td></tr>`}</tbody>
  </table></div></div>`;
}

async function loadLogs(name, first) {
  const pre = $("#logs");
  if (!pre) return;
  try {
    const r = await api(`/api/apps/${encodeURIComponent(name)}/logs?n=300`);
    const atBottom = pre.scrollTop + pre.clientHeight >= pre.scrollHeight - 20;
    pre.textContent = r.lines.join("\n") || "No output yet.";
    if (first || ($("#follow")?.checked && atBottom)) pre.scrollTop = pre.scrollHeight;
  } catch (e) {
    pre.textContent = "Couldn't load logs: " + e.message;
  }
}

// ---------- AI ----------

async function aiPage() {
  markNav("ai");
  overview = await api("/api/overview");
  chrome(overview);
  renderAI();
  every(15000, async () => {
    try { overview = await api("/api/overview"); renderAI(); } catch {}
  });
}

function renderAI() {
  const ai = overview.ai;
  if (!ai) {
    main.innerHTML = `<div class="page-head"><div><h1>AI gateway</h1><div class="sub">Not set up. Run <span class="mono">berth ai:setup</span>.</div></div></div>`;
    return;
  }
  main.innerHTML = `
    <div class="page-head"><div><h1>AI gateway</h1><div class="sub">Apps reach it at <span class="mono">${esc(ai.url)}</span> with their own tokens</div></div></div>
    <div class="grid cols-3">
      <div class="card tile"><div class="label">Ollama</div><div class="value">${ai.ollama_error ? status("critical", "Not answering") : status("good", "Up")}</div><div class="foot">${ai.ollama_version ? "version " + esc(ai.ollama_version) + " · " : ""}${esc(ai.ollama_url)}</div></div>
      <div class="card tile"><div class="label">In memory</div><div class="value num">${(ai.loaded || []).length ? bytes(ai.loaded.reduce((s, m) => s + m.size, 0)) : "–"}</div><div class="foot">${(ai.loaded || []).map((m) => esc(m.name)).join(", ") || "no model loaded"}</div></div>
      <div class="card tile"><div class="label">Tokens this month</div><div class="value num">${num((ai.apps || []).reduce((s, a) => s + a.used_tokens, 0))}</div><div class="foot">across ${(ai.apps || []).length} app${(ai.apps || []).length === 1 ? "" : "s"}</div></div>
    </div>
    <div class="section grid cols-2">
      <div class="card flush"><h2>Apps</h2><div class="table-wrap"><table><thead><tr><th>App</th><th>Models</th><th class="r">Tokens this month</th><th>Budget</th></tr></thead><tbody>
        ${(ai.apps || []).map((a) => {
          const f = a.monthly_tokens ? a.used_tokens / a.monthly_tokens : 0;
          return `<tr><td><a class="app-name" href="#/apps/${encodeURIComponent(a.app)}">${esc(a.app)}</a></td><td class="mono">${esc((a.models || []).join(", ") || "any")}</td><td class="r num">${num(a.used_tokens)}</td>
            <td class="cell-meter">${a.monthly_tokens ? `${pct(f)} of ${num(a.monthly_tokens)}<div class="meter ${f > 0.85 ? "hot" : ""}"><span style="width:${Math.min(100, f * 100).toFixed(0)}%"></span></div>` : `<span class="muted">no cap</span>`}</td></tr>`;
        }).join("") || `<tr><td colspan="4" class="empty">No apps yet. <span class="mono">berth ai:enable myapp</span></td></tr>`}
      </tbody></table></div></div>
      <div class="card"><h2>Aliases</h2><dl class="facts">${Object.keys(ai.aliases || {}).sort().map((k) => `<dt class="mono">${esc(k)}</dt><dd class="mono">${esc(ai.aliases[k])}</dd>`).join("") || `<dt class="muted">none</dt><dd></dd>`}</dl>
        <h2 style="margin-top:18px">Providers</h2><dl class="facts">
          <dt class="mono">local</dt><dd>${(ai.local || []).map((m) => `<span class="mono">${esc(m.name)}</span> <span class="muted">${bytes(m.size)}</span>`).join(", ") || `<span class="muted">no models downloaded</span>`}</dd>
          ${(ai.providers || []).map((p) => `<dt class="mono">${esc(p.name)}</dt><dd>${p.has_key ? status("good", "Key set") : status("idle", "No key")} <span class="muted mono">${esc(p.url)}</span></dd>`).join("")}
        </dl></div>
    </div>`;
}

route();
