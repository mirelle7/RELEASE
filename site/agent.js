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

  const Agent = { connected: false, url: null, token: null, current: null, progress: "", queue: [], results: [], timer: null, fails: 0, reports: {}, watch: null, project: null, projects: null, platformsBy: {}, commits: {} };
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
      $("agent-restart").hidden = false; $("agent-quit").hidden = false;
      clearInterval(Agent.timer); Agent.timer = setInterval(poll, 2000);
      Agent.loadProjects().then(() => { Agent.loadPlatforms(); Agent.loadCommits(Agent.project); changed(); });
      loadData().catch(() => {}); // the Source panel
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
    Object.assign(Agent, { connected: false, current: null, batchStart: 0, progress: "", queue: [], results: [], reports: {}, watch: null, projects: null, platformsBy: {}, commits: {} });
    store.set("agent-token", null);
    $("agent-connect").hidden = false; $("agent-disconnect").hidden = true;
    $("agent-restart").hidden = true; $("agent-quit").hidden = true;
    Agent.data = null; renderData();
    Agent.platforms = null; renderPlatforms(); renderProjects();
    say("Disconnected.");
    renderQueue(); renderLive(); changed();
  }

  function apply(s) {
    Object.assign(Agent, { current: s.current || null, progress: s.progress || "", started: s.started_unix > 0 ? s.started_unix : 0, queue: s.queue || [], results: s.results || [] });
    if ((Agent.current || Agent.queue.length) && !Agent.batchStart) Agent.batchStart = Math.floor(Date.now() / 1000) - 10; // a new batch of work began
    if (!Agent.current && !Agent.queue.length) Agent.batchStart = 0;
    renderQueue(); renderLive();
    const busy = !!(Agent.current || Agent.queue.length);
    if (Agent.wasBusy !== undefined && Agent.wasBusy !== busy && Agent.connected) Agent.loadProjects(); // the download size changed, and the delete button follows the busy state
    Agent.wasBusy = busy;
  }

  /* The live task view: overall progress of the current batch, the running job with a clock, and its log following along. */
  const live = { id: null, next: 0, text: "", busy: false };
  function renderLive() {
    const box = $("agent-live");
    if (!box) return;
    box.hidden = !Agent.connected;
    if (!Agent.connected) { live.id = null; return; }
    const since = Agent.batchStart || 0;
    const done = Agent.results.filter((r) => !r.dry_run && r.at_unix >= since && since);
    const ok = done.filter((r) => r.ok).length, bad = done.length - ok, run = Agent.current ? 1 : 0, wait = Agent.queue.length;
    const total = ok + bad + run + wait;
    $("live-counts").textContent = total ? `${ok} done, ${bad} failed, ${run} running, ${wait} waiting` : "";
    const bar = $("live-bar");
    bar.replaceChildren(...(total ? [["ok", ok], ["bad", bad], ["run", run]].map(([c, n]) => el("i", { class: c, style: `width:${(100 * n) / total}%` })) : []));
    $("live-now").hidden = !Agent.current;
    $("live-idle").hidden = !!Agent.current;
    if (Agent.current !== live.id) { live.id = Agent.current; live.next = -1; live.text = ""; $("live-log").textContent = ""; $("live-tail").textContent = ""; }
    $("live-id").textContent = Agent.current || "";
    $("live-progress").textContent = Agent.progress ? "— " + Agent.progress : "";
    tickLive();
  }
  function tickLive() {
    if (!Agent.connected || !Agent.current || !Agent.started) { const e = $("live-elapsed"); if (e) e.textContent = ""; return; }
    const s = Math.max(0, Math.floor(Date.now() / 1000) - Agent.started);
    $("live-elapsed").textContent = `${Math.floor(s / 60)}:${String(s % 60).padStart(2, "0")}`;
  }
  /* The last 3 lines of what the build is doing: no colour codes, no blank lines, a carriage-return progress line counts as one. */
  function renderTail() {
    const lines = live.text.replace(/\x1b\[[0-9;]*[A-Za-z]/g, "").split("\n").map((l) => l.split("\r").pop().trimEnd()).filter((l) => l.trim());
    $("live-tail").textContent = lines.slice(-3).join("\n");
  }
  async function pullLog() {
    if (!Agent.connected || !live.id || live.busy) return;
    live.busy = true;
    const id = live.id;
    try {
      const r = await call(`/api/log?id=${encodeURIComponent(id)}&from=${live.next}`);
      if (r.status === 200 && id === live.id) {
        if (r.body.from !== live.next && live.next >= 0) live.text = ""; // the agent restarted the log
        live.text = (live.text + r.body.text).slice(-60000); live.next = r.body.next;
        const pre = $("live-log"), stick = pre.scrollTop + pre.clientHeight >= pre.scrollHeight - 24; // follow unless the reader scrolled up
        pre.textContent = live.text;
        if (stick) pre.scrollTop = pre.scrollHeight;
        renderTail();
      }
    } catch (e) { /* the status poll notices a lost agent */ } finally { live.busy = false; }
  }
  setInterval(tickLive, 1000);
  setInterval(pullLog, 1500);

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
    // Visual C++ 6 and Visual Studio builds can carry the player's own binkw32.dll and mss32.dll inside the exe.
    if (Agent.embed && /^(vc6|win32)/.test(job.preset || "")) job = { ...job, embed: true };
    const r = await call("/api/queue", { method: "POST", body: JSON.stringify(job) });
    if (r.status === 202) say(`Queued ${r.body.id} on your PC.`, "ok");
    else if (r.status === 200) say(`${r.body.id} is already queued.`);
    else say("The agent refused that build: " + ((r.body && r.body.error) || r.status), "bad");
    poll();
  };

  /* Stop the agent, or restart it (same port and token, so this page reconnects by itself). */
  Agent.exit = async function (restart) {
    if (!Agent.connected) return;
    const r = await call(restart ? "/api/restart" : "/api/quit", { method: "POST" });
    if (r.status !== 200) { say("The agent would not " + (restart ? "restart" : "stop") + " (" + ((r.body && r.body.error) || r.status) + ").", "bad"); return; }
    if (!restart) { disconnect(); say("The agent has stopped. Start it again from where you opened it."); return; }
    const url = Agent.url, token = Agent.token;
    clearInterval(Agent.timer);
    say("Restarting the agent…");
    for (let i = 0; i < 20; i++) { // it needs a moment to free the port and come back
      await new Promise((res) => setTimeout(res, 750));
      if (await connect(url, token)) return;
    }
    say("The agent did not come back. Start it again from where you opened it.", "bad");
  };

  Agent.cancel = async function () {
    const r = await call("/api/cancel", { method: "POST" });
    say(r.status === 200 ? "Cancelled. Nothing more will start." : "Could not cancel (" + r.status + ").", r.status === 200 ? "" : "bad");
    poll();
  };

  /* Can this PC build this preset? The agent checks (OS, tools, compiler, container/WSL) and says how to fix what is missing. */
  const inflight = {};
  const rkey = (preset, project) => (project || Agent.project || "") + "|" + preset;
  Agent.checkPreset = async function (preset, force) {
    if (!Agent.connected || !preset) return;
    const key = rkey(preset), project = Agent.project;
    const cur = Agent.reports[key];
    if (!force && ((cur && Date.now() - cur.at < 20000) || inflight[key])) return;
    inflight[key] = true;
    try {
      const r = await call("/api/doctor?preset=" + encodeURIComponent(preset) + (project ? "&project=" + encodeURIComponent(project) : ""));
      if (r.status === 200) { Agent.reports[key] = { ...r.body, at: Date.now() }; renderDoctor(); changed(); }
    } catch (e) { /* the status poll notices a lost agent */ } finally { delete inflight[key]; }
  };
  Agent.setPreset = function (preset) {
    const key = rkey(preset);
    if (Agent.watch !== key) { Agent.watch = key; renderDoctor(); }
    Agent.checkPreset(preset);
  };
  /* { known, ready, problems } for a preset; unknown means the report has not arrived yet (the agent still checks). */
  Agent.readiness = function (preset) {
    const r = Agent.reports[rkey(preset)];
    if (!r) return { known: false, ready: true, problems: [] };
    return { known: true, ready: r.ready, problems: r.checks.filter((c) => c.status === "fail") };
  };

  /* Which build platforms (compilers) can this PC build with right now? Pick one to select it above. */
  Agent.platforms = null; Agent.compiler = null;
  Agent.loadPlatforms = async function () {
    if (!Agent.connected) return;
    const box = $("agent-platforms"), project = Agent.project;
    if (!Agent.platforms) box.replaceChildren(el("p", { class: "hint", text: "Checking which build platforms this PC can use…" }));
    try {
      const r = await call("/api/platforms" + (project ? "?project=" + encodeURIComponent(project) : ""));
      if (r.status === 200 && r.body) {
        Agent.platformsBy[project] = r.body.platforms;
        if (project !== Agent.project) return; // the page moved on to another project meanwhile
        Agent.platforms = r.body.platforms;
        for (const p of Agent.platforms) if (p.preset) Agent.reports[rkey(p.preset, project)] = { ...p.report, at: Date.now() };
        renderPlatforms(); renderDoctor(); changed();
      } else if (!Agent.platforms) box.replaceChildren(); // an older agent has no platform list
    } catch (e) { /* the status poll notices a lost agent */ }
  };
  function renderPlatforms() {
    const box = $("agent-platforms");
    box.replaceChildren();
    if (!Agent.connected || !Agent.platforms) return;
    const ready = Agent.platforms.filter((p) => p.ready).length;
    box.append(el("div", { class: "doc-head" },
      el("strong", { text: "Build platforms on this PC: " }),
      el("span", { class: ready ? "ok" : "bad", text: `${ready} of ${Agent.platforms.length} ready` }),
      el("button", { type: "button", class: "ghost", text: "Check again", onclick: () => Agent.loadPlatforms() })));
    const list = el("div", { class: "plat-list", role: "radiogroup", "aria-label": "Build platform" });
    for (const p of Agent.platforms) {
      const on = Agent.compiler === p.id;
      list.append(el("button", { type: "button", class: "plat", role: "radio", "aria-checked": on ? "true" : "false",
        onclick: () => window.dispatchEvent(new CustomEvent("portal:pick-compiler", { detail: p.id })) },
        el("strong", { text: p.name }),
        el("span", { class: "st " + (p.ready ? "ok" : "bad"), text: p.ready ? "✓ ready" + (p.backend && p.backend !== "native" ? " (" + p.backend + ")" : "") : "✗ not ready" }),
        p.ready ? "" : el("span", { class: "why", text: p.problem || "Something is missing." })));
    }
    box.append(list);
  }
  /* Can this PC build with this compiler at all? Unknown (list not loaded) counts as no. */
  Agent.platformReady = (id) => !!(Agent.platforms && Agent.platforms.find((p) => p.id === id && p.ready));
  /* The project the page has selected: platforms, readiness and the build folder are all per project. */
  Agent.setProject = function (id) {
    if (Agent.project === id) return;
    Agent.project = id;
    Agent.platforms = Agent.platformsBy[id] || null;
    renderPlatforms(); renderDoctor();
    if (Agent.connected) { Agent.loadPlatforms(); Agent.loadCommits(id); }
  };
  /* True when this project is set to build from the user's own folder instead of the download. */
  Agent.isLocal = (id) => !!(Agent.projects && Agent.projects.some((p) => p.id === id && p.mode === "local"));

  /* The recent commits of a project, for the source version picker. */
  Agent.loadCommits = async function (project) {
    if (!Agent.connected || !project) return;
    const old = Agent.commits[project];
    Agent.commits[project] = { ...(old || { commits: [] }), loading: true };
    changed();
    try {
      const r = await call(`/api/commits?project=${encodeURIComponent(project)}&limit=50&fetch=1`);
      if (r.status === 200 && r.body) Agent.commits[project] = { branch: r.body.branch, head: r.body.head, fetched: r.body.fetched !== false, commits: r.body.commits || [], loading: false };
      else Agent.commits[project] = { ...(old || { commits: [] }), loading: false, fetched: false };
    } catch (e) { Agent.commits[project] = { ...(old || { commits: [] }), loading: false, fetched: false }; }
    changed();
  };

  /* Where each project's source comes from: downloaded by the agent, or the user's own folder. */
  Agent.loadProjects = async function () {
    try {
      const r = await call("/api/projects");
      Agent.projects = r.status === 200 && r.body && r.body.projects ? r.body.projects : null;
    } catch (e) { Agent.projects = null; }
    renderProjects();
  };
  async function postProject(path, body) {
    const r = await call(path, { method: "POST", body: JSON.stringify(body) });
    if (r.status !== 200) return (r.body && r.body.error) || "status " + r.status;
    await Agent.loadProjects();
    if (body.id === Agent.project) { Agent.loadPlatforms(); Agent.loadCommits(body.id); }
    changed();
    return null;
  }
  const saveProject = (id, dir) => postProject("/api/projects", { id, dir });
  function renderProjects() {
    const box = $("agent-projects");
    box.replaceChildren();
    if (!Agent.connected || !Agent.projects) return;
    box.append(el("div", { class: "doc-head" }, el("strong", { text: "Source" }),
      el("span", { class: "hint", text: "The agent downloads each project's source code from GitHub itself. Nothing to set up." })));
    const busy = !!(Agent.current || Agent.queue.length);
    for (const p of Agent.projects) {
      const local = p.mode === "local";
      const name = p.name || p.id;
      const err = el("p", { class: "hint bad", role: "alert" });
      const run = async (fn) => { err.textContent = (await fn()) || ""; };
      let line;
      if (local) {
        const ok = p.exists && p.has_presets !== false;
        line = el("span", { class: ok ? "ok" : "bad", text: !p.exists ? `Your own folder ${p.dir} cannot be read on this PC.` : p.has_presets === false ? `Your own folder ${p.dir} has no CMakePresets.json.` : `Building from your own folder: ${p.dir}` });
      } else {
        const link = el("a", { href: p.repo, target: "_blank", rel: "noopener", text: String(p.repo || "").replace(/^https:\/\/github.com\//, "") });
        const cache = p.cached ? `cached: ${p.cache_mb} MB${p.head ? ", at " + String(p.head).slice(0, 7) : ""}` : "not downloaded yet";
        line = el("span", {}, "Downloaded from ", link, " on the first build, then kept up to date (", cache, ")");
      }
      const input = el("input", { type: "text", value: p.dir || "", placeholder: `e.g. C:\\code\\${p.id}`, size: "52", "aria-label": `Your own folder for ${name}` });
      input.addEventListener("keydown", (e) => { if (e.key === "Enter") run(() => saveProject(p.id, input.value.trim())); });
      const own = el("details", { class: "own" }, el("summary", { text: "Use my own copy instead (advanced)" }),
        el("div", { class: "row" }, input, el("button", { type: "button", class: "ghost", text: "Save", onclick: () => run(() => saveProject(p.id, input.value.trim())) }),
          local ? el("button", { type: "button", class: "ghost", text: "Use the download again", onclick: () => run(() => saveProject(p.id, "")) }) : ""));
      if (local) own.open = true;
      const clear = el("button", { type: "button", class: "ghost", text: "Delete the downloaded copy", onclick: () => run(() => postProject("/api/projects/clear-cache", { id: p.id })) });
      clear.disabled = busy || !p.cached;
      if (busy) clear.title = "Not while a build is running";
      box.append(el("div", { class: "proj" }, el("div", { class: "nm" }, el("strong", { text: name }), line), own, el("div", { class: "row" }, clear), err));
    }
  }
  Agent.note =(msg) => say(msg, "ok");
  Agent.setCompiler =function (id) { if (Agent.compiler !== id) { Agent.compiler = id; renderPlatforms(); } };

  function renderDoctor() {
    const box = $("agent-doctor");
    box.replaceChildren();
    const r = Agent.connected && Agent.watch && Agent.reports[Agent.watch];
    if (!r) return;
    const family = { vc6: "Visual C++ 6", msvc: "Visual Studio", mingw: "MinGW-w64", linux: "Linux", macos: "macOS" }[r.family] || r.family;
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

  /* The player's game folder: the agent looks there for binkw32.dll and mss32.dll. Optional; nothing is assumed. */
  Agent.data = null;
  Agent.embed = store.get("embed-dlls") === "1";
  async function loadData(dir) {
    const r = dir === undefined ? await call("/api/data") : await call("/api/data", { method: "POST", body: JSON.stringify({ dir }) });
    if (r.status === 200) { Agent.data = r.body; renderData(); changed(); return null; }
    return (r.body && r.body.error) || "status " + r.status;
  }
  function renderData() {
    const box = $("agent-data");
    box.replaceChildren();
    if (!Agent.connected || !Agent.data) return;
    const d = Agent.data, input = el("input", { type: "text", id: "data-dir", value: d.dir || "", placeholder: "e.g. C:\\Program Files (x86)\\EA Games\\Command and Conquer Generals Zero Hour", size: "52", "aria-label": "Your game folder" });
    const err = el("p", { class: "hint bad", role: "alert" });
    const save = async () => { err.textContent = (await loadData(input.value.trim())) || ""; };
    const check = el("input", { type: "checkbox", id: "embed-dlls", onchange: (e) => { Agent.embed = e.target.checked; store.set("embed-dlls", Agent.embed ? "1" : null); } });
    check.checked = Agent.embed;
    const state = !d.dir ? "No game folder set." : !d.exists ? "That folder cannot be read on this PC."
      : d.ready ? "Found binkw32.dll and mss32.dll." : "Not found in that folder: " + d.missing.join(", ") + ".";
    box.append(
      el("div", { class: "doc-head" }, el("strong", { text: "Your game folder (optional)" }),
        el("span", { class: d.ready ? "ok" : "hint", text: state })),
      el("div", { class: "row" }, input, el("button", { type: "button", class: "ghost", text: "Save", onclick: save })),
      el("label", { class: "hint" }, check, " Put binkw32.dll and mss32.dll inside the exe (Visual C++ 6 and Visual Studio builds). ",
        d.ready ? "" : "Without both files the build still goes ahead and says it did not embed them."),
      err);
    input.addEventListener("keydown", (e) => { if (e.key === "Enter") save(); });
  }

  Agent.resultFor = (id) => Agent.results.find((r) => r.id === id);

  /* The browser does the download itself (progress bar, straight to disk): the agent gives a one-time link for it. */
  async function fetchFile(id, exe) {
    const t = await call("/api/ticket?id=" + encodeURIComponent(id), { method: "POST" });
    if (t.status !== 200) { say("Download failed (" + ((t.body && t.body.error) || t.status) + ").", "bad"); return false; }
    const q = (exe ? `exe=${encodeURIComponent(exe)}&` : "") + `ticket=${t.body.ticket}`;
    const a = el("a", { href: `${Agent.url}/api/download/${encodeURIComponent(id)}?${q}`, download: exe || id + ".zip" });
    document.body.append(a); a.click(); a.remove();
    return true;
  }
  Agent.download = async function (id) {
    const res = Agent.resultFor(id), mb = res && res.size ? ` (${Math.round(res.size / 1e6)} MB)` : "";
    if (await fetchFile(id)) say(`Downloading ${id}.zip${mb}: see your browser's downloads.`, "ok");
  };
  /* Only the main executable, as a plain file (not zipped). */
  Agent.downloadExe = async function (id, name) {
    const res = Agent.resultFor(id), x = res && (res.exes || []).find((e) => e.name === name);
    const mb = x && x.size ? ` (${Math.round(x.size / 1e6)} MB)` : "";
    if (await fetchFile(id, name)) say(`Downloading ${name}${mb}…`, "ok");
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
    const os = /Windows/i.test(navigator.userAgent || "") ? "windows" : /Mac/i.test(navigator.userAgent || "") ? "mac" : "linux";
    if (os !== "windows") $("os-note").hidden = false;
    for (const li of document.querySelectorAll("#agent-downloads li")) if (li.dataset.os === os) { li.style.fontWeight = "600"; $("agent-downloads").prepend(li); } // this computer's agent first
    $("agent-connect").addEventListener("click", () => connect($("agent-url").value.trim(), $("agent-token").value.trim()));
    $("agent-disconnect").addEventListener("click", disconnect);
    $("agent-restart").addEventListener("click", () => Agent.exit(true));
    $("agent-quit").addEventListener("click", () => Agent.exit(false));
    // The page is served by the agent itself (same origin): ask it for its token and connect, with nothing to type.
    if (!hashToken && await servedByAgent()) {
      try {
        const r = await fetch("/api/session", { cache: "no-store" });
        if (r.ok) {
          const t = (await r.json()).token;
          $("agent-url").value = location.origin; $("agent-token").value = t;
          if (await connect(location.origin, t)) return;
        }
      } catch (e) { /* fall through to the manual way */ }
    }
    const saved = store.get("agent-url"), savedToken = store.get("agent-token");
    // A link with a token comes from the agent itself, so its own address is the agent's address.
    $("agent-url").value = hashToken ? location.origin : saved || "http://127.0.0.1:8787";
    if (hashToken || savedToken) {
      $("agent-token").value = hashToken || savedToken;
      connect($("agent-url").value, $("agent-token").value);
    } else renderQueue();
  }
  if (document.readyState === "loading") document.addEventListener("DOMContentLoaded", init); else init();
})();
