// Small SVG charts for the admin page: stacked bars, lines, sparklines.
// Marks follow the data-viz spec: bars <= 24px with 4px rounded data-ends
// and 2px surface gaps between stacked segments, 2px lines, hairline
// gridlines, one shared tooltip (built with textContent: paths and app
// names are untrusted).

const SVG = "http://www.w3.org/2000/svg";

function el(tag, attrs = {}, parent) {
  const n = document.createElementNS(SVG, tag);
  for (const [k, v] of Object.entries(attrs)) n.setAttribute(k, v);
  if (parent) parent.appendChild(n);
  return n;
}

// niceMax rounds a maximum up to 1, 2, 2.5 or 5 × 10^n.
function niceMax(v) {
  if (v <= 0) return 1;
  const p = Math.pow(10, Math.floor(Math.log10(v)));
  for (const m of [1, 2, 2.5, 5, 10]) if (m * p >= v) return m * p;
  return 10 * p;
}

const tip = () => document.getElementById("tooltip");

export function hideTip() {
  tip().classList.add("hidden");
}

// showTip renders rows of {label, value, color, kind: "line"|"box"} under
// a heading, next to the pointer.
function showTip(evt, heading, rows) {
  const t = tip();
  t.replaceChildren();
  const h = document.createElement("div");
  h.className = "when";
  h.textContent = heading;
  t.appendChild(h);
  for (const r of rows) {
    const row = document.createElement("div");
    row.className = "row";
    const key = document.createElement("span");
    key.className = "key";
    const sw = document.createElement("i");
    sw.style.background = r.color;
    key.append(sw, document.createTextNode(r.label));
    const val = document.createElement("b");
    val.textContent = r.value;
    row.append(key, val);
    t.appendChild(row);
  }
  t.classList.remove("hidden");
  const pad = 14;
  const w = t.offsetWidth, hgt = t.offsetHeight;
  let x = evt.clientX + pad, y = evt.clientY + pad;
  if (x + w > window.innerWidth - 8) x = evt.clientX - w - pad;
  if (y + hgt > window.innerHeight - 8) y = evt.clientY - hgt - pad;
  t.style.left = Math.max(8, x) + "px";
  t.style.top = Math.max(8, y) + "px";
}

function frame(container, { yTicks, fmtY, xLabels }) {
  container.querySelector("svg")?.remove();
  const W = container.clientWidth, H = container.clientHeight;
  const m = { l: 48, r: 10, t: 10, b: 24 };
  const svg = el("svg", { viewBox: `0 0 ${W} ${H}`, role: "img" });
  const iw = Math.max(10, W - m.l - m.r), ih = Math.max(10, H - m.t - m.b);
  const g = el("g", {}, svg);
  for (const tv of yTicks) {
    const y = m.t + ih - (tv.frac * ih);
    el("line", { x1: m.l, x2: m.l + iw, y1: y, y2: y, class: tv.frac === 0 ? "axis" : "gridline" }, g);
    const tx = el("text", { x: m.l - 8, y: y + 4, "text-anchor": "end", class: "tick" }, g);
    tx.textContent = fmtY(tv.value);
  }
  for (const xl of xLabels) {
    const tx = el("text", { x: xl.x, y: H - 6, "text-anchor": xl.anchor || "middle", class: "tick" }, g);
    tx.textContent = xl.text;
  }
  container.appendChild(svg);
  return { svg, W, H, m, iw, ih };
}

// scale picks a round step (1, 2, 2.5 or 5 × 10^n) so every tick is a
// round number, and the top tick at or above the data.
function scale(v, n = 4) {
  const step = niceMax(Math.max(v, 1e-9) / n);
  const max = step * Math.max(1, Math.ceil(v / step));
  const ticks = [];
  for (let x = 0; x <= max + step / 2; x += step) ticks.push({ value: x, frac: x / max });
  return { max, ticks };
}

function xLabelsFor(points, x, fmtX, iw, m) {
  const want = Math.max(2, Math.min(7, Math.floor(iw / 90)));
  const every = Math.max(1, Math.ceil(points.length / want));
  const out = [];
  for (let i = 0; i < points.length; i += every) out.push({ x: x(i), text: fmtX(points[i].t) });
  return out;
}

// Rounded top corners, square at the baseline.
function topRounded(x, y, w, h, r) {
  r = Math.min(r, w / 2, h);
  return `M${x},${y + h}V${y + r}Q${x},${y} ${x + r},${y}H${x + w - r}Q${x + w},${y} ${x + w},${y + r}V${y + h}Z`;
}

function noData(container, msg) {
  let n = container.querySelector(".empty-chart");
  if (!msg) return n?.remove();
  if (!n) {
    n = document.createElement("div");
    n.className = "empty-chart";
    container.appendChild(n);
  }
  n.textContent = msg;
}

