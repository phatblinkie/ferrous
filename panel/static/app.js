/* ferrous panel — vanilla JS */
"use strict";

const $ = (s) => document.querySelector(s);
const $$ = (s) => [...document.querySelectorAll(s)];

let OVERVIEW = { hosts: [] };   // last /api/overview payload
let pollTimer = null;
let ACTIVE = "servers";         // active tab
let CUR = null;                 // selected server key: "<hostId>:<containerId>"
const HOST_FILTER = () => $("#host-filter").value;

/* ------------------------------------------------------------- api helper */
async function api(path, opts = {}) {
  const o = {
    method: opts.method || "GET",
    headers: { "X-Requested-With": "ferrous" },
    credentials: "same-origin",
  };
  if (opts.body !== undefined) {
    o.headers["Content-Type"] = "application/json";
    o.body = JSON.stringify(opts.body);
  }
  const r = await fetch(path, o);
  if (r.status === 401 && path !== "/api/me") { showLogin(); throw new Error("unauthorized"); }
  let data = null;
  try { data = await r.json(); } catch (e) { /* ignore */ }
  return { status: r.status, data };
}

function toast(msg, kind = "") {
  const t = document.createElement("div");
  t.className = "toast " + kind;
  t.textContent = msg;
  $("#toasts").appendChild(t);
  setTimeout(() => t.remove(), 4500);
}

function esc(s) {
  return String(s ?? "").replace(/[&<>"]/g, (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;" }[c]));
}

function fmtDur(sec) {
  if (sec == null) return "–";
  sec = Math.round(sec);
  const h = Math.floor(sec / 3600), m = Math.floor((sec % 3600) / 60), s = sec % 60;
  if (h) return `${h}h ${m}m`;
  if (m) return `${m}m ${s}s`;
  return `${s}s`;
}

/* ------------------------------------------------------------- auth / tabs */
function showLogin() { $("#login").classList.remove("hidden"); $("#login-user").focus(); }
function hideLogin() { $("#login").classList.add("hidden"); }

$("#login-form").addEventListener("submit", async (e) => {
  e.preventDefault();
  $("#login-err").textContent = "";
  const { status, data } = await api("/api/login", {
    method: "POST",
    body: { username: $("#login-user").value, password: $("#login-pw").value },
  });
  if (status === 200) {
    hideLogin();
    $("#login-pw").value = "";
    boot();
  } else {
    $("#login-err").textContent = (data && data.error) || "login failed";
  }
});

$("#btn-logout").addEventListener("click", async () => {
  await api("/api/logout", { method: "POST", body: {} });
  stopPoll();
  stopStream();
  setupPlTimer();
  showLogin();
});

function activateTab(name) {
  ACTIVE = name;
  $$(".tab").forEach((x) => x.classList.toggle("active", x.dataset.tab === name));
  $$(".tabpane").forEach((p) => p.classList.toggle("active", p.id === "tab-" + name));
  if (name === "players") refreshPlayers();
  setupPlTimer();
}
$$(".tab").forEach((b) => b.addEventListener("click", () => activateTab(b.dataset.tab)));

$("#btn-refresh").addEventListener("click", () => pollOverview());

/* ------------------------------------------------------------- selection */
function selected() {
  if (!CUR) return null;
  const i = CUR.indexOf(":");
  const hid = CUR.slice(0, i), sid = CUR.slice(i + 1);
  const host = OVERVIEW.hosts.find((h) => String(h.id) === hid);
  const srv = host && host.servers.find((s) => s.id === sid);
  return host && srv ? { host, srv } : null;
}

function renderServerSel() {
  const sel = $("#server-sel");
  const keys = [];
  const labels = {};
  for (const h of OVERVIEW.hosts) {
    for (const s of h.servers) {
      const k = `${h.id}:${s.id}`;
      keys.push(k);
      labels[k] = `${h.name} › ${s.name || s.short_id}`;
    }
  }
  // keep the current selection when it still exists; else the remembered one;
  // else the first server
  const stored = localStorage.getItem("ferrous.server");
  if (CUR && !keys.includes(CUR)) CUR = null;
  if (!CUR && stored && keys.includes(stored)) CUR = stored;
  if (!CUR && keys.length) CUR = keys[0];

  sel.innerHTML = keys.length ? "" : '<option value="">(no servers)</option>';
  for (const k of keys) {
    const o = document.createElement("option");
    o.value = k;
    o.textContent = labels[k];
    sel.appendChild(o);
  }
  if (CUR) sel.value = CUR;
}

