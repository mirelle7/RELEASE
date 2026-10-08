"use strict";
/* Two panels that live on the page:
 *   - PR triangulator: paste PR data or fetch open PRs from GitHub, then see duplicates / conflicts / related PRs.
 *   - Desktop agent: connect to tools/agent.py running on your own PC and queue builds from the configurator.
 * Exposes window.PortalAgent for app.js. */
(function () {
  const $ = (id) => document.getElementById(id);
  const el = (tag, props = {}, ...kids) => {
    const n = document.createElement(tag);
    for (const [k, v] of Object.entries(props)) {
      if (k === "class") n.className = v;
      else if (k === "text") n.textContent = v;
      else if (k.startsWith("on")) n.addEventListener(k.slice(2), v);
      else n.setAttribute(k, v);
    }
    for (const c of kids) n.append(c);
    return n;
  };
  const store = {
    get(k) { try { return sessionStorage.getItem(k); } catch (e) { return null; } },
    set(k, v) { try { v == null ? sessionStorage.removeItem(k) : sessionStorage.setItem(k, v); } catch (e) { /* storage unavailable */ } },
  };

  /* ------------------------------------------------------------------ desktop agent */
  // `agent.py serve` prints a link ending in #agent-token=...; take the token and remove it from the address bar.
  let hashToken = null;
  const m = /(?:^#|[&#])agent-token=([\w-]+)/.exec(location.hash);
  if (m) { hashToken = m[1]; history.replaceState(null, "", location.pathname + location.search); }

  const Agent = { connected: false, url: null, token: null, current: null, queue: [], recent: [], timer: null, fails: 0 };
  window.PortalAgent = Agent;
  const changed = () => window.dispatchEvent(new CustomEvent("portal:agent"));

  async function call(path, opts = {}) {
    const headers = { "X-Agent-Token": Agent.token || "" };
    if (opts.body) headers["Content-Type"] = "application/json";
    const r = await fetch(Agent.url + path, { ...opts, headers });
    let body = null;
    try { body = await r.json(); } catch (e) { /* no body */ }
    return { status: r.status, body };
  }

  function say(msg, cls = "") { const s = $("agent-status"); s.textContent = msg; s.className = "agent-status " + cls; }

  async function connect(url, token) {
    url = url.replace(/\/+$/, "");
    Agent.url = url; Agent.token = token;
    say("Connecting…");
    try {
      const ping = await call("/api/ping");
      if (!ping.body || ping.body.agent !== "portal-build-agent") throw new Error("not an agent");
      const st = await call("/api/status");
      if (st.status === 401) { say("The agent is there, but the token is wrong. Copy it from the agent's window.", "bad"); return false; }
      if (st.status !== 200) throw new Error("status " + st.status);
      Agent.connected = true; Agent.fails = 0;
      store.set("agent-url", url); store.set("agent-token", token);
      apply(st.body);
      say("Connected" + (st.body.dry_run ? " (dry run: nothing will really be built)" : ""), "ok");
      $("agent-connect").hidden = true; $("agent-disconnect").hidden = false;
      clearInterval(Agent.timer); Agent.timer = setInterval(poll, 2000);
      changed();
      return true;
    } catch (e) {
      Agent.connected = false;
      const hosted = location.protocol === "https:" && !/^https:/.test(url);
      say("Could not reach an agent at " + url + ". Is it running? " +
        (location.origin !== url ? `If this page is hosted elsewhere, start the agent with --allow-origin ${location.origin}. ` : "") +
        (hosted ? "Some browsers (Safari) block a secure page from talking to a local address; open the link the agent printed instead." : ""), "bad");
      return false;
    }
  }

  function disconnect() {
    clearInterval(Agent.timer);
    Object.assign(Agent, { connected: false, current: null, queue: [], recent: [] });
    store.set("agent-token", null);
    $("agent-connect").hidden = false; $("agent-disconnect").hidden = true;
    say("Disconnected.");
    renderQueue(); changed();
  }

  function apply(s) {
    const finished = s.recent.length && (!Agent.recent.length || s.recent[0].at !== Agent.recent[0].at || s.recent[0].id !== Agent.recent[0].id);
    Object.assign(Agent, { current: s.current, queue: s.queue, recent: s.recent });
    renderQueue();
    if (finished && window.portalReload) window.portalReload();
  }

  async function poll() {
    try {
      const st = await call("/api/status");
      if (st.status !== 200) throw new Error("status " + st.status);
      Agent.fails = 0;
      const before = Agent.current + "|" + Agent.queue.join();
      apply(st.body);
      if (before !== Agent.current + "|" + Agent.queue.join()) changed();
    } catch (e) {
      if (++Agent.fails >= 3) { disconnect(); say("Lost contact with the agent.", "bad"); }
    }
  }

  Agent.queueBuild = async function (job) {
    const r = await call("/api/queue", { method: "POST", body: JSON.stringify(job) });
    if (r.status === 202) say(`Queued ${r.body.id} on your PC.`, "ok");
    else if (r.status === 200) say(`${r.body.id} is already queued.`, "");
    else say("The agent refused that build: " + ((r.body && r.body.error) || r.status), "bad");
    poll();
  };

  function renderQueue() {
    const box = $("agent-queue");
    box.replaceChildren();
    if (!Agent.connected) return;
    const row = (label, id, cls) => box.append(el("div", { class: "qrow " + cls }, el("span", { class: "tag " + cls, text: label }), el("code", { text: id })));
    if (Agent.current) row("building", Agent.current, "info");
    Agent.queue.forEach((id) => row("queued", id, ""));
    Agent.recent.slice(0, 5).forEach((r) => row(r.ok ? "done" : "failed", r.id, r.ok ? "ok" : "bad"));
    if (!box.children.length) box.append(el("p", { class: "hint", text: "Nothing queued. Pick a configuration above and press “Build on my PC”." }));
  }

  /* ------------------------------------------------------------------ triangulator */
  let lastRepo = null;

  function tri(msg, cls = "") { const n = $("tri-msg"); n.textContent = msg; n.className = "hint " + cls; }

  function renderTriangles(result) {
    const out = $("tri-out");
    out.replaceChildren();
    const showWeak = $("tri-weak").checked;
    const edges = result.edges.filter((e) => showWeak || e.label !== "weak");
    const title = (n) => result.prs[n] || "";
    const prLink = (n) => lastRepo
      ? el("a", { href: `https://github.com/${lastRepo}/pull/${n}`, target: "_blank", rel: "noopener noreferrer", text: "#" + n })
      : el("span", { text: "#" + n });
    if (result.groups.length) {
      out.append(el("p", { class: "tri-groups" }, el("strong", { text: "Groups of related PRs: " }),
        ...result.groups.map((g, i) => el("span", { class: "tri-group" }, ...g.flatMap((n, j) => j ? [", ", prLink(n)] : [prLink(n)]), i < result.groups.length - 1 ? "  ·  " : ""))));
    }
    if (!edges.length) { out.append(el("p", { class: "hint", text: "No related pull requests found." + (showWeak ? "" : " (Tick “show weak” to include single-signal matches.)") })); return; }
    for (const e of edges) {
      const cls = { duplicate: "bad", conflict: "warn", related: "info", weak: "" }[e.label];
      const chips = [["text", e.text.toFixed(2)], ["files", e.files.toFixed(2)], ["lines", e.overlapping_files.length ? "overlap" : "–"]]
        .map(([k, v]) => el("span", { class: "chip" + (e.signals[k] ? " on" : ""), text: `${k} ${v}` }));
      const card = el("div", { class: "tri-card" },
        el("div", { class: "tri-head" }, el("span", { class: "tag " + cls, text: e.label }), prLink(e.a), " ↔ ", prLink(e.b), el("span", { class: "chips" }, ...chips)),
        el("div", { class: "tri-titles" }, el("div", { text: title(e.a) }), el("div", { text: title(e.b) })));
      if (e.overlapping_files.length) card.append(el("div", { class: "tri-files", text: "Same lines touched in: " + e.overlapping_files.join(", ") }));
      else if (e.shared_files.length) card.append(el("div", { class: "tri-files", text: "Shared files: " + e.shared_files.join(", ") }));
      out.append(card);
    }
  }

  function runTri() {
    let prs;
    try { prs = JSON.parse($("tri-json").value); } catch (e) { tri("That is not valid JSON.", "bad"); return; }
    if (!Array.isArray(prs) || !prs.every((p) => p && Number.isInteger(p.number))) { tri("Expected a list of PRs, each with a number, title and files.", "bad"); return; }
    if (prs.length < 2) { tri("Need at least two pull requests to compare.", "bad"); return; }
    const result = window.Triangulator.triangulate(prs);
    tri(`Compared ${prs.length} pull requests (${prs.length * (prs.length - 1) / 2} pairs).`);
    renderTriangles(result);
    $("tri-out")._result = result;
  }

  async function fetchTri() {
    const repo = $("tri-repo").value.trim();
    tri("Fetching open pull requests…");
    $("tri-fetch").disabled = true;
    try {
      const prs = await window.Triangulator.fetchGitHub(repo, { token: $("tri-token").value.trim() || undefined });
      if (prs.length < 2) { tri(`Only ${prs.length} open pull request(s) in ${repo}; need at least two.`, "bad"); return; }
      lastRepo = repo;
      $("tri-json").value = JSON.stringify(prs, null, 1);
      runTri();
    } catch (e) {
      tri(e.message || "Could not fetch pull requests.", "bad");
    } finally {
      $("tri-fetch").disabled = false;
    }
  }

  // Is this page being served by an agent (opened without the token link)? Only worth asking on a loopback address.
  async function servedByAgent() {
    if (!/^(127\.0\.0\.1|localhost)$/.test(location.hostname)) return false;
    try { const r = await fetch("/api/ping"); return (await r.json()).agent === "portal-build-agent"; } catch (e) { return false; }
  }

  async function init() {
    if (!$("agent-connect")) return;
    $("agent-connect").addEventListener("click", () => connect($("agent-url").value.trim(), $("agent-token").value.trim()));
    $("agent-disconnect").addEventListener("click", disconnect);
    $("tri-run").addEventListener("click", () => { lastRepo = null; runTri(); });
    $("tri-fetch").addEventListener("click", fetchTri);
    $("tri-sample").addEventListener("click", () => { lastRepo = null; $("tri-json").value = JSON.stringify(window.SAMPLE_PRS, null, 1); runTri(); });
    $("tri-weak").addEventListener("change", () => { const r = $("tri-out")._result; if (r) renderTriangles(r); });

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