// bars draws stacked columns. series: [{key, label, cls, color}], bottom first.
export function bars(container, { points, series, fmtX, fmtWhen, fmtY, empty = "No requests in this period" }) {
  const totals = points.map((p) => series.reduce((s, se) => s + (p[se.key] || 0), 0));
  const { max, ticks } = scale(Math.max(...totals, 0));
  const n = points.length;
  let f = frame(container, { yTicks: ticks, fmtY, xLabels: [] });
  const band = f.iw / Math.max(1, n);
  const bw = Math.max(1, Math.min(24, band * 0.72));
  const x = (i) => f.m.l + i * band + band / 2;
  f = frame(container, { yTicks: ticks, fmtY, xLabels: xLabelsFor(points, x, fmtX, f.iw, f.m) });
  const { svg, m, ih } = f;
  const hl = el("rect", { class: "band-hl hidden", y: m.t, height: ih, width: band, rx: 4 }, svg);
  const marks = el("g", {}, svg);
  const base = m.t + ih;
  points.forEach((p, i) => {
    let cursor = base;
    const segs = series.map((se) => ({ se, v: p[se.key] || 0 })).filter((s) => s.v > 0);
    segs.forEach((s, j) => {
      const h = (s.v / max) * ih;
      const gap = j > 0 ? 2 : 0;
      const top = cursor - h;
      const hh = Math.max(0, h - gap);
      if (hh <= 0) return;
      const isTop = j === segs.length - 1;
      const x0 = x(i) - bw / 2;
      if (isTop && hh > 1) el("path", { d: topRounded(x0, top, bw, hh, 4), class: s.se.cls }, marks);
      else el("rect", { x: x0, y: top, width: bw, height: hh, class: s.se.cls }, marks);
      cursor = top;
    });
  });
  noData(container, totals.some((t) => t > 0) ? "" : empty);
  svg.addEventListener("pointermove", (e) => {
    const r = svg.getBoundingClientRect();
    const i = Math.floor((e.clientX - r.left - m.l) / band);
    if (i < 0 || i >= n) return leave();
    hl.setAttribute("x", m.l + i * band);
    hl.classList.remove("hidden");
    const p = points[i];
    const rows = [...series].reverse().map((se) => ({ label: se.label, value: fmtY(p[se.key] || 0), color: se.color }));
    if (series.length > 1) rows.push({ label: "Total", value: fmtY(totals[i]), color: "transparent" });
    showTip(e, fmtWhen(p.t), rows);
  });
  const leave = () => { hl.classList.add("hidden"); hideTip(); };
  svg.addEventListener("pointerleave", leave);
}

// lines draws one line per series, with gaps where a point has no value
// (or a zero, unless zeroIsGap is false). yMax fixes the top of the scale.
export function lines(container, { points, series, fmtX, fmtWhen, fmtY, empty = "No requests in this period", zeroIsGap = true, yMax }) {
  const has = (p, k) => p?.[k] != null && (!zeroIsGap || p[k] !== 0);
  const vals = points.flatMap((p) => series.map((se) => p[se.key] || 0));
  const { max, ticks } = scale(yMax ?? Math.max(...vals, 0));
  const n = points.length;
  const pre = frame(container, { yTicks: ticks, fmtY, xLabels: [] });
  const step = pre.iw / Math.max(1, n - 1);
  const x = (i) => pre.m.l + i * step;
  const f = frame(container, { yTicks: ticks, fmtY, xLabels: xLabelsFor(points, x, fmtX, pre.iw, pre.m) });
  const { svg, m, ih } = f;
  const y = (v) => m.t + ih - (v / max) * ih;
  for (const se of series) {
    let d = "", pen = false;
    points.forEach((p, i) => {
      const v = p[se.key];
      if (!has(p, se.key)) { pen = false; return; }
      d += (pen ? "L" : "M") + x(i).toFixed(1) + "," + y(v).toFixed(1);
      pen = true;
    });
    el("path", { d, class: "line " + se.cls }, svg);
    // Isolated points (a gap on both sides) need a dot to be seen.
    points.forEach((p, i) => {
      if (has(p, se.key) && !has(points[i - 1], se.key) && !has(points[i + 1], se.key)) el("circle", { cx: x(i), cy: y(p[se.key]), r: 3, class: "dot " + se.cls }, svg);
    });
  }
  const cross = el("line", { y1: m.t, y2: m.t + ih, class: "crosshair hidden" }, svg);
  const dots = series.map((se) => el("circle", { r: 4, class: "dot ring hidden " + se.cls }, svg));
  noData(container, points.some((p) => series.some((se) => has(p, se.key))) ? "" : empty);
  svg.addEventListener("pointermove", (e) => {
    const r = svg.getBoundingClientRect();
    const i = Math.round((e.clientX - r.left - m.l) / step);
    if (i < 0 || i >= n) return leave();
    cross.setAttribute("x1", x(i));
    cross.setAttribute("x2", x(i));
    cross.classList.remove("hidden");
    series.forEach((se, k) => {
      const v = points[i][se.key];
      if (has(points[i], se.key)) {
        dots[k].setAttribute("cx", x(i));
        dots[k].setAttribute("cy", y(v));
        dots[k].classList.remove("hidden");
      } else dots[k].classList.add("hidden");
    });
    showTip(e, fmtWhen(points[i].t), series.map((se) => ({ label: se.label, value: has(points[i], se.key) ? fmtY(points[i][se.key]) : "–", color: se.color })));
  });
  const leave = () => { cross.classList.add("hidden"); dots.forEach((d) => d.classList.add("hidden")); hideTip(); };
  svg.addEventListener("pointerleave", leave);
}

// spark returns a tiny line with a wash under it, for table rows.
export function spark(values, cls = "s1") {
  const W = 120, H = 28;
  const svg = el("svg", { viewBox: `0 0 ${W} ${H}`, class: "spark", "aria-hidden": "true", preserveAspectRatio: "none" });
  const max = Math.max(...values, 1);
  const n = values.length;
  if (n < 2) return svg;
  const pts = values.map((v, i) => [(i / (n - 1)) * (W - 4) + 2, H - 3 - (v / max) * (H - 6)]);
  const line = pts.map((p, i) => (i ? "L" : "M") + p[0].toFixed(1) + "," + p[1].toFixed(1)).join("");
  el("path", { d: line + `L${pts[n - 1][0]},${H}L${pts[0][0]},${H}Z`, class: "wash " + cls }, svg);
  el("path", { d: line, class: "line thin " + cls }, svg);
  const last = pts[n - 1];
  el("circle", { cx: last[0], cy: last[1], r: 2.5, class: "dot " + cls }, svg);
  return svg;
}