$("#server-sel").addEventListener("change", (e) => {
  CUR = e.target.value || null;
  onServerChange();
});

// called when the selected server changes: re-point every server-scoped view
function onServerChange() {
  if (CUR) localStorage.setItem("ferrous.server", CUR); // never wipe on transient empty
  // logs
  logBuf = [];
  $("#logview").innerHTML = "";
  $("#log-count").textContent = "0 lines";
  startStream();
  // players
  closeDrawer();
  if (ACTIVE === "players") refreshPlayers();
  setupPlTimer();
  // console
  $("#conview").innerHTML = "";
}

/* ------------------------------------------------------------- overview */
async function pollOverview() {
  const { status, data } = await api("/api/overview");
  if (status !== 200 || !data) return;
  OVERVIEW = data;
  const prev = CUR;
  renderHeader();
  renderHostFilter();
  renderServerSel();
  renderServers();
  renderHosts();
  $("#srv-updated").textContent = "updated " + new Date().toLocaleTimeString();
  if (CUR !== prev) onServerChange();
}

function renderHeader() {
  const hs = OVERVIEW.hosts;
  const up = hs.filter((h) => h.ok === true).length;
  const down = hs.filter((h) => h.ok === false).length;
  const pill = $("#host-pill");
  if (!hs.length) { pill.textContent = "○ no hosts"; pill.className = "pill grey"; }
  else if (down) { pill.textContent = `✖ ${down} down`; pill.className = "pill red"; }
  else { pill.textContent = `● ${up}/${hs.length} up`; pill.className = "pill green"; }
  pill.title = hs.map((h) => `${h.name}: ${h.ok === true ? "ok" : h.ok === false ? h.error : "disabled"}`).join("\n");
  const n = hs.reduce((a, h) => a + h.servers.length, 0);
  $("#host-summary").textContent = hs.length
    ? `${hs.length} host${hs.length > 1 ? "s" : ""} · ${n} container${n === 1 ? "" : "s"}`
    : "no hosts yet — open the Hosts tab";
}

function renderHostFilter() {
  const sel = $("#host-filter");
  const cur = sel.value || "all";
  sel.innerHTML = '<option value="all">all hosts</option>';
  for (const h of OVERVIEW.hosts) {
    const o = document.createElement("option");
    o.value = String(h.id);
    o.textContent = h.name + (h.ok === false ? " (down)" : "");
    sel.appendChild(o);
  }
  sel.value = [...sel.options].some((o) => o.value === cur) ? cur : "all";
}

/* ------------------------------------------------------------- servers */
function statePill(state) {
  if (state === "running") return '<span class="pill green">● running</span>';
  if (state === "exited") return '<span class="pill grey">○ exited</span>';
  if (state === "created") return '<span class="pill yellow">◌ created</span>';
  if (state === "paused") return '<span class="pill yellow">⏸ paused</span>';
  return `<span class="pill grey">${esc(state || "?")}</span>`;
}

