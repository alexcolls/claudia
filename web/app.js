"use strict";
// Agent output and tool input are untrusted, since a prompt-injected agent writes
// them. Everything reaches the DOM through textContent or attributes set in h().
// Never use innerHTML here.

const app = document.getElementById("app");

function h(tag, attrs, ...kids) {
  const el = document.createElement(tag);
  fill(el, attrs, kids);
  return el;
}

function s(tag, attrs, ...kids) {
  const el = document.createElementNS("http://www.w3.org/2000/svg", tag);
  fill(el, attrs, kids);
  return el;
}

function fill(el, attrs, kids) {
  for (const [k, v] of Object.entries(attrs || {})) {
    if (v === undefined || v === null || v === false) continue;
    if (k.startsWith("on")) el.addEventListener(k.slice(2), v);
    else if (k === "value" || k === "checked" || k === "disabled" || k === "selected") el[k] = v;
    else el.setAttribute(k === "class" ? "class" : k, v === true ? "" : v);
  }
  for (const kid of kids.flat(Infinity)) {
    if (kid === null || kid === undefined || kid === false) continue;
    el.append(kid.nodeType ? kid : document.createTextNode(String(kid)));
  }
}

function clear(el, ...kids) {
  el.replaceChildren();
  fill(el, null, kids);
}

async function api(path, opts = {}) {
  const init = { method: opts.method || "GET", headers: {} };
  if (opts.body !== undefined) {
    init.headers["Content-Type"] = "application/json";
    init.body = JSON.stringify(opts.body);
  }
  const res = await fetch("/api" + path, init);
  let data = null;
  try { data = await res.json(); } catch { /* an empty or non-JSON body */ }
  if (res.status === 401 && path !== "/login") {
    showLogin();
    throw new Error("Please log in");
  }
  if (!res.ok) throw new Error((data && data.error) || "Request failed (" + res.status + ")");
  return data;
}

let toastTimer;
function toast(msg) {
  let t = document.getElementById("toast");
  if (!t) {
    t = h("div", { id: "toast", role: "alert" });
    document.body.append(t);
  }
  t.textContent = msg;
  clearTimeout(toastTimer);
  toastTimer = setTimeout(() => t.remove(), 6000);
}

// act wraps a button action: reports failures and stops double taps.
function act(fn) {
  return async (ev) => {
    const b = ev && ev.currentTarget;
    if (b && b.disabled) return;
    if (b && b.tagName === "BUTTON") b.disabled = true;
    try { await fn(ev); } catch (e) { if (e.message !== "Please log in") toast(e.message); }
    if (b && b.tagName === "BUTTON") b.disabled = false;
  };
}

// armed is a two-tap button for anything hard to undo.
function armed(label, sure, fn, cls) {
  let timer;
  const b = h("button", { type: "button", class: cls || "danger" }, label);
  b.addEventListener("click", act(async () => {
    if (!b.classList.contains("armed")) {
      b.classList.add("armed");
      b.textContent = sure;
      timer = setTimeout(() => { b.classList.remove("armed"); b.textContent = label; }, 4000);
      return;
    }
    clearTimeout(timer);
    b.classList.remove("armed");
    b.textContent = label;
    await fn();
  }));
  return b;
}

function usd(n) {
  n = Number(n) || 0;
  if (n === 0) return "$0";
  if (Math.abs(n) >= 1) return "$" + n.toLocaleString(undefined, { minimumFractionDigits: 2, maximumFractionDigits: 2 });
  if (Math.abs(n) >= 0.01) return "$" + n.toFixed(2);
  return "$" + n.toFixed(4);
}
function count(n) {
  n = Number(n) || 0;
  if (n >= 1e6) return (n / 1e6).toFixed(1) + "M";
  if (n >= 10000) return (n / 1e3).toFixed(1) + "K";
  return n.toLocaleString();
}
const words = (t) => String(t || "").replaceAll("_", " ");
const when = (ts) => (ts ? new Date(ts * 1000).toLocaleString(undefined, { dateStyle: "short", timeStyle: "short" }) : "");

function field(label, control, hint) {
  return h("label", null, label, control, hint ? h("span", { class: "hint" }, hint) : null);
}

function select(name, options, value) {
  return h("select", { name }, options.map(([v, text]) => h("option", { value: v, selected: v === value }, text)));
}

function pill(text, kind) {
  return h("span", { class: "pill " + (kind || "") }, words(text));
}

// ---- shell

const CLI_NAMES = { claude: "Claude Code", codex: "Codex", agy: "Antigravity (experimental)" };
const MODE_NAMES = {
  ask: "Ask first: you approve each risky action",
  reviewed: "Reviewed: the CLI's classifier decides",
  allowlist: "Allowlist: only listed tools run",
  full: "Full auto: no checks inside the container",
};

let view = null;        // the current page's { refresh, destroy }
let appState = null;
let feed = null;        // the state stream
let refreshTimer = null;
let badgeEl = null;

