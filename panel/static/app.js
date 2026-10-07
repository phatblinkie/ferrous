/* ferrous panel — vanilla JS */
"use strict";

const $ = (s) => document.querySelector(s);
const $$ = (s) => [...document.querySelectorAll(s)];

let OVERVIEW = { hosts: [] };   // last /api/overview payload
let pollTimer = null;
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

/* ------------------------------------------------------------- auth */
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
  closeConsole();
  showLogin();
});

$$(".tab").forEach((b) => b.addEventListener("click", () => {
  $$(".tab").forEach((x) => x.classList.toggle("active", x === b));
  $$(".tabpane").forEach((p) => p.classList.toggle("active", p.id === "tab-" + b.dataset.tab));
}));

$("#btn-refresh").addEventListener("click", () => pollOverview());

/* ------------------------------------------------------------- overview */
async function pollOverview() {
  const { status, data } = await api("/api/overview");
  if (status !== 200 || !data) return;
  OVERVIEW = data;
  renderHeader();
  renderHostFilter();
  renderServers();
  renderHosts();
  $("#srv-updated").textContent = "updated " + new Date().toLocaleTimeString();
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
      rows.push(`<tr data-h="${h.id}" data-s="${esc(s.id)}">
        <td><b>${esc(s.name || s.short_id)}</b>${s.managed ? "" : ' <span class="dim">(foreign)</span>'}</td>
        <td class="dim">${esc(h.name)}</td>
        <td class="dim">${esc(s.image)}</td>
        <td>${statePill(s.state)}</td>
        <td class="dim">${esc(s.status)}</td>
        <td class="pwr">
          <button class="btn sm" data-act="start" ${run ? "disabled" : ""} title="start">▶</button>
          <button class="btn sm warn" data-act="stop" ${run ? "" : "disabled"} title="stop">■</button>
          <button class="btn sm accent" data-act="restart" ${run ? "" : "disabled"} title="restart">↻</button>
        </td>
        <td><button class="btn sm con">console</button></td>
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
    tr.querySelector(".con").addEventListener("click", () => openConsole(host, srv));
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

/* ------------------------------------------------------------- console */
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

let CON = null;  // {host, srv}

function openConsole(host, srv) {
  if (!host || !srv) return;
  CON = { host, srv };
  $("#console-panel").classList.remove("hidden");
  $("#con-title").textContent = `${srv.name} @ ${host.name}`;
  logBuf = []; $("#logview").innerHTML = ""; $("#log-count").textContent = "0 lines";
  startStream();
}

function closeConsole() {
  if (es) { es.close(); es = null; }
  CON = null;
  $("#console-panel").classList.add("hidden");
}

function startStream() {
  if (es) es.close();
  if (!CON) return;
  const url = `/api/logs?host=${CON.host.id}&server=${encodeURIComponent(CON.srv.id)}&tail=200&follow=1`;
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
$("#con-close").addEventListener("click", closeConsole);
$("#host-filter").addEventListener("change", renderServers);

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
  await pollOverview();
  stopPoll();
  pollTimer = setInterval(pollOverview, 5000);
}

(async function init() {
  const { status, data } = await api("/api/me");
  if (status === 200 && data.auth) hideLogin(), boot();
  else showLogin();
})();
