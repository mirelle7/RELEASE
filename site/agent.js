"use strict";
/* "Build on my PC": connects the page to the build agent (a small program the user runs on their own computer).
 * Exposes window.PortalAgent for app.js and fires a "portal:agent" event whenever its state changes. */
(function () {
  const $ = (id) => document.getElementById(id);
  const el = (tag, props = {}, ...kids) => {
    const n = document.createElement(tag);
    for (const [k, v] of Object.entries(props)) {
      if (k === "class") n.className = v;
      else if (k === "text") n.textContent = v;
      else if (k.startsWith("on")) n.addEventListener(k.slice(2), v);
      else if (v !== undefined && v !== false && v !== null) n.setAttribute(k, v);
    }
    for (const c of kids) n.append(c);
    return n;
  };
  const store = {
    get(k) { try { return sessionStorage.getItem(k); } catch (e) { return null; } },
    set(k, v) { try { v == null ? sessionStorage.removeItem(k) : sessionStorage.setItem(k, v); } catch (e) { /* storage unavailable */ } },
  };

  // The agent opens the page with #agent-token=...; take the token and remove it from the address bar.
  let hashToken = null;
  const m = /(?:^#|[&#])agent-token=([\w-]+)/.exec(location.hash);
  if (m) { hashToken = m[1]; history.replaceState(null, "", location.pathname + location.search); }

  const Agent = { connected: false, url: null, token: null, current: null, progress: "", queue: [], results: [], timer: null, fails: 0, reports: {}, watch: null };
  window.PortalAgent = Agent;
  const changed = () => window.dispatchEvent(new CustomEvent("portal:agent"));
  const say = (msg, cls = "") => { const s = $("agent-status"); s.textContent = msg; s.className = "agent-status " + cls; };

  async function call(path, opts = {}) {
    const headers = { "X-Agent-Token": Agent.token || "" };
    if (opts.body) headers["Content-Type"] = "application/json";
    const r = await fetch(Agent.url + path, { ...opts, headers });
    let body = null;
    try { body = await r.clone().json(); } catch (e) { /* not JSON */ }
    return { status: r.status, body, response: r };
  }

  async function connect(url, token) {
    url = url.replace(/\/+$/, "");
    Agent.url = url; Agent.token = token;
    say("Connecting…");
    try {
      const ping = await call("/api/ping");
      if (!ping.body || ping.body.agent !== "portal-build-agent") throw new Error("not an agent");
      const st = await call("/api/status");
      if (st.status === 401) { say("The agent is running, but the token is wrong. Use the link it opened, or copy the token from its window.", "bad"); return false; }
      if (st.status !== 200) throw new Error("status " + st.status);
      Object.assign(Agent, { connected: true, fails: 0 });
      store.set("agent-url", url); store.set("agent-token", token);
      apply(st.body);
      say("Connected" + (st.body.dry_run ? " (dry run: nothing will really be built)" : ""), "ok");
      $("agent-connect").hidden = true; $("agent-disconnect").hidden = false;
      clearInterval(Agent.timer); Agent.timer = setInterval(poll, 2000);
      changed();
      return true;
    } catch (e) {
      Agent.connected = false;
      say("Could not reach an agent at " + url + ". Is it running? " +
        (location.origin !== url ? `If this page is hosted elsewhere, start the agent with --allow-origin ${location.origin}. ` : "") +
        (location.protocol === "https:" ? "Some browsers (Safari) will not let a secure page talk to a local program; use the link the agent opens instead." : ""), "bad");
      return false;
    }
  }

  function disconnect() {
    clearInterval(Agent.timer);
    Object.assign(Agent, { connected: false, current: null, progress: "", queue: [], results: [], reports: {}, watch: null });
    store.set("agent-token", null);
    $("agent-connect").hidden = false; $("agent-disconnect").hidden = true;
    say("Disconnected.");
    renderQueue(); changed();
  }

  function apply(s) {
    Object.assign(Agent, { current: s.current || null, progress: s.progress || "", queue: s.queue || [], results: s.results || [] });
    renderQueue();
  }

  async function poll() {
    try {
      const st = await call("/api/status");
      if (st.status !== 200) throw new Error("status " + st.status);
      Agent.fails = 0;
      const sig = () => JSON.stringify([Agent.current, Agent.progress, Agent.queue, Agent.results.map((r) => r.id + r.at_unix)]);
      const before = sig();
      apply(st.body);
      if (before !== sig()) changed();
    } catch (e) {
      if (++Agent.fails >= 3) { disconnect(); say("Lost contact with the agent.", "bad"); }
    }
  }

  /* job = { id, preset, game, args }: an already-resolved build. The agent re-checks every field. */
  Agent.queueBuild = async function (job) {
    const r = await call("/api/queue", { method: "POST", body: JSON.stringify(job) });
    if (r.status === 202) say(`Queued ${r.body.id} on your PC.`, "ok");
    else if (r.status === 200) say(`${r.body.id} is already queued.`);
    else say("The agent refused that build: " + ((r.body && r.body.error) || r.status), "bad");
    poll();
  };

  Agent.cancel = async function () {
    const r = await call("/api/cancel", { method: "POST" });
    say(r.status === 200 ? "Cancelled. Nothing more will start." : "Could not cancel (" + r.status + ").", r.status === 200 ? "" : "bad");
    poll();
  };

  /* Can this PC build this preset? The agent checks (OS, tools, compiler, container/WSL) and says how to fix what is missing. */
  const inflight = {};
  Agent.checkPreset = async function (preset, force) {
    if (!Agent.connected || !preset) return;
    const cur = Agent.reports[preset];
    if (!force && ((cur && Date.now() - cur.at < 20000) || inflight[preset])) return;
    inflight[preset] = true;
    try {
      const r = await call("/api/doctor?preset=" + encodeURIComponent(preset));
      if (r.status === 200) { Agent.reports[preset] = { ...r.body, at: Date.now() }; renderDoctor(); changed(); }
    } catch (e) { /* the status poll notices a lost agent */ } finally { delete inflight[preset]; }
  };
  Agent.setPreset = function (preset) {
    if (Agent.watch !== preset) { Agent.watch = preset; renderDoctor(); }
    Agent.checkPreset(preset);
  };
  /* { known, ready, problems } for a preset; unknown means the report has not arrived yet (the agent still checks). */
  Agent.readiness = function (preset) {
    const r = Agent.reports[preset];
    if (!r) return { known: false, ready: true, problems: [] };
    return { known: true, ready: r.ready, problems: r.checks.filter((c) => c.status === "fail") };
  };

  function renderDoctor() {
    const box = $("agent-doctor");
    box.replaceChildren();
    const r = Agent.connected && Agent.watch && Agent.reports[Agent.watch];
    if (!r) return;
    const family = { vc6: "Visual C++ 6", msvc: "Visual Studio", mingw: "MinGW-w64" }[r.family] || r.family;
    box.append(el("div", { class: "doc-head" },
      el("strong", { text: `This PC for ${family} (${r.preset}): ` }),
      el("span", { class: r.ready ? "ok" : "bad", text: r.ready ? (r.skipped ? "checks skipped" : "ready") : "not ready" }),
      el("button", { type: "button", class: "ghost", text: "Check again", onclick: () => Agent.checkPreset(r.preset, true) })));
    if (r.backend && r.backend !== "native") box.append(el("p", { class: "hint", text: "The build will run in: " + r.backend }));
    const icon = { ok: "✓", warn: "!", fail: "✗", info: "·" };
    for (const c of r.checks) {
      box.append(el("div", { class: "chk " + c.status },
        el("span", { class: "ico", text: icon[c.status] || "·" }),
        el("div", {}, el("div", { class: "nm", text: c.name + (c.detail ? ": " + c.detail : "") }), c.fix && c.status !== "ok" ? el("div", { class: "fix", text: c.fix }) : "")));
    }
    if (!r.ready) box.append(el("p", { class: "hint", text: "Fix the items marked ✗ and press “Check again”. If you know your setup works, start the agent with --skip-checks." }));
  }

  Agent.resultFor = (id) => Agent.results.find((r) => r.id === id);

  Agent.download = async function (id) {
    const r = await call("/api/download/" + encodeURIComponent(id));
    if (r.status !== 200) { say("Download failed (" + r.status + ").", "bad"); return; }
    const blob = await r.response.blob();
    const a = el("a", { href: URL.createObjectURL(blob), download: id + ".zip" });
    document.body.append(a); a.click(); a.remove();
    setTimeout(() => URL.revokeObjectURL(a.href), 10000);
  };

  function renderQueue() {
    const box = $("agent-queue");
    box.replaceChildren();
    if (!Agent.connected) return;
    const row = (label, id, cls, extra) => box.append(el("div", { class: "qrow" }, el("span", { class: "tag " + cls, text: label }), el("code", { text: id }), ...(extra || [])));
    if (Agent.current || Agent.queue.length) {
      box.append(el("p", { class: "progress" },
        el("span", { text: Agent.current ? `Building ${Agent.current}${Agent.progress ? " — " + Agent.progress : ""}` : "Waiting to start" }),
        el("button", { type: "button", class: "ghost", text: "Cancel", onclick: () => Agent.cancel() })));
    }
    Agent.queue.forEach((id) => row("queued", id, ""));
    const finished = (r) => {
      const name = r.label ? `${r.label}${r.sha ? " · " + r.sha.slice(0, 7) : ""}` : r.id;
      if (r.dry_run) return row("dry run", name, "");
      if (r.ok) return row("done", name, "ok", [el("button", { type: "button", class: "ghost", text: "Download", onclick: () => Agent.download(r.id) })]);
      row("failed", name, "bad", [el("span", { class: "hint", text: r.error || "" })]);
    };
    // Range steps are shown together under their range; everything else as before.
    const groups = new Map(), singles = [];
    for (const r of Agent.results.slice(0, 60)) (r.group ? (groups.has(r.group) ? groups.get(r.group) : groups.set(r.group, []).get(r.group)).push(r) : singles.push(r));
    for (const [id, steps] of groups) {
      const ok = steps.filter((s) => s.ok).length;
      box.append(el("div", { class: "qgroup", text: `Range ${id}: ${ok} of ${steps.length} built` }));
      steps.sort((x, y) => x.at_unix - y.at_unix).forEach(finished);
    }
    singles.slice(0, 8).forEach(finished);
    if (!box.children.length) box.append(el("p", { class: "hint", text: "Nothing yet. Pick a configuration above and press “Build on my PC”." }));
  }

  // Is this page being served by the agent itself (opened without the token link)? Only worth asking on loopback.
  async function servedByAgent() {
    if (!/^(127\.0\.0\.1|localhost)$/.test(location.hostname)) return false;
    try { const r = await fetch("/api/ping"); return (await r.json()).agent === "portal-build-agent"; } catch (e) { return false; }
  }

  async function init() {
    if (!$("agent-connect")) return;
    if (!/Windows/i.test(navigator.userAgent || "")) $("os-note").hidden = false;
    $("agent-connect").addEventListener("click", () => connect($("agent-url").value.trim(), $("agent-token").value.trim()));
    $("agent-disconnect").addEventListener("click", disconnect);
    const saved = store.get("agent-url"), savedToken = store.get("agent-token");
    // A link with a token comes from the agent itself, so its own address is the agent's address.
    $("agent-url").value = hashToken || (!saved && await servedByAgent()) ? location.origin : saved || "http://127.0.0.1:8787";
    if (hashToken || savedToken) {
      $("agent-token").value = hashToken || savedToken;
      connect($("agent-url").value, $("agent-token").value);
    } else renderQueue();
  }
  if (document.readyState === "loading") document.addEventListener("DOMContentLoaded", init); else init();
})();