const routes = [
  [/^#\/agents\/(\d+)$/, (m) => agentPage(Number(m[1]))],
  [/^#\/queue(?:\/\d+)?$/, () => queuePage()],
  [/^#\/approvals(?:\/(\d+))?$/, (m) => approvalsPage(m[1] ? Number(m[1]) : 0)],
  [/^#\/costs$/, () => costsPage()],
  [/^#\/settings$/, () => settingsPage()],
  [/^/, () => fleetPage()],
];

const TABS = [["#/", "Agents"], ["#/queue", "Queue"], ["#/approvals", "Approvals"], ["#/costs", "Costs"], ["#/settings", "Settings"]];

function route() {
  if (view && view.destroy) view.destroy();
  view = null;
  const hash = location.hash || "#/";
  const main = h("main");
  badgeEl = h("span", { class: "badge", hidden: true });
  const nav = h("nav", { class: "tabs", "aria-label": "Main" }, TABS.map(([href, label]) => {
    const on = href === "#/" ? hash === "#/" || hash.startsWith("#/agents") : hash.startsWith(href);
    return h("a", { href, "aria-current": on ? "page" : null }, label, label === "Approvals" ? badgeEl : null);
  }));
  const top = h("header", { class: "top" }, h("h1", null, "Claudia"), h("span", { class: "spacer" }),
    h("button", { type: "button", onclick: act(async () => { await api("/logout", { method: "POST" }); showLogin(); }) }, "Log out"));
  clear(app, top, nav, main);
  for (const [re, make] of routes) {
    const m = hash.match(re);
    if (m) { view = make(m, main) || {}; mount(main, view); break; }
  }
  refreshState();
}

function mount(main, v) {
  if (v.el) clear(main, v.el);
  if (v.refresh) v.refresh().catch((e) => { if (e.message !== "Please log in") toast(e.message); });
}

async function refreshState() {
  try {
    appState = await api("/state");
  } catch { return; }
  if (badgeEl) {
    badgeEl.textContent = appState.pending_approvals;
    badgeEl.hidden = !appState.pending_approvals;
  }
}

// One stream tells every page that something changed.
function connectStateFeed() {
  if (feed) return;
  feed = new EventSource("/api/events");
  feed.onmessage = () => {
    clearTimeout(refreshTimer);
    refreshTimer = setTimeout(() => {
      refreshState();
      if (view && view.refresh) view.refresh().catch(() => {});
    }, 250);
  };
}

function showLogin() {
  if (view && view.destroy) view.destroy();
  view = null;
  if (feed) { feed.close(); feed = null; }
  const pw = h("input", { type: "password", name: "password", autocomplete: "current-password", required: true });
  const form = h("form", { class: "card login", onsubmit: (e) => { e.preventDefault(); login(); } },
    h("h1", null, "Claudia"), field("Password", pw), h("button", { class: "primary", type: "submit" }, "Log in"));
  const login = act(async () => {
    await api("/login", { method: "POST", body: { password: pw.value } });
    connectStateFeed();
    route();
  });
  clear(app, form);
  pw.focus();
}

// ---- fleet

function agentStatus(ag) {
  if (ag.pause_reason) return pill("paused", "warn");
  if (ag.running) return pill("running", "busy");
  return pill("idle", "good");
}

function fleetPage() {
  const list = h("div", { style: "display:flex;flex-direction:column;gap:8px" });
  const banners = h("div", { style: "display:contents" });
  const formBox = h("details", { class: "card" }, h("summary", null, "New agent"));
  let formBuilt = false;
  formBox.addEventListener("toggle", () => {
    if (formBox.open && !formBuilt) { formBuilt = true; formBox.append(agentForm()); }
  });
  const el = h("div", { style: "display:contents" },
    h("div", { class: "row between" }, h("h2", null, "Agents"),
      armed("Stop all", "Tap again to stop all", async () => { await api("/stop-all", { method: "POST" }); toast("Stopping all agents"); })),
    banners, list, formBox);
  return {
    el,
    async refresh() {
      const st = await api("/state");
      clear(banners, st.dispatch_block ? h("div", { class: "banner" }, st.dispatch_block) : null);
      if (!st.agents.length) {
        clear(list, h("div", { class: "card muted" }, "No agents yet. Create one below. For an API key, add a credential in Settings first."));
        return;
      }
      clear(list, st.agents.map((ag) => h("a", { class: "list-link", href: "#/agents/" + ag.id },
        h("div", { class: "card" },
          h("div", { class: "row between" }, h("strong", { class: "break" }, ag.name), agentStatus(ag)),
          h("div", { class: "small muted" }, [CLI_NAMES[ag.cli] || ag.cli, ag.model, words(ag.mode), ag.credential_id ? ag.preset : (ag.account || "subscription, not signed in")].filter(Boolean).join(" · ")),
          ag.pause_reason ? h("div", { class: "small" }, ag.pause_reason) : null))));
    },
  };
}

function agentForm(existing, onDone) {
  const ag = existing || {};
  const editing = !!existing;
  const name = h("input", { name: "name", value: ag.name || "", required: true });
  const cli = select("cli", Object.entries(CLI_NAMES), ag.cli || "claude");
  cli.disabled = editing;
  const cred = h("select", { name: "credential" });
  const model = h("input", { name: "model", value: ag.model || "", autocapitalize: "off", spellcheck: "false" });
  const mode = h("select", { name: "mode" });
  const tools = h("input", { name: "tools", value: ag.allowed_tools || "", placeholder: "Read,Edit,Bash(git status)", autocapitalize: "off" });
  const toolsField = field("Allowed tools (comma-separated)", tools);
  const maxRun = h("input", { type: "number", min: "0", step: "0.01", inputmode: "decimal", value: ag.max_run_usd || "" });
  const daily = h("input", { type: "number", min: "0", step: "0.01", inputmode: "decimal", value: ag.daily_budget_usd || "" });
  const limit = h("input", { type: "number", min: "1", inputmode: "numeric", value: ag.run_limit_sec ? Math.round(ag.run_limit_sec / 60) : 60 });
  const queue = h("input", { type: "checkbox", checked: editing ? ag.accepts_queue : true });
  const note = h("p", { class: "hint" });
  let presets = [], creds = [];

  async function refreshModes() {
    const c = creds.find((x) => String(x.id) === cred.value);
    const list = await api("/modes?cli=" + encodeURIComponent(cli.value) + "&preset=" + encodeURIComponent(c ? c.preset : ""));
    // Default to the safest mode the CLI offers. Modes are listed safest first.
    const wanted = mode.value || ag.mode;
    const keep = list.includes(wanted) ? wanted : list[0];
    clear(mode, list.map((m) => h("option", { value: m, selected: m === keep }, MODE_NAMES[m] || m)));
    toolsField.hidden = mode.value !== "allowlist";
    tools.placeholder = cli.value === "agy" ? "command(git status),command(npm test)" : "Read,Edit,Bash(git status)";
    const p = c && presets.find((x) => x.key === c.preset);
    let text = p && p.note ? p.note : "";
    if (!c && cli.value === "claude") {
      text = "Subscription: after creating the agent, sign in from its Terminal tab. Each agent keeps its own login, so add one agent per account. Plan limits assume ordinary individual use, so this agent stays out of the queue by default, and costs aren't measured in USD.";
    }
    note.textContent = text;
    if (!editing && !c) queue.checked = false;
    if (!editing && c) queue.checked = true;
  }
  function fillCreds() {
    const matching = creds.filter((c) => { const p = presets.find((x) => x.key === c.preset); return p && p.cli === cli.value; });
    const opts = matching.map((c) => h("option", { value: String(c.id), selected: c.id === ag.credential_id }, c.name));
    if (cli.value === "claude") opts.push(h("option", { value: "", selected: editing && !ag.credential_id }, "Claude subscription (sign in from the terminal)"));
    clear(cred, opts);
  }
  cli.addEventListener("change", act(async () => { clear(mode); fillCreds(); await refreshModes(); }));
  cred.addEventListener("change", act(refreshModes));
  mode.addEventListener("change", () => { toolsField.hidden = mode.value !== "allowlist"; });
  (async () => {
    try {
      [presets, creds] = await Promise.all([api("/presets"), api("/credentials")]);
      fillCreds();
      await refreshModes();
    } catch (e) { toast(e.message); }
  })();

  return h("form", { style: "display:flex;flex-direction:column;gap:10px", onsubmit: (e) => {
    e.preventDefault();
    act(async () => {
      const body = {
        name: name.value, cli: cli.value, credential_id: Number(cred.value) || 0, model: model.value, mode: mode.value,
        allowed_tools: tools.value, max_run_usd: Number(maxRun.value) || 0, daily_budget_usd: Number(daily.value) || 0,
        run_limit_sec: Math.round((Number(limit.value) || 60) * 60), accepts_queue: queue.checked,
      };
      const saved = await api(editing ? "/agents/" + ag.id : "/agents", { method: editing ? "PUT" : "POST", body });
      toast(editing ? "Saved" : "Agent created");
      if (onDone) onDone(saved); else location.hash = "#/agents/" + saved.id;
    })();
  } },
    h("div", { class: "fields" }, field("Name", name), field("CLI", cli), field("Credential", cred), field("Model", model, "Required for Codex, Antigravity and gateways, so usage can be priced.")),
    field("Permissions", mode), toolsField, note,
    h("div", { class: "fields" }, field("Per-run cap (USD)", maxRun, "Enforced by Claude Code. Leave empty for none."), field("Daily budget (USD)", daily), field("Run time limit (minutes)", limit)),
    h("label", { class: "check" }, queue, "Takes tasks from the queue"),
    h("div", null, h("button", { class: "primary", type: "submit" }, editing ? "Save" : "Create agent")));
}

// ---- agent page

function agentPage(id) {
  const head = h("div", { class: "card" });
  const body = h("div", { style: "display:contents" });
  const tabButtons = {};
  let tab = "chat", es = null, term = null, ag = null, feedView = null, composerStop = null;

  function setTab(next) {
    tab = next;
    for (const [k, b] of Object.entries(tabButtons)) b.setAttribute("aria-selected", String(k === tab));
    teardown();
    if (tab === "chat") showChat();
    if (tab === "terminal") showTerminal();
    if (tab === "settings") showSettings();
  }
  function teardown() {
    if (es) { es.close(); es = null; }
    if (term) { term.dispose(); term = null; }
  }
  for (const [k, label] of [["chat", "Chat"], ["terminal", "Terminal"], ["settings", "Settings"]]) {
    tabButtons[k] = h("button", { type: "button", role: "tab", "aria-selected": String(k === tab), onclick: () => setTab(k) }, label);
  }

  function showChat() {
    const log = h("div", { class: "feed", "aria-live": "polite" });
    const input = h("textarea", { rows: 2, placeholder: "Message this agent", "aria-label": "Message" });
    const send = act(async () => {
      const text = input.value.trim();
      if (!text) return;
      await api("/agents/" + id + "/messages", { method: "POST", body: { text } });
      input.value = "";
    });
    composerStop = h("button", { type: "button", class: "danger", hidden: true, onclick: act(async () => { await api("/agents/" + id + "/stop", { method: "POST" }); toast("Stopping"); }) }, "Stop");
    const composer = h("form", { class: "composer", onsubmit: (e) => { e.preventDefault(); send(e); } }, input, composerStop,
      h("button", { class: "primary", type: "submit" }, "Send"));
    input.addEventListener("keydown", (e) => { if (e.key === "Enter" && (e.metaKey || e.ctrlKey)) { e.preventDefault(); send(e); } });
    clear(body, log, composer);
    feedView = newFeed(log);
    es = new EventSource("/api/events?agent=" + id);
    es.onmessage = (e) => {
      const m = JSON.parse(e.data);
      if (m.data && m.data.kind === "state") return;
      feedView.add(m.data);
    };
    syncRunning();
  }

  function showTerminal() {
    clear(body, h("div", { class: "muted" }, "Loading the terminal..."));
    loadXterm().then(() => { if (tab === "terminal") openTerminal(); }).catch((e) => toast(e.message));
  }

  function openTerminal() {
    const box = h("div");
    const wrap = h("div", { class: "term-wrap" }, box);
    const status = h("span", { class: "small muted" }, "Connecting...");
    const send = (data) => {
      if (!ws || ws.readyState !== 1) return false;
      ws.send(new TextEncoder().encode(data));
      return true;
    };
    // Phones have no Esc or Ctrl key. Above the terminal, since an on-screen keyboard
    // covers whatever is below it.
    const keys = h("div", { class: "keys" }, [["Esc", "\x1b"], ["Tab", "\t"], ["Ctrl-C", "\x03"], ["Ctrl-D", "\x04"], ["Up", "\x1b[A"], ["Down", "\x1b[B"], ["Enter", "\r"]]
      .map(([label, seq]) => h("button", { type: "button", onclick: () => { send(seq); t.focus(); } }, label)));
    let ws = null;
    const reconnect = h("button", { type: "button", hidden: true, onclick: () => connect() }, "Reconnect");
    const signin = ag && !ag.credential_id ? signinBox(send) : null;
    clear(body, signin && signin.el,
      h("div", { class: "row between" }, status, reconnect), keys, wrap,
      h("p", { class: "hint" }, "The session runs in tmux, so it keeps going if you lose connection. API keys aren't in this shell."));
    const t = new window.Terminal({ convertEol: false, fontSize: 13, cursorBlink: true, scrollback: 5000 });
    const fit = new window.FitAddon.FitAddon();
    t.loadAddon(fit);
    t.open(box);
    term = t;
    const sendSize = () => { if (ws && ws.readyState === 1) ws.send(JSON.stringify({ cols: t.cols, rows: t.rows })); };
    const onResize = () => { try { fit.fit(); sendSize(); } catch { /* not visible right now */ } };
    window.addEventListener("resize", onResize);
    const dispose = t.dispose.bind(t);
    t.dispose = () => { window.removeEventListener("resize", onResize); if (signin) signin.stop(); if (ws) ws.close(); dispose(); };
    t.onData(send);
    function connect() {
      reconnect.hidden = true;
      status.textContent = "Connecting...";
      ws = new WebSocket((location.protocol === "https:" ? "wss://" : "ws://") + location.host + "/api/agents/" + id + "/terminal");
      ws.binaryType = "arraybuffer";
      ws.onopen = () => { status.textContent = "Connected"; onResize(); t.focus(); };
      ws.onmessage = (e) => { t.write(new Uint8Array(e.data)); };
      ws.onclose = () => { if (term) { status.textContent = "Disconnected. The session is still running."; reconnect.hidden = false; } };
    }
    onResize();
    connect();
  }

  // signinBox helps a subscription agent sign in. The CLI in the terminal runs the
  // sign-in, on Anthropic's own page. This makes its long link tappable and gives phones
  // somewhere to paste the code. The code only goes to the terminal, and isn't kept.
  function signinBox(send) {
    const box = h("div", { class: "banner", style: "display:flex;flex-direction:column;gap:8px" });
    const code = h("input", { "aria-label": "Sign-in code", placeholder: "Paste the code here", autocomplete: "off", autocapitalize: "off", spellcheck: "false" });
    const pasted = h("form", { class: "row", onsubmit: (e) => {
      e.preventDefault();
      if (!code.value.trim()) return;
      if (!send(code.value.trim() + "\r")) { toast("The terminal isn't connected. Try again in a moment."); return; }
      code.value = "";
      toast("Code sent. Watch the terminal below.");
    } }, h("span", { class: "grow" }, code), h("button", { class: "primary", type: "submit" }, "Send code"));
    let timer = null, shownUrl = null;
    const stop = () => { clearInterval(timer); timer = null; };
    const poll = () => { if (!timer) timer = setInterval(check, 2000); };
    function start() {
      // Ctrl-C first, in case an earlier sign-in is still waiting for its code. Without
      // the pause, the waiting CLI could read the command as the code.
      if (!send("\x03")) { toast("The terminal is still connecting. Try again in a moment."); return; }
      setTimeout(() => send("clear && claude auth login\r"), 500);
      shownUrl = null;
      clear(box, h("span", null, "Starting the sign-in..."));
      poll();
    }
    async function check() {
      let st;
      try { st = await api("/agents/" + id + "/signin"); } catch { return; }
      if (st.account) {
        stop();
        shownUrl = null;
        clear(box, h("span", null, "Signed in as " + st.account + "."),
          h("span", { class: "hint" }, "To change account, run claude auth logout here, then sign in again. Use a separate agent for each account you want to keep."));
        if (st.account !== ag.account) refreshHead();
        return;
      }
      if (st.url) {
        poll(); // a sign-in left waiting on an earlier visit still needs watching
        if (st.url === shownUrl) return; // redrawing would wipe a half-pasted code
        shownUrl = st.url;
        clear(box, h("a", { href: st.url, target: "_blank", rel: "noopener noreferrer" }, "Open the Claude sign-in page"), pasted,
          h("span", { class: "hint" }, "Sign in there with the account for this agent, then paste the code it gives you."),
          h("div", null, h("button", { type: "button", onclick: start }, "Start again")));
      } else if (!timer) {
        clear(box, h("span", null, "This agent isn't signed in to a Claude account."),
          h("div", null, h("button", { type: "button", class: "primary", onclick: start }, "Sign in")));
      }
    }
    check();
    return { el: box, stop };
  }

  function showSettings() {
    if (!ag) return;
    clear(body, h("div", { class: "card" },
      agentForm(ag, (saved) => { ag = saved; refreshHead(); }),
      h("div", { class: "row" },
        h("button", { type: "button", onclick: act(async () => { await api("/agents/" + id + "/reset", { method: "POST" }); toast("The next message starts a new conversation"); }) }, "New conversation"),
        armed("Delete agent", "Tap again: deletes its files too", async () => { await api("/agents/" + id, { method: "DELETE" }); location.hash = "#/"; }))));
  }

  function syncRunning() {
    if (composerStop && ag) composerStop.hidden = !ag.running;
  }

  async function refreshHead() {
    ag = await api("/agents/" + id);
    clear(head,
      h("div", { class: "row between" }, h("h2", { class: "break" }, ag.name), agentStatus(ag)),
      h("div", { class: "small muted" }, [CLI_NAMES[ag.cli] || ag.cli, ag.model, words(ag.mode), ag.credential_id ? "" : (ag.account || "not signed in")].filter(Boolean).join(" · ")),
      ag.pause_reason ? h("div", { class: "row between" }, h("span", { class: "small break grow" }, ag.pause_reason),
        h("button", { type: "button", onclick: act(async () => { await api("/agents/" + id + "/unpause", { method: "POST" }); refreshHead(); }) }, "Resume")) : null);
    syncRunning();
  }

  const el = h("div", { style: "display:contents" }, head, h("div", { class: "subtabs", role: "tablist" }, Object.values(tabButtons)), body);
  let started = false;
  return {
    el,
    async refresh() {
      await refreshHead();
      // A new subscription agent can't do anything until it signs in.
      if (!started) { started = true; setTab(!ag.credential_id && !ag.account ? "terminal" : "chat"); }
    },
    destroy: teardown,
  };
}

// newFeed renders normalized events. Deltas build a live bubble that the finished
// message replaces.
function newFeed(log) {
  let live = null;
  const tools = new Map();
  const follow = () => {
    if (window.innerHeight + window.scrollY > document.body.offsetHeight - 240) window.scrollTo(0, document.body.scrollHeight);
  };
  return {
    add(ev) {
      if (!ev || !ev.kind) return;
      switch (ev.kind) {
        case "delta":
          if (!live) { live = h("div", { class: "msg text live" }); log.append(live); }
          live.append(document.createTextNode(ev.text));
          break;
        case "text":
          if (live) { live.remove(); live = null; }
          log.append(h("div", { class: "msg text" }, ev.text));
          break;
        case "user":
          log.append(h("div", { class: "msg user" }, ev.text));
          break;
        case "tool_call": {
          const body = h("pre", { class: "mono" }, prettyInput(ev.input));
          const box = h("details", { class: "tool" }, h("summary", null, ev.tool || "tool"), body);
          if (ev.tool_id) tools.set(ev.tool_id, box);
          log.append(box);
          break;
        }
        case "tool_result": {
          const box = tools.get(ev.tool_id);
          if (ev.is_error && box) box.classList.add("err");
          if (ev.text) {
            const out = h("pre", { class: "mono" }, ev.text);
            if (box) box.append(out);
            else log.append(h("details", { class: "tool" }, h("summary", null, "Tool output"), out));
          }
          break;
        }
        case "result":
          if (live) { live.remove(); live = null; }
          // A success repeats the last message. Failures also get an error event.
          break;
        case "error":
          if (live) { live.classList.remove("live"); live = null; }
          log.append(h("div", { class: "msg error" }, ev.text));
          break;
        default:
          log.append(h("div", { class: "msg status" }, ev.text));
      }
      follow();
    },
  };
}

function prettyInput(raw) {
  if (!raw) return "";
  try { return JSON.stringify(JSON.parse(raw), null, 2); } catch { return raw; }
}

let xtermLoading = null;
function loadXterm() {
  if (!xtermLoading) {
    xtermLoading = new Promise((resolve, reject) => {
      document.head.append(h("link", { rel: "stylesheet", href: "vendor/xterm.css" }));
      const load = (src) => new Promise((ok, no) => document.head.append(h("script", { src, onload: ok, onerror: () => no(new Error("Couldn't load the terminal")) })));
      load("vendor/xterm.js").then(() => load("vendor/addon-fit.js")).then(resolve, reject);
    });
  }
  return xtermLoading;
}

// ---- queue

const TASK_KIND = { done: "good", running: "busy", failed: "bad", no_changes: "warn", blocked: "warn", waiting: "", queued: "" };

function queuePage() {
  const list = h("div", { style: "display:flex;flex-direction:column;gap:8px" });
  const banner = h("div", { style: "display:contents" });
  const pauseBtn = h("button", { type: "button" }, "Pause queue");
  let paused = false, tasks = [], refresh;
  pauseBtn.addEventListener("click", act(async () => { await api("/queue", { method: "POST", body: { paused: !paused } }); await refresh(); }));

  const formBox = h("details", { class: "card" }, h("summary", null, "New task"));
  let built = false, depsBox = null;
  formBox.addEventListener("toggle", () => { if (formBox.open && !built) { built = true; depsBox = h("div", { class: "fields" }); taskForm(formBox, depsBox, () => tasks, () => refresh()); } });

  refresh = async () => {
    const data = await api("/tasks");
    tasks = data.tasks;
    paused = data.paused;
    pauseBtn.textContent = paused ? "Resume queue" : "Pause queue";
    clear(banner, data.block ? h("div", { class: "banner" }, data.block) : null);
    if (depsBox) fillDeps(depsBox, tasks);
    if (!tasks.length) { clear(list, h("div", { class: "card muted" }, "No tasks yet.")); return; }
    clear(list, tasks.map((t) => h("div", { class: "card" },
      h("div", { class: "row between" }, h("strong", { class: "break grow" }, "#" + t.id + " " + t.title), pill(t.view, TASK_KIND[t.view])),
      h("div", { class: "small muted break" }, t.repo + " · " + (t.head_sha ? t.branch + " @ " + t.head_sha.slice(0, 10) : "from " + t.base_branch)),
      t.deps.length ? h("div", { class: "small muted" }, "Depends on " + t.deps.map((d) => "#" + d).join(", ")) : null,
      t.note ? h("div", { class: "small" }, t.note) : null,
      t.error ? h("div", { class: "small break" }, t.error) : null,
      h("div", { class: "row" }, h("span", { class: "small muted grow" }, "Attempt " + t.attempts + " of " + t.max_attempts),
        t.status === "failed" || t.status === "no_changes" ? h("button", { type: "button", onclick: act(async () => { await api("/tasks/" + t.id + "/retry", { method: "POST" }); await refresh(); }) }, "Retry") : null,
        t.status !== "running" ? armed("Delete", "Tap again", async () => { await api("/tasks/" + t.id, { method: "DELETE" }); await refresh(); }) : null))));
  };
  return { el: h("div", { style: "display:contents" }, h("div", { class: "row between" }, h("h2", null, "Queue"), pauseBtn), banner, list, formBox), refresh };
}

function fillDeps(box, tasks) {
  const chosen = new Set([...box.querySelectorAll("input:checked")].map((i) => i.value));
  clear(box, tasks.length ? tasks.map((t) => h("label", { class: "check small" },
    h("input", { type: "checkbox", value: String(t.id), checked: chosen.has(String(t.id)) }), "#" + t.id + " " + t.title)) : h("span", { class: "hint" }, "No other tasks yet."));
}

function taskForm(parent, depsBox, getTasks, onDone) {
  const title = h("input", { name: "title", placeholder: "Optional: taken from the prompt" });
  const repo = h("input", { name: "repo", type: "url", required: true, placeholder: "https://github.com/you/repo", autocapitalize: "off", spellcheck: "false" });
  const base = h("input", { name: "base", value: "main", autocapitalize: "off" });
  const prompt = h("textarea", { name: "prompt", rows: 4, required: true });
  const priority = h("input", { type: "number", value: "0", inputmode: "numeric" });
  const cli = select("cli", [["", "Any"], ...Object.entries(CLI_NAMES)], "");
  const preset = h("select", null, h("option", { value: "" }, "Any"));
  const git = h("select", null, h("option", { value: "" }, "None (public repo)"));
  fillDeps(depsBox, getTasks());
  (async () => {
    try {
      const [ps, cs] = await Promise.all([api("/presets"), api("/credentials")]);
      preset.append(...ps.filter((p) => p.cli).map((p) => h("option", { value: p.key }, p.label)));
      git.append(...cs.filter((c) => c.preset === "git").map((c) => h("option", { value: String(c.id) }, c.name)));
    } catch (e) { toast(e.message); }
  })();
  const form = h("form", { style: "display:flex;flex-direction:column;gap:10px", onsubmit: (e) => {
    e.preventDefault();
    act(async () => {
      await api("/tasks", { method: "POST", body: {
        title: title.value, repo: repo.value, base_branch: base.value, prompt: prompt.value, priority: Number(priority.value) || 0,
        cli: cli.value, preset: preset.value, git_credential_id: Number(git.value) || 0,
        deps: [...depsBox.querySelectorAll("input:checked")].map((i) => Number(i.value)),
      } });
      title.value = ""; prompt.value = "";
      toast("Task queued");
      await onDone();
      fillDeps(depsBox, getTasks());
    })();
  } },
    field("Prompt", prompt),
    h("div", { class: "fields" }, field("Repository", repo), field("Base branch", base), field("Title", title), field("Git token", git, "Needed to push. Use a fine-grained token for one repo."),
      field("Priority", priority), field("CLI", cli), field("Provider", preset)),
    h("div", null, h("div", { class: "small", style: "margin-bottom:4px" }, "Depends on"), depsBox),
    h("div", null, h("button", { class: "primary", type: "submit" }, "Add to queue")));
  parent.append(form);
}

// ---- approvals

function approvalsPage(focusID) {
  const pending = h("div", { style: "display:flex;flex-direction:column;gap:10px" });
  const past = h("div", { class: "table-wrap" });
  const kept = new Map(); // keeps half-filled cards across refreshes
  const refresh = async () => {
    const all = await api("/approvals");
    const waiting = all.filter((a) => a.status === "pending");
    const ids = new Set(waiting.map((a) => a.id));
    for (const id of kept.keys()) if (!ids.has(id)) kept.delete(id);
    if (!waiting.length) clear(pending, h("div", { class: "card muted" }, "Nothing is waiting for you."));
    else clear(pending, waiting.map((a) => {
      if (!kept.has(a.id)) kept.set(a.id, approvalCard(a, refresh));
      return kept.get(a.id);
    }));
    const target = focusID && kept.get(focusID);
    if (target) { target.scrollIntoView({ block: "center" }); focusID = 0; }
    const done = all.filter((a) => a.status !== "pending").slice(0, 30);
    clear(past, done.length ? h("table", null,
      h("thead", null, h("tr", null, ["Agent", "Tool", "Outcome", "When"].map((c) => h("th", null, c)))),
      h("tbody", null, done.map((a) => h("tr", null, h("td", { class: "break" }, a.agent_name), h("td", { class: "break" }, a.tool),
        h("td", null, a.status), h("td", null, when(a.decided_at)))))) : h("p", { class: "muted small" }, "No past requests."));
  };
  return { el: h("div", { style: "display:contents" }, h("h2", null, "Waiting for you"), pending,
    h("details", { class: "card" }, h("summary", null, "Recent decisions"), past)), refresh };
}

function approvalCard(a, refresh) {
  const decide = (allow, extra) => act(async () => {
    await api("/approvals/" + a.id, { method: "POST", body: { allow, ...extra } });
    await refresh();
  });
  const message = h("input", { placeholder: "Reason (optional): the agent sees this", "aria-label": "Reason for denying" });
  const deny = h("button", { type: "button", class: "danger", onclick: (e) => decide(false, { message: message.value })(e) }, "Deny");
  const head = h("div", { class: "row between" }, h("strong", { class: "break" }, a.agent_name), h("span", { class: "small muted" }, when(a.created_at)));

  let input = null;
  try { input = JSON.parse(a.input); } catch { /* clipped, so show it as text */ }
  if (a.tool === "AskUserQuestion" && input && Array.isArray(input.questions)) {
    const readers = input.questions.map((q, i) => {
      const type = q.multiSelect ? "checkbox" : "radio";
      const other = h("input", { placeholder: "Or type your own answer", "aria-label": "Your own answer" });
      const box = h("fieldset", { style: "border:0;padding:0;margin:0;display:flex;flex-direction:column;gap:6px" },
        h("legend", { style: "font-weight:600;padding:0;margin-bottom:4px" }, q.question),
        (q.options || []).map((o) => h("label", { class: "check" }, h("input", { type, name: "q" + a.id + "-" + i, value: o.label }),
          h("span", null, h("span", { style: "color:var(--ink)" }, o.label), o.description ? " - " + o.description : ""))), other);
      return { box, read: () => {
        if (other.value.trim()) return other.value.trim();
        const picked = [...box.querySelectorAll("input:checked")].map((x) => x.value);
        return q.multiSelect ? picked.join(", ") : picked[0];
      }, question: q.question };
    });
    const submit = h("button", { type: "button", class: "primary", onclick: (e) => {
      const answers = {};
      for (const r of readers) {
        const v = r.read();
        if (!v) { toast("Please answer: " + r.question); return; }
        answers[r.question] = v;
      }
      decide(true, { input: { questions: input.questions, answers } })(e);
    } }, "Send answer");
    return h("div", { class: "card" }, head, h("div", { class: "small muted" }, "The agent has a question"), readers.map((r) => r.box), message, h("div", { class: "row" }, submit, deny));
  }
  return h("div", { class: "card" }, head,
    h("div", null, "Wants to use ", h("strong", { class: "break" }, a.tool)),
    h("pre", { class: "code mono" }, input ? JSON.stringify(input, null, 2) : a.input),
    message,
    h("div", { class: "row" }, h("button", { type: "button", class: "primary", onclick: (e) => decide(true, {})(e) }, "Allow"), deny));
}

// ---- costs

const STATE_NOTES = {
  estimated: "estimated from the stream (no usable result)",
  unpriced: "unpriced (no entry in the price table)",
  subscription: "on a subscription (tokens only)",
  priced: "priced from the table",
  reported: "reported by the CLI",
};

function costsPage() {
  const root = h("div", { style: "display:contents" });
  let sys = null;
  const refresh = async () => {
    const [m, system] = await Promise.all([api("/metrics"), sys ? Promise.resolve(sys) : api("/system").catch(() => null)]);
    sys = system;
    setTimeout(() => { sys = null; }, 15000);
    clear(root,
      h("div", { class: "card" }, h("span", { class: "small muted" }, "Estimated spend today"), h("div", { class: "hero" }, usd(m.spent_today)),
        h("p", { class: "hint" }, "Every figure here is an estimate and can differ from your bill. Your providers' spend limits are the real backstop.")),
      h("div", { class: "stats" },
        statTile("Estimated spend this month", usd(m.spent_month)),
        statTile("Task success", taskRate(m.tasks))),
      meterCard("Daily budget", m.spent_today, m.budget_daily_usd),
      meterCard("Monthly budget", m.spent_month, m.budget_monthly_usd),
      h("div", { class: "card" }, h("h3", null, "Estimated spend per day, last 30 days"), dailyChart(m.by_day)),
      barCard("Spend by model", m.by_model),
      barCard("Spend by agent", m.by_agent),
      barCard("Spend by provider", m.by_provider),
      perfCard(m.by_agent),
      statesCard(m.usd_states),
      recentCard(m.recent),
      systemCard(sys));
  };
  return { el: root, refresh };
}

function statTile(label, value) {
  return h("div", { class: "card" }, h("span", { class: "small muted" }, label), h("span", { class: "stat-value" }, value));
}

function taskRate(tasks) {
  let ok = 0, closed = 0;
  for (const t of tasks || []) {
    if (["done", "failed", "no_changes"].includes(t.status)) closed += t.n;
    if (t.status === "done") ok += t.n;
  }
  return closed ? Math.round((ok / closed) * 100) + "%" : "No tasks yet";
}

// The fill carries severity. Icon and label go with it, so it's never color alone.
function meterCard(label, spent, cap) {
  if (!cap) return h("div", { class: "card" }, h("div", { class: "row between" }, h("h3", null, label), h("span", { class: "small muted" }, "Not set")));
  const share = spent / cap;
  const state = share >= 1 ? ["over", "● Reached: new work is stopped"] : share >= 0.8 ? ["near", "▲ Close to the limit"] : ["", "On track"];
  const bar = h("div", { class: "meter " + state[0], role: "meter", "aria-valuemin": "0", "aria-valuemax": String(cap), "aria-valuenow": String(Math.min(spent, cap)), "aria-label": label },
    h("div", { style: "width:" + Math.min(share * 100, 100) + "%" }));
  return h("div", { class: "card" }, h("div", { class: "row between" }, h("h3", null, label), h("span", { class: "small" }, usd(spent) + " of " + usd(cap))), bar, h("span", { class: "small muted" }, state[1]));
}

function niceMax(v) {
  if (v <= 0) return 1;
  const p = Math.pow(10, Math.floor(Math.log10(v)));
  for (const m of [1, 2, 5, 10]) if (m * p >= v) return m * p;
  return 10 * p;
}

// One series, so one color and no legend: the title says what's plotted.
function dailyChart(rows) {
  const wrap = h("div", { class: "chart" });
  if (!rows.length) { wrap.append(h("p", { class: "muted small" }, "No runs yet.")); return wrap; }
  // Fill the gaps, so columns line up with real days.
  const byDay = new Map(rows.map((r) => [r.day, r]));
  const days = [];
  for (let i = 29; i >= 0; i--) {
    const d = new Date(); d.setDate(d.getDate() - i);
    const key = d.getFullYear() + "-" + String(d.getMonth() + 1).padStart(2, "0") + "-" + String(d.getDate()).padStart(2, "0");
    days.push({ day: key, usd: byDay.has(key) ? byDay.get(key).usd : 0, runs: byDay.has(key) ? byDay.get(key).runs : 0 });
  }
  const tip = h("div", { class: "tip", hidden: true });
  const draw = () => {
    const width = Math.max(wrap.clientWidth, 260), height = 170, left = 44, bottom = 22, top = 8, right = 4;
    const max = niceMax(Math.max(...days.map((d) => d.usd)));
    const y = (v) => top + (height - top - bottom) * (1 - v / max);
    const band = (width - left - right) / days.length;
    const barW = Math.min(24, Math.max(3, band - 2)); // 2px surface gap between neighbours
    const svg = s("svg", { viewBox: "0 0 " + width + " " + height, role: "img", "aria-label": "Estimated spend per day for the last 30 days. The table below has the values." });
    for (const t of [0, max / 2, max]) {
      svg.append(s("line", { class: t === 0 ? "base" : "grid", x1: left, x2: width - right, y1: y(t), y2: y(t) }));
      // Two significant figures: rounding a tick to cents would misstate the axis.
      const label = s("text", { x: left - 6, y: y(t) + 4, "text-anchor": "end" }, t === 0 ? "$0" : "$" + Number(t.toPrecision(2)));
      svg.append(label);
    }
    days.forEach((d, i) => {
      const x = left + band * i + (band - barW) / 2;
      const hit = s("rect", { class: "hit", x: left + band * i, y: top, width: band, height: height - top - bottom, tabindex: d.runs ? 0 : null, "aria-label": d.day + ": " + usd(d.usd) });
      const show = () => {
        clear(tip, h("b", null, usd(d.usd)), d.day + " · " + d.runs + (d.runs === 1 ? " run" : " runs"));
        tip.hidden = false;
        const room = wrap.clientWidth - tip.offsetWidth;
        tip.style.left = Math.max(0, Math.min(x - tip.offsetWidth / 2, room)) + "px";
        tip.style.top = "0px";
      };
      hit.addEventListener("pointerenter", show);
      hit.addEventListener("pointerdown", show);
      hit.addEventListener("focus", show);
      hit.addEventListener("pointerleave", () => { tip.hidden = true; });
      hit.addEventListener("blur", () => { tip.hidden = true; });
      svg.append(hit);
      if (d.usd > 0) {
        // 4px rounded data-end, square at the baseline.
        const h0 = Math.max(2, height - bottom - y(d.usd)), r = Math.min(4, barW / 2, h0);
        const by = height - bottom - h0;
        svg.append(s("path", { class: "col", d: `M${x},${height - bottom}V${by + r}Q${x},${by} ${x + r},${by}H${x + barW - r}Q${x + barW},${by} ${x + barW},${by + r}V${height - bottom}Z`, "pointer-events": "none" }));
      }
    });
    for (const i of [0, days.length - 1]) {
      svg.append(s("text", { x: i === 0 ? left : width - right, y: height - 6, "text-anchor": i === 0 ? "start" : "end" }, days[i].day.slice(5)));
    }
    clear(wrap, svg, tip);
  };
  const ro = new ResizeObserver(draw);
  ro.observe(wrap);
  const table = h("details", null, h("summary", { class: "small" }, "Show as a table"), h("div", { class: "table-wrap" }, h("table", null,
    h("thead", null, h("tr", null, h("th", null, "Day"), h("th", { class: "num" }, "Spend"), h("th", { class: "num" }, "Runs"))),
    h("tbody", null, rows.slice().reverse().map((r) => h("tr", null, h("td", null, r.day), h("td", { class: "num" }, usd(r.usd)), h("td", { class: "num" }, r.runs)))))));
  return h("div", { style: "display:flex;flex-direction:column;gap:8px" }, wrap, table);
}

// Spend we can't measure must not read as $0.
function spendLabel(r) {
  return !r.usd && r.not_priced ? "not priced" : usd(r.usd);
}

// Ranked bars, one color: the order and length carry it, and each value is printed.
function barCard(title, rows) {
  const card = h("div", { class: "card" }, h("h3", null, title));
  if (!rows || !rows.length) { card.append(h("p", { class: "muted small" }, "No runs yet.")); return card; }
  const top = rows.slice(0, 7);
  const rest = rows.slice(7);
  if (rest.length) top.push({ name: "Other (" + rest.length + ")", usd: rest.reduce((n, r) => n + r.usd, 0), runs: rest.reduce((n, r) => n + r.runs, 0) });
  const max = Math.max(...top.map((r) => r.usd), 0);
  card.append(h("div", { class: "bars" }, top.map((r) => h("div", null,
    h("div", { class: "bar-label" }, r.name),
    h("div", { class: "bar-line" },
      h("div", { class: "bar-track" }, h("div", { class: "bar-fill", style: "width:" + (max ? Math.max((r.usd / max) * 100, 1) : 1) + "%", title: r.name + ": " + usd(r.usd) })),
      h("span", { class: "bar-value" }, spendLabel(r) + " · " + r.runs + (r.runs === 1 ? " run" : " runs")))))));
  return card;
}

function perfCard(rows) {
  const card = h("div", { class: "card" }, h("h3", null, "Performance by agent, last 30 days"));
  if (!rows || !rows.length) { card.append(h("p", { class: "muted small" }, "No runs yet.")); return card; }
  const cols = [
    ["Agent", (r) => r.name, false],
    ["Runs", (r) => count(r.runs), true],
    ["Success", (r) => (r.runs ? Math.round((r.ok / r.runs) * 100) : 0) + "%", true],
    ["Avg time", (r) => duration(r.avg_sec), true],
    ["Turns", (r) => count(r.turns), true],
    ["Tokens", (r) => count(r.input + r.output + r.cache_read + r.cache_write), true],
    ["Cache hits", (r) => { const t = r.input + r.cache_read + r.cache_write; return t ? Math.round((r.cache_read / t) * 100) + "%" : "-"; }, true],
    ["Retries", (r) => count(r.retries), true],
    ["Tool calls", (r) => count(r.tool_calls), true],
    ["Denied", (r) => count(r.denials), true],
    ["Spend", spendLabel, true],
  ];
  card.append(h("div", { class: "table-wrap" }, h("table", null,
    h("thead", null, h("tr", null, cols.map(([c, , n]) => h("th", { class: n ? "num" : null }, c)))),
    h("tbody", null, rows.map((r) => h("tr", null, cols.map(([, f, n]) => h("td", { class: n ? "num" : "break" }, f(r)))))))));
  return card;
}

function duration(sec) {
  sec = Math.round(Number(sec) || 0);
  if (sec < 90) return sec + "s";
  if (sec < 5400) return Math.round(sec / 60) + "m";
  return (sec / 3600).toFixed(1) + "h";
}

function statesCard(rows) {
  if (!rows || !rows.length) return null;
  return h("div", { class: "card" }, h("h3", null, "How these figures were measured"),
    h("ul", { style: "margin:0;padding-left:18px", class: "small" }, rows.map((r) => h("li", null, r.runs + (r.runs === 1 ? " run " : " runs ") + (STATE_NOTES[r.state] || r.state)))));
}

function recentCard(rows) {
  if (!rows || !rows.length) return null;
  return h("details", { class: "card" }, h("summary", null, "Recent runs"), h("div", { class: "table-wrap" }, h("table", null,
    h("thead", null, h("tr", null, h("th", null, "Run"), h("th", null, "Agent"), h("th", null, "Status"), h("th", { class: "num" }, "Tokens"), h("th", { class: "num" }, "Spend"), h("th", null, "Basis"))),
    h("tbody", null, rows.map((r) => h("tr", null, h("td", null, r.id), h("td", { class: "break" }, r.agent), h("td", null, words(r.status)),
      h("td", { class: "num" }, count(r.tokens)), h("td", { class: "num" }, usd(r.usd)), h("td", null, r.usd_state)))))));
}

function systemCard(sys) {
  if (!sys) return null;
  const gb = (b) => (b / (1 << 30)).toFixed(1) + " GB";
  return h("div", { class: "card" }, h("h3", null, "Server, live"),
    sys.disk_total ? h("div", { class: "small" }, "Disk: " + gb(sys.disk_free) + " free of " + gb(sys.disk_total)) : null,
    sys.containers.length ? h("div", { class: "table-wrap" }, h("table", null,
      h("thead", null, h("tr", null, h("th", null, "Container"), h("th", { class: "num" }, "CPU"), h("th", { class: "num" }, "Memory"))),
      h("tbody", null, sys.containers.map((c) => h("tr", null, h("td", null, c.name), h("td", { class: "num" }, c.cpu), h("td", { class: "num" }, c.mem)))))) : h("p", { class: "small muted" }, "No agent containers are running."));
}

// ---- settings

function settingsPage() {
  const credList = h("div", { style: "display:flex;flex-direction:column;gap:6px" });
  const priceList = h("div", { class: "table-wrap" });
  const daily = h("input", { type: "number", min: "0", step: "0.01", inputmode: "decimal" });
  const monthly = h("input", { type: "number", min: "0", step: "0.01", inputmode: "decimal" });
  const webhook = h("input", { type: "url", placeholder: "https://ntfy.sh/your-private-topic", autocapitalize: "off", spellcheck: "false" });
  let loaded = false, presets = [];

  const presetSel = h("select", { name: "preset" });
  const credName = h("input", { name: "name", required: true });
  const varsBox = h("div", { style: "display:flex;flex-direction:column;gap:10px" });
  const noteBox = h("p", { class: "hint" });
  function drawVars() {
    const p = presets.find((x) => x.key === presetSel.value);
    if (!p) return;
    noteBox.textContent = p.note || "";
    const rows = [
      ...(p.env || []).map((n) => field(n, h("input", { "data-env": n, type: n.includes("URL") || n.includes("REGION") || n.includes("PROJECT") || n.includes("LOCATION") || n.includes("RESOURCE") ? "text" : "password", autocomplete: "off", autocapitalize: "off", spellcheck: "false" }))),
      ...(p.files || []).map((n) => field(n + " (paste the file's contents)", h("textarea", { "data-file": n, rows: 4, autocapitalize: "off", spellcheck: "false" }))),
    ];
    clear(varsBox, rows, field("Extra variables (one NAME=value per line)", h("textarea", { "data-extra": "1", rows: 2, autocapitalize: "off", spellcheck: "false" }), "Optional, for example a custom base URL."));
  }
  presetSel.addEventListener("change", drawVars);

  const credForm = h("form", { style: "display:flex;flex-direction:column;gap:10px", onsubmit: (e) => {
    e.preventDefault();
    act(async () => {
      const env = {}, files = {};
      for (const i of varsBox.querySelectorAll("[data-env]")) if (i.value) env[i.dataset.env] = i.value;
      for (const i of varsBox.querySelectorAll("[data-file]")) if (i.value) files[i.dataset.file] = i.value;
      const extra = varsBox.querySelector("[data-extra]").value;
      for (const line of extra.split("\n")) {
        const at = line.indexOf("=");
        if (at > 0) env[line.slice(0, at).trim()] = line.slice(at + 1).trim();
      }
      await api("/credentials", { method: "POST", body: { name: credName.value, preset: presetSel.value, env, files } });
      credName.value = "";
      drawVars();
      toast("Credential saved");
      await refresh();
    })();
  } }, field("Provider", presetSel), noteBox, field("Name", credName), varsBox,
    h("p", { class: "hint" }, "Stored encrypted. Agents can read keys passed to them, so use dedicated keys with a spend limit set at the provider."),
    h("div", null, h("button", { class: "primary", type: "submit" }, "Save credential")));

  const pm = h("input", { placeholder: "Model ID", autocapitalize: "off", spellcheck: "false", required: true, "aria-label": "Model ID" });
  const num = (label) => h("input", { type: "number", min: "0", step: "0.001", inputmode: "decimal", placeholder: label, "aria-label": label + " per million tokens" });
  const [pi, po, pr, pw] = ["Input", "Output", "Cache read", "Cache write"].map(num);
  const priceForm = h("form", { class: "fields", onsubmit: (e) => {
    e.preventDefault();
    act(async () => {
      await api("/prices", { method: "PUT", body: { model: pm.value.trim(), input: Number(pi.value) || 0, output: Number(po.value) || 0, cache_read: Number(pr.value) || 0, cache_write: Number(pw.value) || 0 } });
      for (const i of [pm, pi, po, pr, pw]) i.value = "";
      await refresh();
    })();
  } }, pm, pi, po, pr, pw, h("button", { type: "submit" }, "Save price"));

  const refresh = async () => {
    const [creds, prices, settings, ps] = await Promise.all([api("/credentials"), api("/prices"), api("/settings"), loaded ? null : api("/presets")]);
    if (!loaded) {
      loaded = true;
      presets = ps;
      clear(presetSel, presets.map((p) => h("option", { value: p.key }, p.label)));
      drawVars();
      daily.value = settings.budget_daily_usd || "";
      monthly.value = settings.budget_monthly_usd || "";
      webhook.value = settings.webhook_url || "";
    }
    clear(credList, creds.length ? creds.map((c) => h("div", { class: "row between" },
      h("span", { class: "grow break" }, h("strong", null, c.name), h("span", { class: "small muted" }, " " + c.preset + ": " + c.vars.sort().join(", "))),
      armed("Delete", "Tap again", async () => { await api("/credentials/" + c.id, { method: "DELETE" }); await refresh(); }))) : h("p", { class: "muted small" }, "No credentials yet."));
    prices.sort((a, b) => a.model.localeCompare(b.model));
    clear(priceList, h("table", null,
      h("thead", null, h("tr", null, h("th", null, "Model"), ["Input", "Output", "Cache read", "Cache write"].map((c) => h("th", { class: "num" }, c)), h("th", null, "Checked"), h("th", null, ""))),
      h("tbody", null, prices.map((p) => h("tr", null, h("td", { class: "break" }, p.model),
        [p.input, p.output, p.cache_read, p.cache_write].map((v) => h("td", { class: "num" }, v)), h("td", null, p.checked_at),
        h("td", null, armed("Delete", "Sure?", async () => { await api("/prices/" + encodeURIComponent(p.model), { method: "DELETE" }); await refresh(); })))))));
  };

  const el = h("div", { style: "display:contents" },
    h("div", { class: "card" }, h("h2", null, "Credentials"), credList, h("details", null, h("summary", null, "Add a credential"), credForm)),
    h("form", { class: "card", onsubmit: (e) => {
      e.preventDefault();
      act(async () => {
        await api("/settings", { method: "PUT", body: { budget_daily_usd: daily.value, budget_monthly_usd: monthly.value, webhook_url: webhook.value } });
        toast("Saved");
      })();
    } }, h("h2", null, "Budgets and alerts"),
      h("div", { class: "fields" }, field("Daily budget (USD)", daily), field("Monthly budget (USD)", monthly)),
      h("p", { class: "hint" }, "Reaching a budget stops new tasks and interrupts running agents. These are estimates, so also set a limit at your provider."),
      field("Webhook URL", webhook, "Gets approval requests and failures. Works with ntfy, Slack and Discord. Only the tool's name is sent, never its input."),
      h("div", null, h("button", { class: "primary", type: "submit" }, "Save"))),
    h("div", { class: "card" }, h("h2", null, "Prices"), h("p", { class: "hint" }, "USD per million tokens. Used for CLIs that don't report USD, and for estimates during a run. Prices change, so check the dates."), priceList, priceForm));
  return { el, refresh };
}

// ---- start

window.addEventListener("hashchange", route);
(async () => {
  try {
    await api("/state");
  } catch {
    return; // api() has already shown the login page if that's the reason
  }
  connectStateFeed();
  route();
})();