function renderServers() {
  const body = $("#srv-body");
  const f = HOST_FILTER();
  const rows = [];
  const hosts = OVERVIEW.hosts.filter((h) => f === "all" || String(h.id) === f);
  for (const h of hosts) {
    if (h.ok === false) {
      rows.push(`<tr class="hosterr"><td colspan="7" class="dim center">host <b>${esc(h.name)}</b> unreachable — ${esc(h.error || "unknown error")} · retrying every 5s</td></tr>`);
      continue;
    }
    for (const s of h.servers) {
      const run = s.state === "running";
      const isCur = CUR === `${h.id}:${s.id}`;
      rows.push(`<tr data-h="${h.id}" data-s="${esc(s.id)}"${isCur ? ' class="sel"' : ""}>
        <td><b>${esc(s.name || s.short_id)}</b>${s.managed ? "" : ' <span class="dim">(foreign)</span>'}${s.rcon ? ' <span class="pill grey" title="WebRCON configured">rcon</span>' : ""}</td>
        <td class="dim">${esc(h.name)}</td>
        <td class="dim">${esc(s.image)}</td>
        <td>${statePill(s.state)}</td>
        <td class="dim">${esc(s.status)}</td>
        <td class="pwr">
          <button class="btn sm" data-act="start" ${run ? "disabled" : ""} title="start">▶</button>
          <button class="btn sm warn" data-act="stop" ${run ? "" : "disabled"} title="stop">■</button>
          <button class="btn sm accent" data-act="restart" ${run ? "" : "disabled"} title="restart">↻</button>
        </td>
        <td><button class="btn sm con" title="select this server and open its live logs">logs ▸</button></td>
      </tr>`);
    }
  }
  body.innerHTML = rows.length ? rows.join("")
    : '<tr><td colspan="7" class="dim center">' +
      (OVERVIEW.hosts.length ? "no servers (or none managed yet)" : "no hosts — add one in the Hosts tab") +
      "</td></tr>";

  body.querySelectorAll("tr[data-s]").forEach((tr) => {
    const hid = +tr.dataset.h, sid = tr.dataset.s;
    const host = OVERVIEW.hosts.find((h) => h.id === hid);
    const srv = host && host.servers.find((s) => s.id === sid);
    tr.querySelectorAll("[data-act]").forEach((b) =>
      b.addEventListener("click", () => power(host, srv, b.dataset.act)));
    tr.querySelector(".con").addEventListener("click", () => {
      CUR = `${hid}:${sid}`;
      $("#server-sel").value = CUR;
      onServerChange();
      activateTab("logs");
    });
  });
}

async function power(host, srv, action) {
  if (!host || !srv) return;
  const verbs = {
    stop: `STOP ${srv.name}? Anyone playing will be disconnected.`,
    restart: `RESTART ${srv.name}? Anyone playing will be disconnected.`,
  };
  if (action !== "start" && !confirm(verbs[action])) return;
  const { status, data } = await api("/api/power", {
    method: "POST",
    body: { host: host.id, server: srv.id, action },
  });
  if (status === 200 && data.ok) {
    toast(`${action} issued — ${srv.name} is ${data.state || "changing state"}`, "ok");
  } else {
    toast(`${action} failed: ${(data && data.error) || "unknown"}`, "bad");
  }
  pollOverview();
}

/* ------------------------------------------------------------- live logs */
let es = null, logBuf = [], follow = true, renderQueued = false;
const MAX_LINES = 3000;

