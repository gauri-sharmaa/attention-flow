// Attention Flow dashboard. Renders engine snapshots; no framework, no build.
// Data comes from the live server (/api/meta + /api/stream) or, for a static
// demo, from frames.json next to this file.
(() => {
  const $ = (id) => document.getElementById(id);
  const css = (v) => getComputedStyle(document.documentElement).getPropertyValue(v).trim();
  let meta = null;
  let colors = [];

  const sector = (c) => { const s = meta.clusters[c]; return s.length <= 2 ? s.toUpperCase() : s[0].toUpperCase() + s.slice(1); };
  const name = (e) => (e < 0 ? "sector" : meta.names[e]);
  const color = (e) => colors[meta.cluster[e]] || css("--muted");
  const sign = (x, d = 1) => (x > 0 ? "+" : x < 0 ? "−" : "") + Math.abs(x).toFixed(d);
  const cls = (x) => (x > 0 ? "up" : x < 0 ? "down" : "");
  const dur = (bars) => {
    const s = bars * meta.barSec;
    if (s < 3600) return Math.round(s / 60) + "m";
    if (s < 86400) return Math.round(s / 3600) + "h";
    return Math.round(s / 86400) + "d";
  };
  const esc = (s) => s.replace(/[&<>"]/g, (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;" }[c]));

  function spark(d) {
    const W = 150, H = 30, all = d.past.concat(d.fc, d.fd);
    let lo = Math.min(...all), hi = Math.max(...all);
    if (hi - lo < 1) { const m = (hi + lo) / 2; lo = m - 0.5; hi = m + 0.5; }
    const n = d.past.length + d.fc.length - 1;
    const x = (i) => (i / n) * W, y = (v) => H - 2 - ((v - lo) / (hi - lo)) * (H - 4);
    const line = (arr, off) => arr.map((v, i) => `${x(i + off).toFixed(1)},${y(v).toFixed(1)}`).join(" ");
    const now = d.past.length - 1;
    const up = d.gap >= 0 ? css("--up") : css("--down");
    return `<svg class="spark" viewBox="0 0 ${W} ${H}" preserveAspectRatio="none">
      <line x1="${x(now)}" x2="${x(now)}" y1="0" y2="${H}" stroke="${css("--faint")}" />
      <polyline fill="none" stroke="${css("--muted")}" stroke-width="1.3" points="${line(d.past, 0)}" />
      <polyline fill="none" stroke="${css("--muted")}" stroke-width="1" stroke-dasharray="1 2" points="${line(d.fd, now)}" />
      <polyline fill="none" stroke="${up}" stroke-width="1.6" stroke-dasharray="3 2" points="${line(d.fc, now)}" />
    </svg>`;
  }

  function renderDisl(list) {
    $("disl").innerHTML = list.length
      ? list.map((d) => {
          const why = (d.drivers || []).map((r) => `<b>${esc(name(r.e))}</b> ${sign(r.pct)}`).join(" · ");
          return `<li><div class="row">
            <span class="name"><span class="dot" style="background:${color(d.e)}"></span>${esc(name(d.e))}</span>
            ${spark(d)}
            <span class="num ${cls(d.gap)}">${sign(d.gap)}%</span>
            <span class="num">${Math.round(d.prob * 100)}%</span>
            <span class="num muted">${dur(d.catch)}</span>
          </div>${why ? `<div class="why">${why}</div>` : ""}</li>`;
        }).join("")
      : `<li class="empty">Learning… the first signals appear after warm-up.</li>`;
  }

  function renderShocks(list) {
    $("shocks").innerHTML = list.length
      ? list.slice(0, 5).map((s) => {
          const kids = (s.children || []).map((c) => {
            const frac = c.final ? Math.max(0, Math.min(1, c.realized / c.final)) : 0;
            return `<div class="kid">
              <span class="name">${"·".repeat(c.depth - 1)}${esc(name(c.e))}</span>
              <span class="bar"><i style="width:${(frac * 100).toFixed(0)}%"></i></span>
              <span class="num"><span class="${cls(c.realized)}">${sign(c.realized)}</span><span class="muted"> / ${sign(c.final)}</span></span>
              <span class="num muted">${c.tested ? Math.round(c.prob * 100) + "%" : "new"}</span>
            </div>`;
          }).join("");
          const via = s.echoOf >= 0 ? `<span class="muted">via ${esc(name(s.echoOf))}</span>` : "";
          return `<div class="shock"><div class="head">
              <span class="dot" style="background:${color(s.e)}"></span><span class="name">${esc(name(s.e))}</span>
              <span class="pill ${cls(s.size)}">${sign(s.size, 0)}%</span>
              <span class="pill">z ${s.z.toFixed(0)}</span>
              ${via}<span class="spacer"></span><span class="muted">${s.age ? dur(s.age) + " ago" : "now"}</span>
            </div>${kids || `<div class="kid muted">no learned followers</div>`}</div>`;
        }).join("")
      : `<div class="empty">Quiet. Bursts show up here with where they're expected to spread.</div>`;
  }

  function renderFactors(list) {
    $("factors").innerHTML = list.map((f) => `<div class="factor">
        <div class="head"><span><span class="dot" style="background:${colors[f.c]}"></span>${esc(sector(f.c))}</span>
        <span class="num ${cls(f.move)}">${sign(f.move)}%</span></div>
        ${(f.off || []).slice(0, 3).map((o) => `<div class="off"><span class="name" style="font-weight:400">${esc(name(o.e))}</span>
          <span class="ea">${sign(o.exp)} → <b class="${cls(o.act - o.exp)}">${sign(o.act)}</b></span></div>`).join("")}
      </div>`).join("");
  }

  // ---- graph: tiny force layout, persistent positions across frames ----
  const canvas = $("graph"), ctx = canvas.getContext("2d");
  const nodes = new Map(); // id -> {x,y,vx,vy}
  let edges = [], hot = new Set(), shockSrc = new Set(), hover = -1;

  // Fruchterman-Reingold with cooling; reheats when the node set changes.
  let temp = 30;
  function layoutStep() {
    const W = canvas.clientWidth, H = canvas.clientHeight;
    const arr = [...nodes.entries()];
    if (!arr.length) return;
    const k = Math.sqrt((W * H) / arr.length) * 0.38, k2 = k * k;
    for (const [, a] of arr) { a.fx = 0; a.fy = 0; }
    for (let i = 0; i < arr.length; i++) {
      const a = arr[i][1];
      for (let j = i + 1; j < arr.length; j++) {
        const b = arr[j][1];
        const dx = a.x - b.x, dy = a.y - b.y, d2 = Math.max(dx * dx + dy * dy, 1);
        const f = k2 / d2; // = (k²/d) / d, applied to the unnormalised delta
        a.fx += dx * f; a.fy += dy * f; b.fx -= dx * f; b.fy -= dy * f;
      }
    }
    for (const e of edges) {
      const a = nodes.get(e.from), b = nodes.get(e.to);
      if (!a || !b) continue;
      const dx = b.x - a.x, dy = b.y - a.y, d = Math.max(Math.hypot(dx, dy), 1);
      const f = d / k; // = (d²/k) / d
      a.fx += dx * f; a.fy += dy * f; b.fx -= dx * f; b.fy -= dy * f;
    }
    const nc = meta.clusters.length;
    for (const [id, a] of arr) {
      // Gentle pull toward a per-sector anchor keeps sectors readable.
      const ang = (meta.cluster[id] / nc) * 2 * Math.PI - Math.PI / 2;
      const cx = W / 2 + Math.cos(ang) * W * 0.25, cy = H / 2 + Math.sin(ang) * H * 0.22;
      a.fx += (cx - a.x) * 0.9; a.fy += (cy - a.y) * 0.9;
      const m = Math.hypot(a.fx, a.fy) || 1, step = Math.min(m, temp);
      a.x = Math.max(14, Math.min(W - 90, a.x + (a.fx / m) * step));
      a.y = Math.max(14, Math.min(H - 14, a.y + (a.fy / m) * step));
    }
    temp = Math.max(0.6, temp * 0.97);
  }

  function draw() {
    const dpr = window.devicePixelRatio || 1, W = canvas.clientWidth, H = canvas.clientHeight;
    if (canvas.width !== W * dpr) { canvas.width = W * dpr; canvas.height = H * dpr; }
    ctx.setTransform(dpr, 0, 0, dpr, 0, 0);
    ctx.clearRect(0, 0, W, H);
    if (!meta) return requestAnimationFrame(draw);
    layoutStep();
    const faint = css("--faint"), muted = css("--muted"), ink = css("--ink");
    for (const e of edges) {
      const a = nodes.get(e.from), b = nodes.get(e.to);
      if (!a || !b) continue;
      const lit = hover === e.from || hover === e.to || shockSrc.has(e.from);
      ctx.strokeStyle = lit ? color(e.from) : faint;
      ctx.lineWidth = lit ? 1.6 : 1;
      const dx = b.x - a.x, dy = b.y - a.y, d = Math.hypot(dx, dy) || 1;
      const ex = b.x - (dx / d) * 6, ey = b.y - (dy / d) * 6;
      ctx.beginPath(); ctx.moveTo(a.x, a.y); ctx.lineTo(ex, ey); ctx.stroke();
      ctx.beginPath();
      ctx.moveTo(ex, ey);
      ctx.lineTo(ex - (dx / d) * 5 - (dy / d) * 3, ey - (dy / d) * 5 + (dx / d) * 3);
      ctx.lineTo(ex - (dx / d) * 5 + (dy / d) * 3, ey - (dy / d) * 5 - (dx / d) * 3);
      ctx.fillStyle = ctx.strokeStyle; ctx.fill();
    }
    const t = performance.now() / 400;
    for (const [id, a] of nodes) {
      const r = hot.has(id) || shockSrc.has(id) ? 5 : 3.5;
      if (shockSrc.has(id)) {
        ctx.beginPath(); ctx.arc(a.x, a.y, r + 4 + 2 * Math.sin(t), 0, 7);
        ctx.strokeStyle = color(id); ctx.lineWidth = 1; ctx.stroke();
      }
      ctx.beginPath(); ctx.arc(a.x, a.y, r, 0, 7); ctx.fillStyle = color(id); ctx.fill();
      if (hot.has(id) || shockSrc.has(id) || id === hover) {
        ctx.fillStyle = id === hover ? ink : muted;
        ctx.font = "11px -apple-system, sans-serif";
        ctx.fillText(meta.names[id], a.x + 7, a.y + 4);
      }
    }
    requestAnimationFrame(draw);
  }

  canvas.addEventListener("mousemove", (ev) => {
    const r = canvas.getBoundingClientRect(), x = ev.clientX - r.left, y = ev.clientY - r.top;
    hover = -1;
    let best = 100;
    for (const [id, a] of nodes) {
      const d = (a.x - x) ** 2 + (a.y - y) ** 2;
      if (d < best) { best = d; hover = id; }
    }
    const tip = $("tip");
    if (hover >= 0) {
      const outs = edges.filter((e) => e.from === hover).map((e) => `${meta.names[e.to]} (${dur(e.lag)})`);
      tip.textContent = meta.names[hover] + (outs.length ? " → " + outs.slice(0, 3).join(", ") : "");
      tip.style.left = x + 26 + "px"; tip.style.top = y + 30 + "px"; tip.style.opacity = 1;
    } else tip.style.opacity = 0;
  });
  canvas.addEventListener("mouseleave", () => { hover = -1; $("tip").style.opacity = 0; });

  function renderGraph(s) {
    edges = s.edges;
    const keep = new Set();
    for (const e of edges) { keep.add(e.from); keep.add(e.to); }
    const W = canvas.clientWidth || 600, H = canvas.clientHeight || 340;
    let changed = false;
    for (const id of keep) if (!nodes.has(id)) {
      // New nodes start near a neighbour if one is placed, else near their sector.
      const nb = edges.find((e) => (e.from === id && nodes.has(e.to)) || (e.to === id && nodes.has(e.from)));
      const p = nb ? nodes.get(nb.from === id ? nb.to : nb.from) : { x: W / 2, y: H / 2 };
      nodes.set(id, { x: p.x + (Math.random() - 0.5) * 30, y: p.y + (Math.random() - 0.5) * 30 });
      changed = true;
    }
    for (const id of [...nodes.keys()]) if (!keep.has(id)) { nodes.delete(id); changed = true; }
    if (changed) temp = Math.max(temp, 8);
    hot = new Set(s.disl.slice(0, 5).map((d) => d.e));
    shockSrc = new Set(s.shocks.filter((x) => x.age < meta.horizon).map((x) => x.e));
    $("graph-stats").textContent = `${edges.length} links · ${keep.size} topics`;
  }

  function render(s) {
    $("clock").textContent = new Date(s.time * 1000).toLocaleString([], { month: "short", day: "numeric", hour: "2-digit", minute: "2-digit" });
    renderDisl(s.disl || []);
    renderShocks(s.shocks || []);
    renderFactors(s.factors || []);
    renderGraph({ edges: s.edges || [], disl: s.disl || [], shocks: s.shocks || [] });
  }

  async function start() {
    colors = [css("--ai"), css("--crypto"), css("--tech"), css("--accent")];
    let frames = null;
    try {
      const r = await fetch("api/meta");
      if (!r.ok) throw new Error();
      meta = await r.json();
    } catch {
      const r = await fetch("frames.json"); // static demo
      const data = await r.json();
      meta = data.meta;
      frames = data.frames;
    }
    $("label").textContent = meta.label;
    requestAnimationFrame(draw);

    let speed = 30, paused = false;
    if (frames) {
      let i = 0, acc = 0, last = performance.now();
      const tick = (now) => {
        acc += ((now - last) / 1000) * (paused ? 0 : speed / 6);
        last = now;
        while (acc >= 1) { acc -= 1; i = (i + 1) % frames.length; render(frames[i]); }
        requestAnimationFrame(tick);
      };
      render(frames[0]);
      requestAnimationFrame(tick);
    } else {
      const es = new EventSource("api/stream");
      es.onmessage = (m) => render(JSON.parse(m.data));
    }
    const control = (q) => { if (!frames) fetch("api/control?" + q); };
    $("speed").addEventListener("click", (ev) => {
      const b = ev.target.closest("button"); if (!b) return;
      speed = +b.dataset.v;
      for (const x of $("speed").children) x.classList.toggle("on", x === b);
      control("speed=" + speed);
    });
    $("pause").addEventListener("click", () => {
      paused = !paused;
      $("pause").textContent = paused ? "▶" : "❚❚";
      control("pause=" + (paused ? 1 : 0));
    });
  }
  start();
})();
