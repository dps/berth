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

// post changes something. Only the Tools page does: everything else on this
// page is read-only. berthd refuses a POST that didn't come from this page.
async function post(path, body) {
  const res = await fetch(path, { method: "POST", headers: { "Content-Type": "application/json", Accept: "application/json" }, body: JSON.stringify(body), redirect: "manual", cache: "no-store" });
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
    else if (parts[0] === "tools") await toolsPage();
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
  [overview, load] = await Promise.all([api("/api/overview"), fetchLoad()]);
  chrome(overview);
  renderOverview();
  every(10000, async () => {
    try { [overview, load] = await Promise.all([api("/api/overview"), fetchLoad()]); chrome(overview); renderOverview(); } catch {}
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

    <div class="section" id="load"></div>

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
        <thead><tr><th>App</th><th>State</th><th>Release</th><th class="r">Requests 24h</th><th class="hide-sm"></th><th class="r">5xx</th><th class="r">p50</th><th class="r">p95</th><th class="r">Visitors</th><th class="r">CPU</th><th>Memory</th><th>Up</th></tr></thead>
        <tbody>${ordered.map((a) => appRow(a, sums[a.name], byName)).join("") || `<tr><td colspan="12" class="empty">No apps yet. <span class="mono">berth create myapp --repo you/myapp</span></td></tr>`}</tbody>
      </table></div></div>
    </div>

    <div class="section grid cols-2">
      <div class="card flush">
        <h2>Services <span class="count">${services.length}</span></h2>
        <div class="table-wrap"><table>
          <thead><tr><th>Service</th><th>State</th><th class="r">CPU</th><th>Memory</th><th>Linked to</th><th>Backup</th></tr></thead>
          <tbody>${services.map(serviceRow).join("") || `<tr><td colspan="6" class="empty">No databases. <span class="mono">berth pg:create mydb</span></td></tr>`}</tbody>
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
  renderLoad();
  redraw = () => { draw(); renderLoad(); };
}

// ---------- system load ----------

let load = null;
let loadRange = "1h";
const fetchLoad = () => api("/api/load?range=" + loadRange).catch(() => load);

// Cores used, as a share of one core (Activity Monitor's convention: a
// busy app on four cores shows 400%).
function corePct(v) {
  if (v == null) return "–";
  const x = v * 100;
  if (x === 0) return "0%";
  if (x < 0.1) return "<0.1%";
  return x.toFixed(x < 10 ? 1 : 0) + "%";
}
function cpuCell(name) {
  const latest = load?.latest;
  if (!latest?.apps) return `<span class="muted">–</span>`;
  const v = latest.apps[name];
  return v == null ? `<span class="muted">–</span>` : corePct(v);
}

function renderLoad() {
  const box = $("#load");
  if (!box) return;
  const rep = load;
  const L = rep?.latest;
  const seg = `<div class="seg" role="group" aria-label="Load time range">${["1h", "24h"].map((r) => `<button data-load-range="${r}" class="${r === loadRange ? "on" : ""}">${r}</button>`).join("")}</div>`;
  const head = `<div class="chart-head section-head"><h2>System load</h2>${seg}</div>`;
  if (!L) {
    box.innerHTML = head + `<div class="card empty">Collecting the first sample…</div>`;
    return;
  }
  const cores = L.cores || 1;
  const per = L.load[0] / cores;
  const busiest = Object.entries(L.apps || {}).sort((a, b) => b[1] - a[1])[0];
  const mem = L.mem_pressure;
  const peak = rep.peak;
  box.innerHTML = head + `
    <div class="grid kpis load-kpis">
      ${kpi("Load average", `${L.load[0].toFixed(2)} <small>on ${cores} cores</small>`, per >= 1 ? status("critical", "Overloaded") : per >= 0.7 ? status("warning", "Busy") : "",
        `5 min ${L.load[1].toFixed(2)} · 15 min ${L.load[2].toFixed(2)}`)}
      ${kpi("CPU", L.cpu < 0 ? "–" : pct(L.cpu), "", busiest && busiest[1] >= 0.005 ? `busiest: ${esc(busiest[0])} ${corePct(busiest[1])}` : "all apps idle")}
      ${kpi("Memory in use", pct(mem), mem >= 0.9 ? status("critical", "Under pressure") : mem >= 0.8 ? status("warning", "Tight") : "",
        `<div class="meter ${mem >= 0.8 ? "hot" : ""}" style="width:100%"><span style="width:${(mem * 100).toFixed(1)}%"></span></div>`)}
      ${kpi("Swap used", bytes(L.swap_used), "", L.swap_total ? `${bytes(L.swap_total)} of swap files` : "no swap in use")}
      ${kpi(`Peak load, ${loadRange}`, peak ? peak.load[0].toFixed(2) : "–", "", peak ? `at ${clock(peak.t)}` : "")}
    </div>
    <div class="grid cols-2" style="margin-top:16px">
      <div class="card">
        <div class="chart-head"><h2>Load average</h2><div class="legend">
          <span><i class="line" style="background:var(--series-1)"></i>1 min</span><span><i class="line" style="background:var(--muted)"></i>15 min</span>
        </div></div>
        <div class="chart short" id="load-avg"></div>
      </div>
      <div class="card">
        <div class="chart-head"><h2>CPU and memory</h2><div class="legend">
          <span><i class="line" style="background:var(--series-1)"></i>CPU busy</span><span><i class="line" style="background:var(--series-2)"></i>Memory in use</span>
        </div></div>
        <div class="chart short" id="load-cpu"></div>
      </div>
    </div>
    <div class="card" style="margin-top:16px">
      <div class="chart-head"><h2>CPU by app <span class="count">% of one core</span></h2><div class="legend" id="load-legend"></div></div>
      <div class="chart short" id="load-apps"></div>
    </div>`;

  const step = rep.step;
  const fmtWhen = (t) => step <= 60 ? clock(t) : `${day(t)} ${clock(t)}–${clock(t + step)}`;
  const pts = rep.points;
  const v = (p, f) => (p.n ? f(p) : null);
  lines($("#load-avg"), {
    points: pts.map((p) => ({ t: p.t, l1: v(p, (p) => p.load[0]), l15: v(p, (p) => p.load[2]) })),
    series: [
      { key: "l15", label: "15 min", cls: "sl", color: "var(--muted)" },
      { key: "l1", label: "1 min", cls: "s1", color: "var(--series-1)" },
    ],
    fmtX: clock, fmtWhen, fmtY: (x) => String(+x.toFixed(2)), zeroIsGap: false, empty: "No samples yet",
  });
  lines($("#load-cpu"), {
    points: pts.map((p) => ({ t: p.t, cpu: p.n && p.cpu >= 0 ? p.cpu * 100 : null, mem: v(p, (p) => p.mem_pressure * 100) })),
    series: [
      { key: "cpu", label: "CPU busy", cls: "s1", color: "var(--series-1)" },
      { key: "mem", label: "Memory in use", cls: "s2", color: "var(--series-2)" },
    ],
    fmtX: clock, fmtWhen, fmtY: (x) => Math.round(x) + "%", zeroIsGap: false, yMax: 100, empty: "No samples yet",
  });

  // Apps keep the colors they have everywhere else; services and apps
  // past the seventh share one gray.
  const colors = appColors((overview?.status?.apps || []).map((a) => a.name));
  const keys = new Set();
  pts.forEach((p) => Object.keys(p.apps || {}).forEach((k) => keys.add(k)));
  const series = [];
  const other = { key: "_other", label: "Services & others", cls: "sm", color: "var(--axis)" };
  const points = pts.map((p) => {
    const o = { t: p.t };
    for (const [k, c] of Object.entries(p.apps || {})) {
      const key = colors[k] ? k : "_other";
      o[key] = (o[key] || 0) + c * 100;
    }
    return o;
  });
  for (const k of [...keys].sort()) {
    if (!colors[k]) continue;
    if (!points.some((p) => p[k] >= 0.05)) continue; // idle all along
    series.push({ key: k, label: k, cls: "s" + colors[k], color: `var(--series-${colors[k]})` });
  }
  if (points.some((p) => p._other >= 0.05)) series.push(other);
  $("#load-legend").innerHTML = series.length > 1 ? series.map((s) => `<span><i style="background:${s.color}"></i>${esc(s.label)}</span>`).join("") : "";
  bars($("#load-apps"), {
    points, series: series.length ? series : [{ key: "none", label: "CPU", cls: "s1", color: "var(--series-1)" }],
    fmtX: clock, fmtWhen, fmtY: (x) => corePct(x / 100), empty: "Apps have been idle",
  });
}

main.addEventListener("click", async (e) => {
  const b = e.target.closest("[data-load-range]");
  if (!b || b.dataset.loadRange === loadRange) return;
  loadRange = b.dataset.loadRange;
  main.querySelectorAll("[data-load-range]").forEach((x) => x.classList.toggle("on", x === b));
  $("#load")?.classList.add("fade");
  load = await fetchLoad();
  $("#load")?.classList.remove("fade");
  renderLoad();
});

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
    <td class="r num">${cpuCell(a.name)}</td>
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
    <td class="r num">${cpuCell(s.kind + ":" + s.name)}</td>
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
      ${info.sqlite ? `<dt>SQLite</dt><dd><span class="mono">${esc(info.sqlite.env_var)}</span> → <span class="mono">/data/sqlite/app.db</span> · last backup ${info.sqlite.last_backup ? backupAge(info.sqlite.last_backup) : `<span class="muted">none yet</span>`}</dd>` : ""}
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

// ---------- tools ----------

let tools = null;
let toolsEditing = null; // the app whose access is open for editing

async function toolsPage() {
  markNav("tools");
  overview = await api("/api/overview");
  chrome(overview);
  tools = await api("/api/tools");
  toolsEditing = null;
  main.innerHTML = `<div id="tools-page">
    <div class="page-head"><div><h1>Tools</h1><div class="sub">Add a key once and every app you allow can use it. Apps never see your keys.</div></div></div>
    <div class="grid cols-3" id="tools-tiles"></div>
    <div class="notice hidden" id="tools-notice" role="status"></div>
    <div class="section"><h2>API keys</h2><div class="provider-grid" id="tools-keys"></div><p class="muted fine" id="tools-keyless"></p></div>
    <div class="section"><div class="card flush"><h2>Tools <span class="count" id="tools-count"></span></h2><div class="table-wrap" id="tools-list"></div></div></div>
    <div class="section"><div class="card flush"><h2>Apps</h2><div class="table-wrap" id="tools-apps"></div></div></div>
  </div>`;
  const page = $("#tools-page");
  page.addEventListener("click", toolsClick);
  page.addEventListener("submit", toolsSubmit);
  renderTools();
}

function renderTools() {
  renderToolsTiles();
  renderToolsKeys();
  renderToolsList();
  renderToolsApps();
}

function renderToolsTiles() {
  const list = tools.tools || [], keyed = (tools.providers || []).filter((p) => p.signup), apps = tools.apps || [];
  const ready = list.filter((t) => (t.providers || []).length).length;
  $("#tools-tiles").innerHTML =
    `<div class="card tile"><div class="label">Tools ready</div><div class="value num">${ready} <small>of ${list.length}</small></div><div class="foot">${ready === list.length ? "All set" : "Add a key to enable the rest"}</div></div>
     <div class="card tile"><div class="label">Keys saved</div><div class="value num">${keyed.filter((p) => p.has_key).length} <small>of ${keyed.length}</small></div><div class="foot">kept on this Mac</div></div>
     <div class="card tile"><div class="label">Calls this month</div><div class="value num">${num(apps.reduce((s, a) => s + a.used, 0))}</div><div class="foot">across ${apps.length} app${apps.length === 1 ? "" : "s"}</div></div>`;
}

function providerCard(p, typed) {
  const budget = p.budget ? ` of ${num(p.budget)}` : "";
  return `<form class="card provider" data-provider="${esc(p.name)}" autocomplete="off">
    <div class="provider-head">
      <strong>${esc(p.title)}</strong>
      ${p.shared ? status("good", "Using AI gateway key") : p.has_key ? status("good", "Key saved") : status("idle", "No key")}
    </div>
    <div class="sub-line provider-tools"><span class="mono">${(p.tools || []).map(esc).join(", ")}</span> · ${num(p.today)}${budget} calls today</div>
    <label class="field"><span>${p.shared ? "Use a different key" : p.has_key ? "Replace key" : "API key"}${p.hint ? ` <span class="muted">(${esc(p.hint)})</span>` : ""}</span>
      <span class="inline"><input class="input mono" type="password" name="key" value="${esc(typed)}" autocomplete="new-password" spellcheck="false" placeholder="${p.shared ? "Optional" : p.has_key ? "••••••••  saved" : esc(p.hint || "Paste the key")}" aria-label="${esc(p.title)} API key">
      <button class="btn primary" type="submit">Save</button></span>
    </label>
    <div class="provider-foot">
      <label class="field small"><span>Daily limit</span><input class="input num" type="number" min="0" step="1" name="daily" value="${p.budget || ""}" placeholder="none" aria-label="${esc(p.title)} calls a day"></label>
      <span class="spacer"></span>
      ${p.has_key && p.test ? `<button class="btn" type="button" data-act="test">Test</button>` : ""}
      ${p.shared ? "" : p.has_key ? `<button class="btn danger" type="button" data-act="remove">Remove</button>` : `<a class="link fine" href="${esc(p.signup)}" target="_blank" rel="noopener noreferrer">Get a key ↗</a>`}
    </div>
    <div class="provider-result fine hidden" role="status"></div>
  </form>`;
}

function renderToolsKeys() {
  // A key typed into one card survives a save on another.
  const typed = {};
  for (const f of document.querySelectorAll("#tools-keys form")) typed[f.dataset.provider] = f.elements.key.value;
  const all = tools.providers || [];
  $("#tools-keys").innerHTML = all.filter((p) => p.signup).map((p) => providerCard(p, typed[p.name] || "")).join("");
  const free = all.filter((p) => !p.signup);
  $("#tools-keyless").innerHTML = free.length ? "No key needed: " + free.map((p) => `<strong>${esc(p.title)}</strong> (${(p.tools || []).map(esc).join(", ")})${p.note ? ", " + esc(p.note) : ""}`).join("; ") + "." : "";
}

function renderToolsList() {
  const list = tools.tools || [];
  $("#tools-count").textContent = list.length;
  $("#tools-list").innerHTML = `<table><thead><tr><th>Tool</th><th>What it does</th><th>Providers</th><th>Status</th></tr></thead><tbody>
    ${list.map((t) => {
      const order = t.order || [];
      const chips = order.map((n, i) => {
        const ready = (t.providers || []).includes(n);
        const label = `${order.length > 1 ? `<span class="rank">${i + 1}</span>` : ""}${esc(n)}`;
        if (i === 0 || order.length < 2) return `<span class="chip ${ready ? "" : "off"}">${label}</span>`;
        return `<button class="chip ${ready ? "" : "off"}" type="button" data-act="first" data-tool="${esc(t.name)}" data-to="${esc(n)}" title="Try ${esc(n)} first">${label}</button>`;
      }).join("");
      return `<tr><td class="mono">${esc(t.name)}</td><td class="ink2">${esc(t.summary)}${t.costly ? ` <span class="pill" title="Apps get this only if you tick it for them">costly</span>` : ""}</td>
        <td><span class="chips">${chips}</span>${t.custom ? ` <button class="linkbtn fine" type="button" data-act="reset" data-tool="${esc(t.name)}">Reset</button>` : ""}</td>
        <td>${(t.providers || []).length ? status("good", "Ready via " + t.providers[0]) : status("idle", "Needs a key")}</td></tr>`;
    }).join("")}
  </tbody></table>`;
}

// allows mirrors tools.Allowed: no list means everything but costly tools.
const allows = (patterns, t) => patterns?.length ? patterns.some((p) => p === "*" || p === t.name || (p.endsWith(".*") && t.name.startsWith(p.slice(0, -1)))) : !t.costly;
const toolGroups = () => {
  const groups = {};
  for (const t of tools.tools || []) (groups[t.name.split(".")[0]] ||= []).push(t);
  return groups;
};

// toolPatterns turns the ticked boxes into the shortest allow list: nothing
// for all but costly tools, * for all of them, and group.* for a whole
// group, so tools added later are included.
function toolPatterns(form) {
  const ticked = [...form.querySelectorAll("input[name=tool]:checked")].map((i) => i.value);
  const all = tools.tools || [];
  if (ticked.length === all.length) return ["*"];
  if (all.every((t) => ticked.includes(t.name) === !t.costly)) return [];
  return Object.entries(toolGroups()).flatMap(([g, list]) => list.every((t) => ticked.includes(t.name)) ? [g + ".*"] : list.filter((t) => ticked.includes(t.name)).map((t) => t.name));
}

function appEditor(a) {
  const groups = Object.entries(toolGroups()).map(([g, list]) => `<fieldset class="tool-group"><legend>${esc(g)}</legend>
    ${list.map((t) => `<label class="check"><input type="checkbox" name="tool" value="${esc(t.name)}" ${allows(a.tools, t) ? "checked" : ""}><span class="mono">${esc(t.name.split(".")[1])}</span>${t.costly ? ` <span class="pill">costly</span>` : ""}</label>`).join("")}</fieldset>`).join("");
  return `<tr class="editor"><td colspan="4"><form data-app="${esc(a.app)}" autocomplete="off">
    <div class="tool-groups">${groups}</div>
    <div class="provider-foot">
      <label class="field small"><span>Calls a month</span><input class="input num" type="number" min="0" name="monthly" value="${a.monthly_calls || ""}" placeholder="no cap"></label>
      <label class="field small"><span>Calls a minute</span><input class="input num" type="number" min="0" name="rpm" value="${a.rpm || ""}" placeholder="no cap"></label>
      ${a.dev ? `<label class="check"><input type="checkbox" name="full" ${a.keyless_only ? "" : "checked"}><span>Let this dev version spend your keys</span></label>` : ""}
      <span class="spacer"></span>
      <button class="btn" type="button" data-act="cancel">Cancel</button>
      <button class="btn primary" type="submit">Save</button>
    </div>
  </form></td></tr>`;
}

function renderToolsApps() {
  const apps = tools.apps || [], others = tools.others || [];
  const rows = apps.map((a) => {
    const total = (tools.tools || []).length;
    const count = (tools.tools || []).filter((t) => allows(a.tools, t)).length;
    const f = a.monthly_calls ? a.used / a.monthly_calls : 0;
    return `<tr><td><a class="app-name" href="#/apps/${encodeURIComponent(a.app)}">${esc(a.app)}</a></td>
      <td>${status("good", "On")} <span class="muted fine">${`${count} of ${total} tools`}${a.keyless_only ? " · keyless only" : ""}</span></td>
      ${a.monthly_calls ? `<td class="cell-meter">${num(a.used)} of ${num(a.monthly_calls)}<div class="meter ${f > 0.85 ? "hot" : ""}"><span style="width:${Math.min(100, f * 100).toFixed(0)}%"></span></div></td>` : `<td class="num">${num(a.used)} <span class="muted">calls this month</span></td>`}
      <td class="r"><button class="btn" type="button" data-act="edit" data-app="${esc(a.app)}">Edit</button> <button class="btn danger" type="button" data-act="disable" data-app="${esc(a.app)}">Turn off</button></td></tr>
      ${toolsEditing === a.app ? appEditor(a) : ""}`;
  }).concat(others.map((name) => `<tr><td><a class="app-name" href="#/apps/${encodeURIComponent(name)}">${esc(name)}</a></td><td>${status("idle", "Off")}</td><td></td>
      <td class="r"><button class="btn" type="button" data-act="enable" data-app="${esc(name)}">Turn on</button></td></tr>`));
  $("#tools-apps").innerHTML = `<table><thead><tr><th>App</th><th>Access</th><th>Usage</th><th></th></tr></thead><tbody>
    ${rows.join("") || `<tr><td colspan="4" class="empty">No apps yet.</td></tr>`}</tbody></table>`;
}

function toolsNotice(msg, bad) {
  const n = $("#tools-notice");
  if (!n) return;
  n.textContent = msg;
  n.classList.toggle("bad", !!bad);
  n.classList.remove("hidden");
}

// toolsDo runs one change with its button disabled, then shows the result.
async function toolsDo(button, fn, ok) {
  if (button) button.disabled = true;
  try {
    tools = await fn();
    renderTools();
    if (ok) toolsNotice(ok);
    else $("#tools-notice")?.classList.add("hidden");
  } catch (e) {
    if (e.message === "signed-out") return showError(e);
    toolsNotice(e.message, true);
  } finally {
    if (button) button.disabled = false;
  }
}

async function toolsClick(e) {
  const b = e.target.closest("[data-act]");
  if (!b) return;
  const act = b.dataset.act, app = b.dataset.app, form = b.closest("form");
  const provider = form?.dataset.provider;
  if (act === "edit") { toolsEditing = toolsEditing === app ? null : app; renderToolsApps(); }
  else if (act === "cancel") { toolsEditing = null; renderToolsApps(); }
  else if (act === "enable") toolsDo(b, () => post("/api/tools/enable", { app }), `${app} can use tools. It's redeploying to pick up its token.`);
  else if (act === "disable") {
    if (confirm(`Turn tools off for ${app}? Its token stops working now, and it redeploys.`)) toolsDo(b, () => post("/api/tools/disable", { app }), `Tools are off for ${app}.`);
  } else if (act === "first") {
    const t = tools.tools.find((x) => x.name === b.dataset.tool);
    toolsDo(b, () => post("/api/tools/use", { tool: t.name, providers: [b.dataset.to, ...t.order.filter((n) => n !== b.dataset.to)] }));
  } else if (act === "reset") toolsDo(b, () => post("/api/tools/use", { tool: b.dataset.tool }));
  else if (act === "remove") {
    if (confirm(`Remove the ${provider} key? Tools that rely on it stop working for every app.`)) toolsDo(b, () => post("/api/tools/key", { provider, remove: true }), `Removed the ${provider} key.`);
  } else if (act === "test") {
    const out = form.querySelector(".provider-result");
    b.disabled = true;
    out.className = "provider-result fine";
    out.textContent = "Testing…";
    try {
      const r = await post("/api/tools/test", { provider });
      out.innerHTML = status("good", `Works: ${r.tool} answered in ${ms(r.ms)}`);
    } catch (err) {
      out.innerHTML = status("critical", err.message === "signed-out" ? "Signed out" : err.message);
    } finally {
      b.disabled = false;
      api("/api/tools").then((t) => { tools = t; renderToolsTiles(); }).catch(() => {});
    }
  }
}

function toolsSubmit(e) {
  e.preventDefault();
  const form = e.target, button = form.querySelector("button[type=submit]");
  if (form.dataset.provider) {
    const provider = form.dataset.provider, key = form.elements.key.value.trim();
    const p = tools.providers.find((x) => x.name === provider);
    const daily = Number(form.elements.daily.value || 0);
    const body = { provider };
    if (key) body.key = key;
    if (daily !== (p.budget || 0)) body.daily = daily;
    if (!key && !("daily" in body)) return toolsNotice(`Paste a key for ${p.title} first.`, true);
    toolsDo(button, async () => {
      const t = await post("/api/tools/key", body);
      form.elements.key.value = ""; // gone from the page as soon as it's saved
      return t;
    }, key ? `Saved the ${p.title} key. Use Test to check it.` : `Updated the daily limit for ${p.title}.`);
  } else if (form.dataset.app) {
    const app = form.dataset.app, f = form.elements;
    const patterns = toolPatterns(form);
    if (!patterns.length && !form.querySelector("input[name=tool]:checked")) return toolsNotice(`Tick at least one tool, or turn tools off for ${app}.`, true);
    const body = { app, tools: patterns, monthly_calls: Number(f.monthly.value || 0), rpm: Number(f.rpm.value || 0), full: !!f.full?.checked };
    toolsDo(button, async () => { const t = await post("/api/tools/enable", body); toolsEditing = null; return t; }, `Updated ${app}.`);
  }
}

route();