function classify(text) {
  if (/\b(Exception|Error|NullReference|error:|\bCRASH\b|Aborted|assert failed)/i.test(text)) return "err";
  if (/\[Global\]|\[Server\]|\[Team\]| : /.test(text) && / : .+/.test(text)) return "chat";
  if (/\bdied \(|was killed by\b/i.test(text)) return "err";
  if (/joined \[|joined from ip|with steamid \d+ joined|disconnecting:|has disconnected|connection destroyed|left the game|is connecting/i.test(text)) return "join";
  if (/^\s*\[[A-Z][^\]]*\]\s+\w+:/.test(text) || /oxide|uMod|\[CSharp\]/i.test(text)) return "plugin";
  if (/\b(warn|failed|failure|denied|cannot|unable|timed out)\b/i.test(text)) return "warn";
  return "";
}

function shortTs(iso) {
  if (!iso) return "";
  try { return iso.slice(11, 19); } catch (e) { return ""; }
}

function appendLine(item) {
  const text = (item.text || "").replace(/\s+$/, "");
  if (!text.trim()) return;
  const entry = { cls: classify(text), ts: shortTs(item.t), text };
  logBuf.push(entry);
  if (logBuf.length > MAX_LINES) logBuf.splice(0, logBuf.length - MAX_LINES);
  const view = $("#logview");
  const q = $("#log-filter").value.trim().toLowerCase();
  if (!q || text.toLowerCase().includes(q)) addEl(view, entry);
  $("#log-count").textContent = logBuf.length + " lines";
  scheduleScroll();
}

function addEl(view, e) {
  const d = document.createElement("div");
  d.className = "logline " + e.cls;
  const ts = document.createElement("span");
  ts.className = "ts"; ts.textContent = e.ts;
  const body = document.createElement("span");
  body.textContent = e.text;
  d.append(ts, body);
  view.appendChild(d);
}

function scheduleScroll() {
  if (renderQueued) return;
  renderQueued = true;
  requestAnimationFrame(() => {
    renderQueued = false;
    if (follow) { const v = $("#logview"); v.scrollTop = v.scrollHeight; }
  });
}

function rerender() {
  const view = $("#logview");
  view.innerHTML = "";
  const q = $("#log-filter").value.trim().toLowerCase();
  for (const e of logBuf) {
    if (!q || e.text.toLowerCase().includes(q)) addEl(view, e);
  }
  scheduleScroll();
}

function stopStream() {
  if (es) { es.close(); es = null; }
  $("#log-state").textContent = "no server selected";
  $("#log-state").className = "pill grey";
}

function startStream() {
  if (es) es.close();
  es = null;
  const sel = selected();
  if (!sel) {
    $("#log-title").textContent = "logs";
    $("#log-state").textContent = "no server selected";
    $("#log-state").className = "pill grey";
    return;
  }
  $("#log-title").textContent = `${sel.srv.name} @ ${sel.host.name}`;
  const url = `/api/logs?host=${sel.host.id}&server=${encodeURIComponent(sel.srv.id)}&tail=200&follow=1`;
  es = new EventSource(url);
  $("#log-state").textContent = "connecting…";
  $("#log-state").className = "pill grey";
  es.onopen = () => { $("#log-state").textContent = "live"; $("#log-state").className = "pill green"; };
  es.onmessage = (ev) => {
    let item;
    try { item = JSON.parse(ev.data); } catch (e) { return; }
    if (item.kind === "hello") {
      $("#log-state").textContent = `live · tail ${item.tail === -1 ? "all" : item.tail}`;
      $("#log-state").className = "pill green";
      return;
    }
    if (item.kind === "log") appendLine(item);
    else if (item.kind === "end") {
      $("#log-state").textContent = "stream ended (container stopped)";
      $("#log-state").className = "pill yellow";
    } else if (item.kind === "error") {
      $("#log-state").textContent = "error: " + (item.error || "stream failed");
      $("#log-state").className = "pill red";
      appendLine({ t: null, text: "[ferrous] " + (item.error || "stream failed") });
    }
  };
  es.onerror = () => {
    $("#log-state").textContent = "disconnected — retrying…";
    $("#log-state").className = "pill red";
  };
}

$("#log-filter").addEventListener("input", rerender);
$("#log-clear").addEventListener("click", () => { logBuf = []; $("#logview").innerHTML = ""; $("#log-count").textContent = "0 lines"; });
$("#log-follow").addEventListener("change", (e) => {
  follow = e.target.checked;
  if (follow) scheduleScroll();
});
$("#logview").addEventListener("scroll", () => {
  const v = $("#logview");
  const atBottom = v.scrollHeight - v.scrollTop - v.clientHeight < 30;
  if (!atBottom && follow) { follow = false; $("#log-follow").checked = false; }
  else if (atBottom && !follow && $("#log-follow").checked) follow = true;
});
$("#host-filter").addEventListener("change", renderServers);

/* ------------------------------------------------------------- players */
let plSel = null, invTimer = null, plTimer = null;

function rconReady() {
  const sel = selected();
  if (!sel) return "no server selected — pick one in the header (or “logs ▸” in Servers)";
  if (sel.srv.rcon === false) return "selected container has no ferrous.rcon_port label (not a ferrous game server?)";
  return null;
}

async function refreshPlayers() {
  const body = $("#pl-body");
  const sel = selected();
  const why = rconReady();
  if (why) {
    body.innerHTML = `<tr><td colspan="9" class="dim center">${esc(why)}</td></tr>`;
    $("#pl-status").textContent = "–";
    return;
  }
  const { status, data } = await api(`/api/players?host=${sel.host.id}&server=${encodeURIComponent(sel.srv.id)}`);
  if (status !== 200 || !data) {
    let msg = (data && data.error) || "no data";
    if (msg.includes("rcon")) msg = "server offline or restarting — retrying automatically…";
    body.innerHTML = `<tr><td colspan="9" class="dim center">${esc(msg)}</td></tr>`;
    return;
  }
  const players = data.players || [];
  const badge = $("#tab-count");
  badge.textContent = String(players.length);
  badge.classList.toggle("hidden", !players.length);

  const st = data.status;
  $("#pl-status").textContent = st
    ? `${st.players ?? "–"}/${st.max ?? "–"} players` + (st.hostname ? ` · ${st.hostname}` : "")
    : "–";

  body.innerHTML = "";
  if (!players.length) {
    body.innerHTML = '<tr><td colspan="9" class="dim center">nobody online</td></tr>';
  }
  for (const p of players) {
    const tr = document.createElement("tr");
    tr.dataset.sid = p.SteamID;
    if (plSel === p.SteamID) tr.classList.add("sel");
    const hp = Math.max(0, Math.min(100, p.Health ?? 0));
    const hcls = hp < 30 ? "low" : hp < 65 ? "mid" : "";
    const pos = p.Position ? `${Math.round(p.Position.x)}, ${Math.round(p.Position.y)}, ${Math.round(p.Position.z)}` : "–";
    tr.innerHTML = `
      <td><b>${esc(p.DisplayName)}</b></td>
      <td class="dim">${p.SteamID}</td>
      <td>${p.Ping} ms</td>
      <td><span class="healthbar ${hcls}"><i style="width:${hp}%"></i></span>${(p.Health ?? 0).toFixed(0)}</td>
      <td>${fmtDur(p.ConnectedSeconds)}</td>
      <td class="dim">${pos}</td>
      <td>${p.TeamID && p.TeamID !== "0" ? p.TeamID : "–"}</td>
      <td>${p.IsMuted ? "🔇" : ""}</td>
      <td class="dim">${(p.Address || "").split(":")[0]}</td>`;
    tr.addEventListener("click", () => openDrawer(p));
    body.appendChild(tr);
  }
  $("#pl-updated").textContent = "updated " + new Date().toLocaleTimeString();
}

function openDrawer(p) {
  plSel = p.SteamID;
  $("#dw-name").textContent = p.DisplayName || "?";
  const g = $("#dw-grid");
  g.innerHTML = "";
  const rows = [
    ["SteamID", p.SteamID], ["OwnerSteamID", p.OwnerSteamID && p.OwnerSteamID !== "0" ? p.OwnerSteamID : "–"],
    ["EntityId", p.EntityId], ["Address", p.Address], ["Ping", p.Ping + " ms"],
    ["Health", (p.Health ?? 0).toFixed(1)], ["Position", p.Position ? `${p.Position.x.toFixed(1)}, ${p.Position.y.toFixed(1)}, ${p.Position.z.toFixed(1)}` : "–"],
    ["Connected", fmtDur(p.ConnectedSeconds)], ["TeamID", p.TeamID || "0"], ["IsMuted", String(!!p.IsMuted)],
    ["ViolationLevel", p.ViolationLevel], ["CurrentLevel", p.CurrentLevel],
  ];
  for (const [k, v] of rows) {
    const kk = document.createElement("div"); kk.className = "k"; kk.textContent = k;
    const vv = document.createElement("div"); vv.className = "v"; vv.textContent = String(v ?? "–");
    g.append(kk, vv);
  }
  $("#dw-raw").textContent = JSON.stringify(p, null, 2);
  $("#pl-drawer").classList.remove("hidden");
  refreshPlayers.__sel = p;
  if (invTimer) clearInterval(invTimer);
  loadInventory(p.SteamID);
  invTimer = setInterval(() => { if (plSel) loadInventory(plSel); }, 5000);
}

function invChip(it) {
  const amt = it.amount > 1 ? `<b>${it.amount}×</b> ` : "";
  const cond = it.cond != null && it.cond < 100 ? `<i class="cond">${it.cond}%</i>` : "";
  let h = `<div class="inv-item" title="${esc(it.shortname)}${it.cat ? " (" + esc(it.cat) + ")" : ""}">${amt}${esc(it.name)}${cond}</div>`;
  if (it.contents && it.contents.length) {
    h += `<div class="inv-sub">${it.contents.map(invChip).join("")}</div>`;
  }
  return h;
}

async function loadInventory(sid) {
  const el = $("#dw-inv");
  if (!el || plSel !== sid) return;
  const sel = selected();
  if (!sel) return;
  const q = `host=${sel.host.id}&server=${encodeURIComponent(sel.srv.id)}&sid=${encodeURIComponent(sid)}`;
  const { status, data } = await api(`/api/inventory?${q}`);
  if (plSel !== sid) return;   // drawer switched player while fetching
  if (status !== 200 || !data || !data.player) {
    el.innerHTML = `<span class="dim">${esc((data && data.error) || "no inventory data")}</span>`;
    $("#inv-updated").textContent = "";
    return;
  }
  const p = data.player;
  let meta = "";
  if (p.calories != null) meta += ` · 🔥 ${p.calories} · 💧 ${p.hydration}`;
  $("#inv-updated").textContent = meta;
  let html = "";
  for (const [key, label] of [["belt", "Belt"], ["wear", "Wear"], ["main", "Main"]]) {
    const items = p[key] || [];
    html += `<div class="inv-group"><div class="inv-label">${label} <span class="dim">${items.length}</span></div><div class="inv-items">`;
    html += items.length ? items.map(invChip).join("") : '<span class="dim">empty</span>';
    html += `</div></div>`;
  }
  el.innerHTML = html;
}

function closeDrawer() {
  $("#pl-drawer").classList.add("hidden");
  plSel = null;
  refreshPlayers.__sel = null;
  if (invTimer) { clearInterval(invTimer); invTimer = null; }
}

$("#dw-close").addEventListener("click", () => {
  closeDrawer();
  refreshPlayers();
});

// shared RCON runner for kick/ban/drawer actions (panel maps agent failures
// to 200 {ok:false} so they render inline)
async function rconCmd(cmd, showIn = null) {
  const sel = selected();
  if (!sel) {
    if (showIn) showIn.textContent = "[error] no server selected";
    return null;
  }
  const { status, data } = await api("/api/rcon", {
    method: "POST",
    body: { host: sel.host.id, server: sel.srv.id, cmd },
  });
  if (status === 200 && data.ok) {
    if (showIn) showIn.textContent = data.out || "(no output)";
    return data.out || "";
  }
  const err = (data && data.error) || "failed";
  if (showIn) showIn.textContent = "[error] " + err;
  toast(`"${cmd}": ${err}`, "bad");
  return null;
}

$("#dw-kick").addEventListener("click", async () => {
  const p = refreshPlayers.__sel;
  if (!p) return;
  if (!confirm(`Kick ${p.DisplayName}?`)) return;
  const out = await rconCmd(`kick ${p.SteamID}`);
  if (out !== null) toast(`kick sent: ${out || "(no output)"}`, "ok");
  refreshPlayers();
});
$("#dw-ban").addEventListener("click", async () => {
  const p = refreshPlayers.__sel;
  if (!p) return;
  const reason = prompt(`Ban ${p.DisplayName}? Optional reason:`, "banned via panel") || "banned";
  if (reason === null) return;
  const out = await rconCmd(`ban ${p.SteamID} ${reason}`);
  if (out !== null) toast(`ban sent: ${out || "(no output)"}`, "ok");
  refreshPlayers();
});
$$(".drawer-actions [data-cmd]").forEach((b) => b.addEventListener("click", async () => {
  const p = refreshPlayers.__sel;
  if (!p) return;
  $("#dw-raw").textContent = "running…";
  await rconCmd(b.dataset.cmd.replace("%STEAMID%", p.SteamID), $("#dw-raw"));
}));

$("#pl-refresh").addEventListener("click", refreshPlayers);
$("#pl-auto").addEventListener("change", setupPlTimer);
function setupPlTimer() {
  if (plTimer) { clearInterval(plTimer); plTimer = null; }
  const sel = selected();
  const canPoll = ACTIVE === "players" && $("#pl-auto").checked && sel && sel.srv.rcon !== false;
  if (canPoll) plTimer = setInterval(refreshPlayers, 4000);
}

/* ------------------------------------------------------------- console */
const hist = []; let histIdx = -1;

function conLine(text, cls) {
  const view = $("#conview");
  const d = document.createElement("div");
  d.className = "logline " + (cls || "out");
  d.textContent = text;
  view.appendChild(d);
  view.scrollTop = view.scrollHeight;
}

async function runCmd(cmd) {
  if (!cmd.trim()) return;
  conLine("› " + cmd, "cmd");
  const sel = selected();
  if (!sel) { conLine("[error] no server selected", "err"); return; }
  const { status, data } = await api("/api/rcon", {
    method: "POST",
    body: { host: sel.host.id, server: sel.srv.id, cmd },
  });
  if (status === 200 && data.ok) {
    const out = data.out || "(no output)";
    const lines = out.split("\n");
    const CAP = 600;   // big dumps like `find .` (~3500 lines) would choke the DOM
    if (lines.length > CAP) {
      lines.slice(0, 300).forEach((l) => conLine(l, "out"));
      conLine(`… ${lines.length - CAP} lines omitted (output ${out.length} chars) …`, "cmd");
      lines.slice(-299).forEach((l) => conLine(l, "out"));
    } else {
      lines.forEach((l) => conLine(l, "out"));
    }
  } else {
    conLine("[error] " + ((data && data.error) || "failed"), "err");
  }
}

$("#con-form").addEventListener("submit", (e) => {
  e.preventDefault();
  const inp = $("#con-input");
  const cmd = inp.value;
  if (!cmd.trim()) return;
  hist.push(cmd); histIdx = hist.length;
  inp.value = "";
  runCmd(cmd);
});
$("#con-input").addEventListener("keydown", (e) => {
  if (e.key === "ArrowUp") { if (histIdx > 0) { histIdx--; e.target.value = hist[histIdx]; e.preventDefault(); } }
  else if (e.key === "ArrowDown") { if (histIdx < hist.length - 1) { histIdx++; e.target.value = hist[histIdx]; } else { histIdx = hist.length; e.target.value = ""; } e.preventDefault(); }
});
$$(".chip[data-cmd]").forEach((c) => c.addEventListener("click", () => runCmd(c.dataset.cmd)));
$$(".chip[data-prompt]").forEach((c) => c.addEventListener("click", () => {
  const pat = prompt(`${c.dataset.prompt} pattern (e.g. oxide, server, damage):`, "");
  if (pat && pat.trim()) runCmd(`${c.dataset.prompt} ${pat.trim()}`);
}));
$("#con-clear").addEventListener("click", () => { $("#conview").innerHTML = ""; });

/* ------------------------------------------------------------- hosts */
let editId = null;   // null = add

function renderHosts() {
  const grid = $("#host-cards");
  if (!OVERVIEW.hosts.length) {
    grid.innerHTML = '<div class="dim center" style="padding:20px">no hosts yet — click “add host” and paste the token from the agent’s startup log</div>';
    return;
  }
  grid.innerHTML = "";
  for (const h of OVERVIEW.hosts) {
    const st = h.ok === true ? '<span class="pill green">● agent ok</span>'
      : h.ok === false ? '<span class="pill red">✖ unreachable</span>'
      : '<span class="pill grey">○ disabled</span>';
    const el = document.createElement("div");
    el.className = "host-card";
    el.innerHTML = `
      <div class="hc-head"><b>${esc(h.name)}</b> ${st}</div>
      <div class="hc-url dim">${esc(h.base_url)}</div>
      <div class="hc-meta dim">${h.servers.length} container${h.servers.length === 1 ? "" : "s"}` +
      (h.ok === false && h.error ? `<br>error: ${esc(h.error)}` : "") + `</div>
      <div class="hc-actions">
        <button class="btn sm" data-a="test">test</button>
        <button class="btn sm" data-a="edit">edit</button>
        <button class="btn sm danger" data-a="delete">delete</button>
      </div>`;
    el.querySelector('[data-a="test"]').addEventListener("click", async () => {
      const { status, data } = await api("/api/hosts/test", { method: "POST", body: { id: h.id } });
      if (status === 200 && data.ok) {
        toast(`${h.name}: agent v${data.ping.version} · docker ${data.system.docker.version} · ${data.system.host.ncpu} cpu ok`, "ok");
      } else {
        toast(`${h.name}: ${(data && data.error) || "test failed"}`, "bad");
      }
    });
    el.querySelector('[data-a="edit"]').addEventListener("click", () => openHostModal(h));
    el.querySelector('[data-a="delete"]').addEventListener("click", async () => {
      if (!confirm(`Delete host "${h.name}"? The agent keeps running; only this panel forgets it.`)) return;
      const { status, data } = await api(`/api/hosts/${h.id}`, { method: "DELETE" });
      if (status === 200) { toast(`host ${h.name} removed`, "ok"); pollOverview(); }
      else toast((data && data.error) || "delete failed", "bad");
    });
    grid.appendChild(el);
  }
}

function openHostModal(h) {
  editId = h ? h.id : null;
  $("#host-modal-title").textContent = h ? `edit host: ${h.name}` : "add host";
  $("#hf-name").value = h ? h.name : "";
  $("#hf-url").value = h ? h.base_url : "";
  $("#hf-token").value = "";
  $("#hf-token").placeholder = h ? "•••• (leave blank to keep)" : "from the agent's startup log";
  $("#hf-enabled").checked = h ? h.enabled : true;
  $("#host-err").textContent = "";
  $("#host-test-out").textContent = "";
  $("#host-modal").classList.remove("hidden");
  $("#hf-name").focus();
}

$("#btn-add-host").addEventListener("click", () => openHostModal(null));
$("#hf-cancel").addEventListener("click", () => $("#host-modal").classList.add("hidden"));

$("#hf-test").addEventListener("click", async () => {
  const body = editId !== null && !$("#hf-token").value
    ? { id: editId }
    : { base_url: $("#hf-url").value.trim(), token: $("#hf-token").value.trim() };
  if (!body.id && (!body.base_url || !body.token)) {
    $("#host-test-out").textContent = "url + token required to test";
    return;
  }
  $("#host-test-out").textContent = "testing…";
  const { status, data } = await api("/api/hosts/test", { method: "POST", body });
  if (status === 200 && data.ok) {
    $("#host-test-out").textContent =
      `✓ agent v${data.ping.version} · docker ${data.system.docker.version} · ` +
      `${data.system.host.ncpu} cpu · ${data.system.host.containers.running} running`;
  } else {
    $("#host-test-out").textContent = "✗ " + ((data && data.error) || "failed");
  }
});

$("#host-form").addEventListener("submit", async (e) => {
  e.preventDefault();
  $("#host-err").textContent = "";
  const body = {
    name: $("#hf-name").value.trim(),
    base_url: $("#hf-url").value.trim(),
    enabled: $("#hf-enabled").checked,
  };
  const tok = $("#hf-token").value.trim();
  if (tok) body.token = tok;
  let status, data;
  if (editId !== null) {
    ({ status, data } = await api(`/api/hosts/${editId}`, { method: "PUT", body }));
  } else {
    ({ status, data } = await api("/api/hosts", { method: "POST", body }));
  }
  if (status === 200 || status === 201) {
    $("#host-modal").classList.add("hidden");
    toast(editId !== null ? "host updated" : "host added", "ok");
    pollOverview();
  } else {
    $("#host-err").textContent = (data && data.error) || "save failed";
  }
});

/* ------------------------------------------------------------- boot */
function stopPoll() {
  if (pollTimer) { clearInterval(pollTimer); pollTimer = null; }
}

async function boot() {
  await pollOverview();   // first render: picks CUR (or restores it) → starts the stream
  stopPoll();
  pollTimer = setInterval(pollOverview, 5000);
}

(async function init() {
  const { status, data } = await api("/api/me");
  if (status === 200 && data.auth) hideLogin(), boot();
  else showLogin();
})();
