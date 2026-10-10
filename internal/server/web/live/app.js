// Live Polymarket dashboard: draws snapshots streamed from /api/live.
(() => {
  const $ = (id) => document.getElementById(id);
  const css = (v) => getComputedStyle(document.documentElement).getPropertyValue(v).trim();
  const esc = (s) => s.replace(/[&<>"]/g, (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;" }[c]));
  const sectors = ["world", "politics", "econ", "crypto", "tech", "sports", "culture", "other"];
  const color = (s) => css("--s-" + (sectors.includes(s) ? s : "other"));
  const ago = (t, now) => { const s = Math.max(0, now - t); return s < 60 ? s + "s" : s < 3600 ? Math.round(s / 60) + "m" : Math.round(s / 3600) + "h"; };
  const lag = (s) => s < 90 ? Math.round(s) + "s" : Math.round(s / 60) + " min";
  let S = null, lastTape = 0;

  function status(s) {
    const st = s.status;
    $("clock").textContent = new Date(s.now * 1000).toLocaleTimeString([], { hour: "2-digit", minute: "2-digit", second: "2-digit" });
    if (st.phase !== "live") {
      $("loading").hidden = false; $("main").hidden = true;
      $("loadText").textContent = st.phase === "loading"
        ? `Loading the last two days of trades for ${st.markets || "the busiest"} markets…`
        : `Fitting the model on ${st.trades.toLocaleString()} trades…`;
      $("loadBar").style.width = (st.phase === "loading" ? 90 * st.progress : 95) + "%";
      return false;
    }
    $("loading").hidden = true; $("main").hidden = false;
    $("status").textContent = `${st.markets} markets · ${st.liveTrades.toLocaleString()} live trades · model refit ${ago(st.fittedAt, s.now)} ago` + (st.error ? " · feed retrying" : "");
    return true;
  }

  function hot(s) {
    $("hotHint").textContent = `trading faster than normal for this hour · odds of a ${Math.round(s.horizon / 60)}-minute 2¢ price move`;
    $("hot").innerHTML = s.hot.length ? s.hot.map((h) => {
      const m = s.markets[h.m];
      const why = h.drivers.length ? "set off by " + h.drivers.map((d) => esc(s.markets[d].q)).join(" · ") : `${h.recent} trades in the last 5 min`;
      return `<li><div class="row">
        <span class="q"><span class="dot" style="background:${color(m.s)}"></span>${esc(m.q)}</span>
        <span class="num">${Math.round(m.p * 100)}¢</span>
        <span class="num">×${h.heat > 99 ? "99+" : h.heat.toFixed(1)}</span>
        <span class="num odds"><b>${Math.round(h.pmove * 100)}%</b> <span class="muted">vs ${Math.round(h.pnorm * 100)}%</span></span>
      </div><div class="why">${why}</div></li>`;
    }).join("") : `<li class="empty">Nothing unusual right now. Markets show up here when trading picks up faster than normal for the hour.</li>`;
  }

  function tape(s) {
    const rows = s.tape.slice().reverse().slice(0, 24);
    const newest = rows.length ? rows[0].t : 0;
    $("tape").innerHTML = rows.map((r) => {
      const m = s.markets[r.m];
      return `<li class="${r.t > lastTape ? "new" : ""}"><span class="muted">${ago(r.t, s.now)}</span>
        <span class="q"><span class="dot" style="background:${color(m.s)}"></span>${esc(m.q)}</span>
        <span class="num ${r.dir > 0 ? "up" : "down"}">${r.dir > 0 ? "▲" : "▼"} ${Math.round(r.p * 100)}¢</span>
        <span class="num muted">$${r.usd.toLocaleString()}</span></li>`;
    }).join("") || `<li class="empty">Waiting for the next trade. These markets trade every few seconds to minutes.</li>`;
    lastTape = Math.max(lastTape, newest);
  }

  // ---- network: main cluster with a force layout, small groups in a grid ----
  const canvas = $("net"), ctx = canvas.getContext("2d");
  let pos = new Map(), key = "", hover = -1;
  function layout(s) {
    const W = canvas.clientWidth, H = canvas.clientHeight;
    const ids = [...new Set(s.links.flatMap((l) => [l.f, l.t]))];
    const parent = new Map(ids.map((i) => [i, i]));
    const find = (x) => { while (parent.get(x) !== x) x = parent.get(x); return x; };
    for (const l of s.links) parent.set(find(l.f), find(l.t));
    const comps = new Map();
    for (const i of ids) { const r = find(i); if (!comps.has(r)) comps.set(r, []); comps.get(r).push(i); }
    const list = [...comps.values()].sort((a, b) => b.length - a.length);
    pos = new Map();
    if (!list.length) return;
    const main = list[0], rest = list.slice(1), narrow = W < 560;
    const ax = rest.length ? (narrow ? W : W * 0.56) : W, ah = rest.length && narrow ? H * 0.6 : H;
    const k = Math.sqrt((ax * ah) / main.length) * 0.55, k2 = k * k;
    const P = main.map((id, i) => ({ id, x: ax / 2 + Math.cos(i) * ax * 0.3, y: ah / 2 + Math.sin(i) * ah * 0.3 }));
    const at = new Map(P.map((p) => [p.id, p]));
    const ml = s.links.filter((l) => at.has(l.f) && at.has(l.t));
    let temp = ax / 8;
    for (let it = 0; it < 300; it++) {
      for (const p of P) { p.fx = 0; p.fy = 0; }
      for (let i = 0; i < P.length; i++) for (let j = i + 1; j < P.length; j++) {
        const a = P[i], b = P[j], dx = a.x - b.x, dy = a.y - b.y, d2 = Math.max(dx * dx + dy * dy, 1), f = k2 / d2;
        a.fx += dx * f; a.fy += dy * f; b.fx -= dx * f; b.fy -= dy * f;
      }
      for (const l of ml) {
        const a = at.get(l.f), b = at.get(l.t), dx = b.x - a.x, dy = b.y - a.y, d = Math.max(Math.hypot(dx, dy), 1), f = d / k;
        a.fx += dx * f; a.fy += dy * f; b.fx -= dx * f; b.fy -= dy * f;
      }
      for (const p of P) {
        p.fx += (ax / 2 - p.x) * 0.02; p.fy += (ah / 2 - p.y) * 0.02;
        const m = Math.hypot(p.fx, p.fy) || 1, st = Math.min(m, temp);
        p.x += (p.fx / m) * st; p.y += (p.fy / m) * st;
      }
      temp = Math.max(1, temp * 0.99);
    }
    // Scale the finished layout to fill its box instead of pinning nodes to the walls.
    const xs = P.map((p) => p.x), ys = P.map((p) => p.y), pad = 18;
    const x0 = Math.min(...xs), x1 = Math.max(...xs), y0 = Math.min(...ys), y1 = Math.max(...ys);
    let sx = (ax - 2 * pad) / Math.max(x1 - x0, 1), sy = (ah - 2 * pad) / Math.max(y1 - y0, 1);
    sx = Math.min(sx, sy * 2.5); sy = Math.min(sy, sx * 2.5); // stretch, but not into a line
    const ox = (ax - (x1 - x0) * sx) / 2, oy = (ah - (y1 - y0) * sy) / 2;
    for (const p of P) pos.set(p.id, { x: ox + (p.x - x0) * sx, y: oy + (p.y - y0) * sy });
    const bx0 = narrow ? 0 : ax + 10, by0 = narrow ? ah + 8 : 0, bw = W - bx0, bh = H - by0;
    const cols = Math.max(1, Math.round(Math.sqrt(rest.length * bw / Math.max(bh, 1)))), rows = Math.ceil(rest.length / cols);
    const cw = bw / cols, ch = bh / Math.max(rows, 1);
    rest.forEach((c, i) => {
      const cx = bx0 + (i % cols + 0.5) * cw, cy = by0 + (Math.floor(i / cols) + 0.5) * ch, r = Math.min(cw, ch) * 0.3;
      c.forEach((id, j) => { const a = (j / c.length) * 2 * Math.PI - Math.PI / 2; pos.set(id, { x: cx + Math.cos(a) * r, y: cy + Math.sin(a) * r }); });
    });
  }
  function drawNet() {
    if (!S) return requestAnimationFrame(drawNet);
    const dpr = window.devicePixelRatio || 1, W = canvas.clientWidth, H = canvas.clientHeight;
    if (canvas.width !== W * dpr) { canvas.width = W * dpr; canvas.height = H * dpr; key = ""; }
    ctx.setTransform(dpr, 0, 0, dpr, 0, 0);
    const k = S.links.map((l) => l.f + ">" + l.t).join(",") + W;
    if (k !== key) { layout(S); key = k; }
    ctx.clearRect(0, 0, W, H);
    const t = performance.now() / 500;
    for (const l of S.links) {
      const a = pos.get(l.f), b = pos.get(l.t); if (!a || !b) continue;
      const lit = hover === l.f || hover === l.t;
      ctx.strokeStyle = lit ? color(S.markets[l.f].s) : css("--faint");
      ctx.lineWidth = Math.min(4, 0.6 + l.b * 8);
      const dx = b.x - a.x, dy = b.y - a.y, d = Math.hypot(dx, dy) || 1, ex = b.x - (dx / d) * 6, ey = b.y - (dy / d) * 6;
      ctx.beginPath(); ctx.moveTo(a.x, a.y); ctx.lineTo(ex, ey); ctx.stroke();
      ctx.beginPath(); ctx.moveTo(ex, ey);
      ctx.lineTo(ex - (dx / d) * 6 - (dy / d) * 3.5, ey - (dy / d) * 6 + (dx / d) * 3.5);
      ctx.lineTo(ex - (dx / d) * 6 + (dy / d) * 3.5, ey - (dy / d) * 6 - (dx / d) * 3.5);
      ctx.fillStyle = ctx.strokeStyle; ctx.fill();
    }
    for (const [id, p] of pos) {
      const m = S.markets[id], busy = m.r > 0;
      if (busy) { ctx.beginPath(); ctx.arc(p.x, p.y, 7 + 2 * Math.sin(t + id) + Math.min(6, m.r), 0, 7); ctx.strokeStyle = color(m.s); ctx.globalAlpha = 0.45; ctx.lineWidth = 1; ctx.stroke(); ctx.globalAlpha = 1; }
      ctx.beginPath(); ctx.arc(p.x, p.y, id === hover ? 6 : 4.5, 0, 7); ctx.fillStyle = color(m.s); ctx.fill();
    }
    requestAnimationFrame(drawNet);
  }
  const pick = (ev) => {
    if (!S) return;
    const r = canvas.getBoundingClientRect(), x = ev.clientX - r.left, y = ev.clientY - r.top;
    let best = -1, bd = 144;
    for (const [id, p] of pos) { const d = (p.x - x) ** 2 + (p.y - y) ** 2; if (d < bd) { bd = d; best = id; } }
    hover = best;
    if (best < 0) { $("tip").textContent = "Hover or tap a market."; return; }
    const outs = S.links.filter((l) => l.f === best).sort((a, b) => b.b - a.b).slice(0, 3);
    $("tip").innerHTML = `<b>${esc(S.markets[best].q)}</b> · ${Math.round(S.markets[best].p * 100)}¢` +
      (outs.length ? "<br>sets off " + outs.map((l) => `${esc(S.markets[l.t].q)} (${lag(l.lag)})`).join(" · ") : "");
  };
  canvas.addEventListener("mousemove", pick);
  canvas.addEventListener("click", pick); // taps on phones
  canvas.addEventListener("mouseleave", () => { hover = -1; $("tip").textContent = "Hover or tap a market."; });

  const es = new EventSource("api/live");
  es.onmessage = (m) => {
    S = JSON.parse(m.data);
    if (!status(S)) return;
    hot(S); tape(S);
    $("netHint").textContent = `${S.links.length} links fitted on the last ${S.status.window.replace("h0m0s", "h")} · rings pulse when a market trades`;
  };
  requestAnimationFrame(drawNet);
})();
