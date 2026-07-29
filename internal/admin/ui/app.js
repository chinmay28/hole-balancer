/* hole-balancer management interface.
 *
 * Plain ES modules, no build step and no dependencies: the page is served by
 * the balancer itself and has to load on a network whose DNS is broken.
 */
(() => {
  "use strict";

  const REFRESH_MS = 5000;
  const SERIES_SLOTS = 8;

  const state = {
    overview: null,
    stats: null,
    range: "hour",
    /** Colour slot per upstream name. Assigned on first sight and never
     *  reassigned, so a Pi-hole keeps its colour when another is added,
     *  removed, or overtakes it in the ranking. */
    colours: new Map(),
    editingFallback: false,
    editingAdd: false,
    /** True while a reading is on screen. The five-second refresh rebuilds the
     *  chart from scratch, which would otherwise yank the tooltip out from
     *  under whoever is reading it — very visible on a phone, where a tap is
     *  the only way to see a value. */
    chartBusy: false,
  };

  const $ = (id) => document.getElementById(id);
  const el = (tag, cls, text) => {
    const n = document.createElement(tag);
    if (cls) n.className = cls;
    if (text !== undefined) n.textContent = text;
    return n;
  };

  /* ---------- formatting ------------------------------------------- */

  const nf = new Intl.NumberFormat();
  const num = (n) => nf.format(Math.round(n || 0));

  function compact(n) {
    if (n < 1000) return String(Math.round(n));
    if (n < 1e6) return (n / 1e3).toFixed(n < 10e3 ? 1 : 0) + "k";
    return (n / 1e6).toFixed(1) + "M";
  }

  function pct(x) {
    if (!isFinite(x) || x <= 0) return "0%";
    if (x < 0.001) return "<0.1%";
    return (x * 100).toFixed(x < 0.1 ? 1 : 0) + "%";
  }

  function duration(sec) {
    sec = Math.max(0, Math.floor(sec || 0));
    const d = Math.floor(sec / 86400);
    const h = Math.floor((sec % 86400) / 3600);
    const m = Math.floor((sec % 3600) / 60);
    if (d) return `${d}d ${h}h`;
    if (h) return `${h}h ${m}m`;
    if (m) return `${m}m`;
    return `${sec}s`;
  }

  function ms(v) {
    if (!v) return "—";
    // A non-breaking space keeps "0.11 ms" on one line in a narrow stat tile.
    return (v < 10 ? v.toFixed(2) : String(Math.round(v))) + "\u00a0ms";
  }

  function clockLabel(iso, range) {
    const d = new Date(iso);
    if (isNaN(d)) return "";
    const hh = String(d.getHours()).padStart(2, "0");
    const mm = String(d.getMinutes()).padStart(2, "0");
    return range === "day" ? `${hh}:00` : `${hh}:${mm}`;
  }

  /* ---------- colour ------------------------------------------------ */

  function colourFor(name) {
    if (!state.colours.has(name)) {
      // Fixed order, never cycled: past the palette a node shares the last
      // slot rather than getting a generated hue that could collide.
      const slot = Math.min(state.colours.size + 1, SERIES_SLOTS);
      state.colours.set(name, `var(--series-${slot})`);
    }
    return state.colours.get(name);
  }

  /* ---------- toast & banner ---------------------------------------- */

  let toastTimer = null;
  function toast(msg, bad) {
    const t = $("toast");
    t.textContent = msg;
    t.className = "toast" + (bad ? " bad" : "");
    t.hidden = false;
    clearTimeout(toastTimer);
    toastTimer = setTimeout(() => { t.hidden = true; }, bad ? 7000 : 3200);
  }

  function banner(msg, bad) {
    const b = $("banner");
    if (!msg) { b.hidden = true; return; }
    b.textContent = msg;
    b.className = "banner" + (bad ? " bad" : "");
    b.hidden = false;
  }

  /* ---------- API --------------------------------------------------- */

  async function api(method, path, body) {
    const opts = { method, headers: {} };
    if (body !== undefined) {
      opts.headers["Content-Type"] = "application/json";
      opts.body = JSON.stringify(body);
    }
    const res = await fetch(path, opts);
    const text = await res.text();
    let data = null;
    try { data = text ? JSON.parse(text) : null; } catch { /* not JSON */ }
    if (!res.ok) {
      throw new Error((data && data.error) || text || `${res.status} ${res.statusText}`);
    }
    return data;
  }

  async function mutate(method, path, body, okMsg) {
    try {
      await api(method, path, body);
      if (okMsg) toast(okMsg);
      await refresh();
      return true;
    } catch (err) {
      toast(err.message, true);
      return false;
    }
  }

  /* ---------- tiles ------------------------------------------------- */

  function tile(label, value, note, opts) {
    const t = el("div", "tile");
    t.append(el("div", "label", label));
    const v = el("div", "value" + (opts && opts.small ? " small" : ""), value);
    if (opts && opts.colour) v.style.color = opts.colour;
    t.append(v);
    if (note) t.append(el("div", "note", note));
    return t;
  }

  function renderTiles() {
    const o = state.overview, s = state.stats;
    if (!o || !s) return;
    const box = $("tiles");
    box.textContent = "";

    box.append(tile("Queries", compact(s.total_queries),
      `${s.queries_per_minute_recent.toFixed(1)}/min recently`));

    box.append(tile("Pi-holes up", `${o.healthy_upstreams}/${o.total_upstreams}`,
      o.healthy_upstreams === 0 ? "none answering" : "in rotation",
      { colour: o.healthy_upstreams === 0 ? "var(--critical)"
        : o.healthy_upstreams < o.total_upstreams ? "var(--warning)" : undefined }));

    box.append(tile("Busiest Pi-hole", s.top_node || "—",
      s.top_node ? `${compact(s.top_node_queries)} queries · ${pct(s.nodes[0].share)}` : "no traffic yet",
      { small: true, colour: s.top_node ? colourFor(s.top_node) : undefined }));

    box.append(tile("Avg response", ms(s.avg_latency_ms),
      s.retries ? `${compact(s.retries)} retries` : "no retries"));

    box.append(tile("Blocked", pct(s.blocked_share), "NXDOMAIN share"));

    box.append(tile("Unfiltered", compact(s.fallback_queries),
      o.fallback.active ? "public DNS active now" : "via public DNS",
      { colour: o.fallback.active ? "var(--critical)" : undefined }));
  }

  /* ---------- bar lists --------------------------------------------- */

  function renderBars(node, rows, colourOf) {
    node.textContent = "";
    if (!rows.length) {
      node.append(el("p", "empty", "No queries recorded yet."));
      return;
    }
    const max = Math.max(...rows.map((r) => r.count), 1);
    for (const r of rows) {
      const row = el("div", "bar-row");
      row.append(el("div", "bar-label", r.name));

      const track = el("div", "bar-track");
      const fill = el("div", "bar-fill");
      fill.style.width = Math.max(2, (r.count / max) * 100) + "%";
      fill.style.background = colourOf(r.name);
      track.append(fill);
      row.append(track);

      // Every bar is directly labelled: several palette slots sit below 3:1
      // against the light surface, so the number must not be colour-only.
      const val = el("div", "bar-value", num(r.count));
      val.append(el("span", "pct", pct(r.share)));
      row.append(val);

      row.title = `${r.name}: ${num(r.count)} (${pct(r.share)})`;
      node.append(row);
    }
  }

  function renderNodeChart() {
    const s = state.stats;
    if (!s) return;
    const rows = (s.nodes || []).map((n) => ({ name: n.name, count: n.queries, share: n.share }));
    if (s.fallback_queries > 0) {
      rows.push({
        name: "public DNS",
        count: s.fallback_queries,
        share: s.total_queries ? s.fallback_queries / s.total_queries : 0,
      });
    }
    renderBars($("node-chart"), rows, (name) =>
      name === "public DNS" ? "var(--text-muted)" : colourFor(name));

    $("nodes-sub").textContent = s.top_node
      ? `${s.top_node} has handled the most — ${pct(s.nodes[0].share)} of all queries.`
      : "Waiting for traffic.";

    // Table view: the accessible alternative to reading values off colour.
    const wrap = $("node-table");
    wrap.textContent = "";
    const table = el("table");
    const thead = el("thead");
    const hr = el("tr");
    ["Pi-hole", "Queries", "Share", "Retries", "Avg latency"].forEach((h, i) => {
      const th = el("th", i > 0 ? "num" : "", h);
      hr.append(th);
    });
    thead.append(hr);
    table.append(thead);
    const tb = el("tbody");
    for (const n of s.nodes || []) {
      const tr = el("tr");
      tr.append(el("td", "", n.name));
      tr.append(el("td", "num", num(n.queries)));
      tr.append(el("td", "num", pct(n.share)));
      tr.append(el("td", "num", num(n.retries)));
      tr.append(el("td", "num", ms(n.avg_latency_ms)));
      tb.append(tr);
    }
    table.append(tb);
    wrap.append(table);
  }

  function renderRCodes() {
    const s = state.stats;
    if (!s) return;
    // Response codes get their own fixed mapping, so NOERROR is not repainted
    // when a SERVFAIL appears and shifts the ordering.
    const fixed = {
      NOERROR: "var(--series-1)",
      NXDOMAIN: "var(--series-3)",
      SERVFAIL: "var(--critical)",
      REFUSED: "var(--series-2)",
      NOTIMP: "var(--series-4)",
      FORMERR: "var(--series-5)",
    };
    renderBars($("rcode-chart"), s.rcodes || [], (n) => fixed[n] || "var(--series-7)");
    renderBars($("qtype-chart"), (s.query_types || []).slice(0, 6), () => "var(--series-1)");
  }

  /* ---------- traffic chart ----------------------------------------- */

  const SVG = "http://www.w3.org/2000/svg";
  const svgEl = (tag, attrs) => {
    const n = document.createElementNS(SVG, tag);
    for (const k in attrs) n.setAttribute(k, attrs[k]);
    return n;
  };

  function renderTraffic() {
    const s = state.stats;
    const host = $("traffic");
    if (!s) return;

    // Do not redraw underneath someone reading a value off the chart.
    if (state.chartBusy && host.firstChild) return;

    const points = state.range === "day" ? s.last_day : s.last_hour;
    host.textContent = "";
    if (!points || !points.length) {
      host.append(el("p", "empty", "No history yet."));
      return;
    }

    const total = points.reduce((a, p) => a + p.queries, 0);
    const anyFallback = points.some((p) => p.fallback > 0);
    $("traffic-sub").textContent = state.range === "day"
      ? `${num(total)} queries in the last 24 hours, by hour.`
      : `${num(total)} queries in the last hour, by minute.`;

    // The viewBox is sized to the element's actual width so the mapping is
    // 1:1. Stretching a fixed viewBox to fit would squash the axis labels
    // horizontally, which on a phone makes them unreadable.
    const W = Math.max(260, Math.round(host.clientWidth || 900));
    const H = W < 520 ? 160 : 190;
    const pad = { t: 12, r: 10, b: 22, l: W < 420 ? 32 : 42 };
    const iw = W - pad.l - pad.r, ih = H - pad.t - pad.b;
    const max = Math.max(...points.map((p) => p.queries), 1);
    const niceMax = niceCeil(max);

    const svg = svgEl("svg", {
      viewBox: `0 0 ${W} ${H}`,
      role: "img",
      "aria-label": `Queries per ${state.range === "day" ? "hour" : "minute"}, ${num(total)} total`,
    });

    const x = (i) => pad.l + (points.length === 1 ? iw / 2 : (i / (points.length - 1)) * iw);
    const y = (v) => pad.t + ih - (v / niceMax) * ih;

    for (let g = 0; g <= 2; g++) {
      const v = (niceMax / 2) * g;
      svg.append(svgEl("line", { class: g === 0 ? "baseline" : "gridline", x1: pad.l, x2: W - pad.r, y1: y(v), y2: y(v) }));
      const t = svgEl("text", { class: "tick", x: pad.l - 7, y: y(v) + 3.5, "text-anchor": "end" });
      t.textContent = compact(v);
      svg.append(t);
    }

    const line = points.map((p, i) => `${i ? "L" : "M"}${x(i).toFixed(1)},${y(p.queries).toFixed(1)}`).join("");
    svg.append(svgEl("path", { class: "area", d: `${line}L${x(points.length - 1)},${y(0)}L${x(0)},${y(0)}Z` }));
    svg.append(svgEl("path", { class: "line", d: line }));

    if (anyFallback) {
      svg.append(svgEl("path", {
        class: "fb-line",
        d: points.map((p, i) => `${i ? "L" : "M"}${x(i).toFixed(1)},${y(p.fallback).toFixed(1)}`).join(""),
      }));
    }

    // Fewer labels on a narrow screen, so they never collide.
    const wantTicks = Math.max(2, Math.min(6, Math.floor(iw / 62)));
    const step = Math.max(1, Math.ceil(points.length / wantTicks));
    for (let i = 0; i < points.length; i += step) {
      const anchor = i === 0 ? "start" : i > points.length - step ? "end" : "middle";
      const t = svgEl("text", { class: "tick", x: x(i), y: H - 6, "text-anchor": anchor });
      t.textContent = clockLabel(points[i].at, state.range);
      svg.append(t);
    }

    const cross = svgEl("line", { class: "crosshair", y1: pad.t, y2: pad.t + ih, opacity: 0 });
    const cursor = svgEl("circle", { class: "cursor", r: 4, opacity: 0 });
    svg.append(cross, cursor);
    host.append(svg);

    const tip = el("div", "tooltip");
    tip.hidden = true;
    host.append(tip);
    let tipTimer = null;

    // Hover layer: an HTML chart is interactive by default. Bound to pointer
    // events rather than mouse ones so a finger scrubs the same way a cursor
    // hovers.
    const showAt = (ev) => {
      const box = svg.getBoundingClientRect();
      const rel = ((ev.clientX - box.left) / box.width) * W;
      let i = Math.round(((rel - pad.l) / iw) * (points.length - 1));
      i = Math.max(0, Math.min(points.length - 1, i));
      const p = points[i];

      cross.setAttribute("x1", x(i)); cross.setAttribute("x2", x(i)); cross.setAttribute("opacity", 1);
      cursor.setAttribute("cx", x(i)); cursor.setAttribute("cy", y(p.queries)); cursor.setAttribute("opacity", 1);

      tip.textContent = "";
      tip.append(el("div", "tt-head", clockLabel(p.at, state.range)));
      const row = (k, v) => {
        const r = el("div", "tt-row");
        r.append(el("span", null, k));
        const b = el("b", null, v);
        r.append(b);
        return r;
      };
      tip.append(row("queries", num(p.queries)));
      if (p.blocked) tip.append(row("blocked", num(p.blocked)));
      if (p.fallback) tip.append(row("unfiltered", num(p.fallback)));
      if (p.failed) tip.append(row("failed", num(p.failed)));
      tip.hidden = false;

      // Keep the tooltip inside the chart, and out from under the finger.
      const px = (x(i) / W) * box.width;
      const tw = tip.offsetWidth;
      tip.style.left = Math.min(Math.max(px - tw / 2, 2), Math.max(2, box.width - tw - 2)) + "px";
      tip.style.top = "4px";
      state.chartBusy = true;
    };
    const hide = () => {
      clearTimeout(tipTimer);
      state.chartBusy = false;
      tip.hidden = true;
      cross.setAttribute("opacity", 0);
      cursor.setAttribute("opacity", 0);
    };

    svg.addEventListener("pointermove", showAt);
    svg.addEventListener("pointerdown", showAt);
    svg.addEventListener("pointercancel", hide);
    // A touch pointer "leaves" the moment the finger lifts, so honouring that
    // event for touch would hide the reading before it could be read. Only a
    // mouse leaving means the cursor has actually moved away.
    svg.addEventListener("pointerleave", (ev) => {
      if (ev.pointerType === "mouse") hide();
    });
    // After a tap, leave the reading up long enough to read, then clear it.
    svg.addEventListener("pointerup", (ev) => {
      if (ev.pointerType === "mouse") return;
      clearTimeout(tipTimer);
      tipTimer = setTimeout(hide, 4000);
    });

    if (anyFallback) {
      const legend = el("div", "legend");
      const key = (colour, label, dashed) => {
        const k = el("span", "key");
        const i = el("i");
        i.style.background = colour;
        if (dashed) i.style.opacity = "0.75";
        k.append(i, document.createTextNode(label));
        return k;
      };
      legend.append(key("var(--series-1)", "all queries"));
      legend.append(key("var(--series-2)", "answered by public DNS (unfiltered)", true));
      host.append(legend);
    }
  }

  function niceCeil(v) {
    if (v <= 5) return 5;
    const mag = Math.pow(10, Math.floor(Math.log10(v)));
    return Math.ceil(v / mag) * mag;
  }

  /* ---------- pool -------------------------------------------------- */

  function renderPool() {
    const o = state.overview;
    if (!o) return;
    const box = $("pool");
    box.textContent = "";
    const canWrite = o.control.enabled;

    for (const u of o.upstreams) {
      const node = el("div", "node" + (u.healthy ? "" : " is-down"));

      const left = el("div");
      const head = el("div", "node-head");
      const sw = el("span", "swatch");
      sw.style.background = colourFor(u.name);
      head.append(sw, el("span", "node-name", u.name));

      const pill = el("span", "pill " + (u.drained ? "warn" : u.healthy ? "ok" : "bad"));
      pill.append(el("span", "dot"));
      pill.append(document.createTextNode(u.drained ? "Drained" : u.healthy ? "Up" : "Down"));
      head.append(pill);
      head.append(el("span", "node-meta", `weight ${u.weight}`));
      left.append(head);

      const eps = el("ul", "eps");
      for (const e of u.endpoints) {
        const li = el("li", "ep");

        const main = el("span", "ep-main");
        const star = el("span", "star", e.preferred ? "★" : "");
        if (e.preferred) star.title = "carrying this Pi-hole's traffic";
        main.append(star, el("span", "addr", e.addr));
        li.append(main);

        const facts = el("span", "ep-facts");
        facts.append(el("span", "state " + (e.healthy ? "up" : "down"), e.healthy ? "up" : "down"));
        if (e.healthy && e.latency_ms) facts.append(el("span", null, ms(e.latency_ms)));
        if (e.queries) facts.append(el("span", null, `${num(e.queries)} q`));
        li.append(facts);

        if (!e.healthy && e.last_error) li.append(el("span", "why", e.last_error));
        eps.append(li);
      }
      left.append(eps);
      node.append(left);

      const actions = el("div", "node-actions");
      const drain = el("button", "btn small", u.drained ? "Undrain" : "Drain");
      drain.type = "button";
      drain.disabled = !canWrite;
      drain.title = u.drained
        ? "Put this Pi-hole back into rotation"
        : "Stop sending queries here without removing it";
      drain.addEventListener("click", () =>
        mutate("POST", `/api/upstreams/${encodeURIComponent(u.name)}/drain`,
          { drained: !u.drained }, u.drained ? `${u.name} back in rotation` : `${u.name} drained`));

      const remove = el("button", "btn small danger", "Remove");
      remove.type = "button";
      remove.disabled = !canWrite;
      remove.addEventListener("click", () => {
        if (!confirm(`Remove ${u.name} from the pool?\n\nThis is written to the configuration file.`)) return;
        mutate("DELETE", `/api/upstreams/${encodeURIComponent(u.name)}`, undefined, `${u.name} removed`);
      });

      actions.append(drain, remove);
      node.append(actions);
      box.append(node);
    }
  }

  /* ---------- fallback ---------------------------------------------- */

  function renderFallback() {
    const o = state.overview;
    if (!o) return;
    const fb = o.fallback;

    const st = $("fallback-state");
    st.className = "pill " + (!fb.enabled ? "quiet" : fb.active ? "bad" : "ok");
    st.textContent = "";
    st.append(el("span", "dot"));
    st.append(document.createTextNode(
      !fb.enabled ? "Disabled" : fb.active ? "Active — answers unfiltered" : "Standby"));

    $("fb-hint").textContent = fb.queries_this_window
      ? `${num(fb.queries_this_window)} unfiltered queries over ${fb.outages_this_window} outage(s) this reporting window.`
      : "";

    // Do not clobber what someone is halfway through typing.
    if (state.editingFallback) return;
    $("fb-enabled").checked = fb.enabled;
    $("fb-servers").value = (fb.servers || []).join("\n");
  }

  /* ---------- header & control gating -------------------------------- */

  function renderHeader() {
    const o = state.overview;
    if (!o) return;

    $("version").textContent = `${o.version} · up ${duration(o.uptime_seconds)}`;

    const pill = $("health-pill");
    const healthy = o.healthy_upstreams, total = o.total_upstreams;
    pill.className = "pill " + (healthy === 0 ? "bad" : healthy < total ? "warn" : "ok");
    $("health-text").textContent = healthy === 0
      ? "No Pi-hole answering"
      : `${healthy} of ${total} up`;

    const sel = $("strategy");
    if (document.activeElement !== sel) {
      sel.textContent = "";
      for (const name of o.available_strategies) {
        const opt = el("option", null, name);
        opt.value = name;
        if (name === o.strategy) opt.selected = true;
        sel.append(opt);
      }
    }
    sel.disabled = !o.control.enabled;

    $("add-open").disabled = !o.control.enabled;
    $("fb-enabled").disabled = !o.control.enabled;
    $("fb-servers").disabled = !o.control.enabled;
    document.querySelector("#fallback-form button[type=submit]").disabled = !o.control.enabled;

    $("foot-config").textContent = o.control.config_path
      ? `changes saved to ${o.control.config_path}`
      : "";

    if (!o.control.enabled) {
      banner("Read-only. " + o.control.reason);
    } else if (o.control.reason) {
      banner(o.control.reason);
    } else if (o.healthy_upstreams === 0 && o.fallback.active) {
      banner("No Pi-hole is answering. Queries are going to public DNS and are NOT being filtered.", true);
    } else if (o.healthy_upstreams === 0) {
      banner("No Pi-hole is answering and public DNS fallback is off — DNS is failing.", true);
    } else {
      banner(null);
    }
  }

  /* ---------- refresh ------------------------------------------------ */

  let failures = 0;
  async function refresh() {
    try {
      const [overview, stats] = await Promise.all([
        api("GET", "/api/overview"),
        api("GET", "/api/stats"),
      ]);
      state.overview = overview;
      state.stats = stats;
      failures = 0;

      // Seed colours in configuration order so a node's colour does not depend
      // on how busy it happens to be.
      for (const u of overview.upstreams) colourFor(u.name);

      renderHeader();
      renderTiles();
      renderTraffic();
      renderNodeChart();
      renderRCodes();
      renderPool();
      renderFallback();
    } catch (err) {
      if (++failures === 1) banner("Lost contact with the balancer: " + err.message, true);
    }
  }

  /* ---------- wiring -------------------------------------------------- */

  function init() {
    // Theme toggle, remembered locally. Both modes are designed, not flipped.
    const saved = localStorage.getItem("hb-theme");
    if (saved) document.documentElement.setAttribute("data-theme", saved);
    $("theme-toggle").addEventListener("click", () => {
      const cur = document.documentElement.getAttribute("data-theme");
      const dark = cur ? cur === "dark" : matchMedia("(prefers-color-scheme: dark)").matches;
      const next = dark ? "light" : "dark";
      document.documentElement.setAttribute("data-theme", next);
      localStorage.setItem("hb-theme", next);
    });

    for (const b of document.querySelectorAll(".seg-btn")) {
      b.addEventListener("click", () => {
        document.querySelectorAll(".seg-btn").forEach((x) => x.classList.toggle("is-on", x === b));
        state.range = b.dataset.range;
        state.chartBusy = false;
        renderTraffic();
      });
    }

    $("nodes-table-toggle").addEventListener("click", (ev) => {
      const wrap = $("node-table"), chart = $("node-chart");
      const showTable = wrap.hidden;
      wrap.hidden = !showTable;
      chart.hidden = showTable;
      ev.target.textContent = showTable ? "Chart" : "Table";
      ev.target.setAttribute("aria-expanded", String(showTable));
    });

    $("strategy").addEventListener("change", (ev) =>
      mutate("PUT", "/api/strategy", { strategy: ev.target.value }, `Strategy set to ${ev.target.value}`));

    $("add-open").addEventListener("click", () => {
      state.editingAdd = true;
      $("add-form").hidden = false;
      $("add-name").focus();
    });
    $("add-cancel").addEventListener("click", () => {
      state.editingAdd = false;
      $("add-form").hidden = true;
      $("add-form").reset();
    });
    $("add-form").addEventListener("submit", async (ev) => {
      ev.preventDefault();
      const endpoints = $("add-endpoints").value.split("\n").map((s) => s.trim()).filter(Boolean);
      const ok = await mutate("POST", "/api/upstreams", {
        name: $("add-name").value.trim(),
        weight: Number($("add-weight").value) || 1,
        endpoints,
      }, "Pi-hole added");
      if (ok) {
        state.editingAdd = false;
        $("add-form").hidden = true;
        $("add-form").reset();
        $("add-weight").value = "1";
      }
    });

    for (const id of ["fb-enabled", "fb-servers"]) {
      $(id).addEventListener("input", () => { state.editingFallback = true; });
    }
    $("fallback-form").addEventListener("submit", async (ev) => {
      ev.preventDefault();
      const servers = $("fb-servers").value.split("\n").map((s) => s.trim()).filter(Boolean);
      const ok = await mutate("PUT", "/api/fallback",
        { enabled: $("fb-enabled").checked, servers }, "Fallback saved");
      if (ok) state.editingFallback = false;
    });

    // The chart is drawn to the pixel width it occupies, so a rotation or a
    // window resize has to redraw it.
    let resizeTimer = null;
    let lastWidth = window.innerWidth;
    addEventListener("resize", () => {
      if (window.innerWidth === lastWidth) return; // iOS fires on scroll
      lastWidth = window.innerWidth;
      clearTimeout(resizeTimer);
      resizeTimer = setTimeout(() => {
        state.chartBusy = false;
        renderTraffic();
      }, 150);
    });

    refresh();
    setInterval(refresh, REFRESH_MS);
  }

  if (document.readyState === "loading") {
    document.addEventListener("DOMContentLoaded", init);
  } else {
    init();
  }
})();
