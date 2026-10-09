"use strict";
/* Build portal front end. Static: reads data/schema.json (matrix.yaml) and data/builds.json.
 * resolve() mirrors scripts/portal.py; scripts/test_portal.py checks they agree. */

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

/* ---------------------------------------------------------------- sha1 (sync, works on file://) */
function sha1hex(str) {
  const bytes = new TextEncoder().encode(str);
  const words = [];
  for (let i = 0; i < bytes.length; i++) words[i >> 2] |= bytes[i] << (24 - (i % 4) * 8);
  const bitLen = bytes.length * 8;
  words[bitLen >> 5] |= 0x80 << (24 - (bitLen % 32));
  words[(((bitLen + 64) >> 9) << 4) + 15] = bitLen;
  const rol = (x, n) => (x << n) | (x >>> (32 - n));
  let h0 = 0x67452301, h1 = 0xefcdab89, h2 = 0x98badcfe, h3 = 0x10325476, h4 = 0xc3d2e1f0;
  const w = new Array(80);
  for (let i = 0; i < words.length; i += 16) {
    for (let t = 0; t < 80; t++) w[t] = t < 16 ? words[i + t] | 0 : rol(w[t - 3] ^ w[t - 8] ^ w[t - 14] ^ w[t - 16], 1);
    let a = h0, b = h1, c = h2, d = h3, e = h4;
    for (let t = 0; t < 80; t++) {
      let f, k;
      if (t < 20) { f = (b & c) | (~b & d); k = 0x5a827999; }
      else if (t < 40) { f = b ^ c ^ d; k = 0x6ed9eba1; }
      else if (t < 60) { f = (b & c) | (b & d) | (c & d); k = 0x8f1bbcdc; }
      else { f = b ^ c ^ d; k = 0xca62c1d6; }
      const tmp = (rol(a, 5) + f + e + k + w[t]) | 0;
      e = d; d = c; c = rol(b, 30); b = a; a = tmp;
    }
    h0 = (h0 + a) | 0; h1 = (h1 + b) | 0; h2 = (h2 + c) | 0; h3 = (h3 + d) | 0; h4 = (h4 + e) | 0;
  }
  return [h0, h1, h2, h3, h4].map((x) => (x >>> 0).toString(16).padStart(8, "0")).join("");
}

/* ---------------------------------------------------------------- resolve (mirror of portal.py) */
function condOk(c, ctx) {
  if (c.compiler && !c.compiler.includes(ctx.compiler)) return false;
  if (c.config && !c.config.includes(ctx.config)) return false;
  if (c.game && !c.game.includes(ctx.game)) return false;
  if (c.switch) {
    const v = ctx.values[c.switch];
    if (c.in && !c.in.includes(v)) return false;
    if (c.not_in && c.not_in.includes(v)) return false;
  }
  return true;
}

/* The project is part of the hashed text only for projects other than the first, so the first project's ids never change. */
function buildId(game, compiler, config, switches, project) {
  let base = `${game}-${compiler}-${config}`;
  if (project) base = `${project}-${base}`;
  const keys = Object.keys(switches).sort();
  if (!keys.length) return base;
  const sorted = {};
  keys.forEach((k) => (sorted[k] = switches[k]));
  const obj = project ? { compiler, config, game, project, switches: sorted } : { compiler, config, game, switches: sorted };
  return `${base}-${sha1hex(JSON.stringify(obj)).slice(0, 8)}`;
}

/* "" = the user's own folder as it is (local mode only), "latest", or a commit id: the build id gets "", "-latest" or "-c" + 7 characters. null = not valid. */
const SHA_RE = /^[0-9a-fA-F]{7,40}$/;
function refSuffix(ref) {
  if (!ref) return "";
  if (ref === "latest") return "-latest";
  if (typeof ref === "string" && SHA_RE.test(ref)) return "-c" + ref.slice(0, 7).toLowerCase();
  return null;
}

const projectOf = (m, id) => (id == null ? m.projects[0] : m.projects.find((p) => p.id === id));
function projectPresets(m, proj) {
  const out = {};
  for (const c of m.configs) {
    const d = {};
    for (const [k, v] of Object.entries(c.presets)) if (proj.compilers.includes(k)) d[k] = v;
    Object.assign(d, (proj.presets || {})[c.id] || {});
    out[c.id] = d;
  }
  return out;
}
const projectSwitches = (m, proj) => m.switches.filter((s) => !s.projects || s.projects.includes(proj.id));
const switchAllowed = (s) => (s.type === "tristate" ? ["DEFAULT", "ON", "OFF"] : s.type === "choice" ? s.choices.map((c) => c.id) : ["ON", "OFF"]);

function resolve(m, sel) {
  const errors = [], warnings = [], disabled = {};
  const { game, compiler, config } = sel;
  const user = sel.switches || {};
  const ref = sel.ref || "";
  const proj = projectOf(m, sel.project);
  if (!proj) return { valid: false, errors: [`unknown project '${sel.project}'`], warnings, disabled };
  const games = Object.fromEntries(m.games.map((g) => [g.id, g]));
  const compilers = Object.fromEntries(m.compilers.map((c) => [c.id, c]));
  const configs = Object.fromEntries(m.configs.map((c) => [c.id, c]));
  const psw = projectSwitches(m, proj);
  const inProj = new Set(psw.map((s) => s.id));
  const swById = Object.fromEntries(m.switches.map((s) => [s.id, s]));
  if (!games[game] || !compilers[compiler] || !proj.compilers.includes(compiler) || !configs[config]) return { valid: false, errors: ["unknown selection"], warnings, disabled };
  for (const k of Object.keys(user)) if (!inProj.has(k)) errors.push(`unknown switch '${k}'`);
  const suffix = refSuffix(ref);
  if (suffix === null) errors.push("Choose a commit, or type a valid commit id (7 to 40 hex digits).");
  if (errors.length) return { valid: false, errors, warnings, disabled };

  const preset = projectPresets(m, proj)[config][compiler];
  if (!preset) errors.push(`${configs[config].name} is not available with ${compilers[compiler].name}`);
  else if (!((proj.preset_games || {})[preset] || Object.keys(games)).includes(game)) errors.push(`${games[game].name} is not built by the ${preset} preset`);
  const pvals = (proj.preset_values || {})[preset] || {};
  const defaults = {};
  for (const s of m.switches) defaults[s.id] = pvals[s.id] ?? s.default;

  const values = {};
  const ctx = { compiler, config, game, values };
  for (const s of m.switches) {
    const d = defaults[s.id];
    if (!inProj.has(s.id)) { values[s.id] = d; continue; }
    let want = user[s.id] ?? d;
    const allowed = switchAllowed(s);
    if (!allowed.includes(want)) { errors.push(`${s.id}: ${want} is not one of ${allowed}`); want = d; }
    values[s.id] = want;
    if (!(s.when || []).every((c) => condOk(c, ctx))) {
      disabled[s.id] = s.why || "not available in this configuration";
      if (want !== d) errors.push(`${s.id}: ${disabled[s.id]}`);
      values[s.id] = s.type === "bool" ? "OFF" : d;
    } else if (want !== d && s.warn) warnings.push(`${s.id}: ${s.warn}`);
  }
  for (const r of m.rules || []) if (condOk(r.if, ctx) && !condOk(r.require, ctx)) errors.push(r.message);

  const switches = {};
  for (const s of psw) if (!(s.id in disabled) && values[s.id] !== defaults[s.id]) switches[s.id] = values[s.id];
  const eff = (i) => (i in disabled || !inProj.has(i) ? swById[i].default : values[i]);
  const retail = compilers[compiler].retail_crc && proj.retail_crc !== false && eff("retail_compat") !== "OFF" && eff("compat_crc") !== "OFF" && eff("compat_aigroup") !== "OFF";
  if (!compilers[compiler].retail_crc && inProj.has("retail_compat") && values.retail_compat !== "OFF") warnings.push("Not CRC-compatible with retail, so it cannot play online against retail clients.");

  const pid = proj.id === m.projects[0].id ? null : proj.id;
  const out = { valid: errors.length === 0, errors, warnings, disabled, project: proj.id, game, compiler, config, ref, switches, defaults, retail_crc: !!retail, values, id: buildId(game, compiler, config, switches, pid) + suffix };
  if (preset && !errors.length) {
    let p = preset;
    for (const sw of proj.preset_swaps || []) if (sw.map[p] && condOk(sw.if, { values })) { p = sw.map[p]; break; }
    if (values.ffmpeg === "ON") p = m.vcpkg_presets[p] || p;
    const c = compilers[compiler];
    const args = Object.entries(games[game].cmake).map(([k, v]) => `-D${k}=${v}`);
    const defines = [];
    const prefix = c.flag_prefix ?? "/";
    for (const [k, v] of Object.entries(switches)) {
      if (swById[k].cmake) args.push(`-D${swById[k].cmake}=${v}`);
      else defines.push(`${prefix}D${swById[k].define}=${v === "ON" ? 1 : 0}`);
    }
    if (defines.length) { const base = c.flags_base ?? m.flags_base; args.push("-DRTS_FLAGS=" + [...(base ? [base] : []), ...defines].join(";")); }
    const shown = args.map((a) => (a.startsWith("-DRTS_FLAGS=") ? `-DRTS_FLAGS="${a.slice(12)}"` : a));
    out.preset = p;
    out.cmake_args = args;
    out.ci_game = games[game].ci_name;
    out.command = ["cmake", "--preset", p, ...shown].join(" ");
  }
  return out;
}

/* ---------------------------------------------------------------- state and address hash */
let M, B, byId;
const state = { project: "ggc", game: "universal", compiler: "vc6", config: "release", switches: {}, refMode: "latest", sha: "" };

/* The ref the page asks for: "latest" (the default), a commit id as typed, or "" (my own folder, local mode only: refMode "local"). */
const refFor = (st) => (st.refMode === "commit" ? st.sha || "" : st.refMode === "local" ? "" : "latest");
/* The published (CI) build that matches a resolved build: only for the latest source, never a commit or a local folder. */
const publishedOf = (r) => (r.valid && r.ref === "latest" ? byId[r.id.slice(0, -refSuffix("latest").length)] : r.valid && !r.ref ? byId[r.id] : null);

/* The address hash for a state, and back. parseHash only returns what the hash says. */
function hashOf(st) {
  const p = new URLSearchParams({ project: st.project, game: st.game, compiler: st.compiler, config: st.config });
  const sw = Object.entries(st.switches).map(([k, v]) => `${k}:${v}`).join(",");
  if (sw) p.set("sw", sw);
  if (st.refMode === "local") p.set("ref", "local");
  else if (st.refMode === "commit" && st.sha) p.set("ref", st.sha);
  return p.toString();
}
function parseHash(hash) {
  const p = new URLSearchParams(hash.replace(/^#/, "").replace(/^configure\?/, ""));
  const out = { switches: {}, refMode: "latest", sha: "" };
  for (const k of ["project", "game", "compiler", "config"]) if (p.get(k)) out[k] = p.get(k);
  (p.get("sw") || "").split(",").filter(Boolean).forEach((kv) => { const [k, v] = kv.split(":"); out.switches[k] = v; });
  const ref = p.get("ref") || "";
  if (ref === "local") out.refMode = "local";
  else if (SHA_RE.test(ref)) { out.refMode = "commit"; out.sha = ref; }
  return out;
}
function readHash() {
  const h = parseHash(location.hash);
  for (const k of ["project", "game", "compiler", "config"]) if (h[k]) state[k] = h[k];
  state.switches = h.switches;
  state.refMode = h.refMode;
  state.sha = h.sha;
}
function writeHash() { history.replaceState(null, "", "#" + hashOf(state)); }
function loadBuild(b) {
  Object.assign(state, { project: b.project || "ggc", game: b.game, compiler: b.compiler, config: b.config, switches: { ...b.switches }, refMode: "latest", sha: "" });
  render();
  $("configure").scrollIntoView();
}

/* ---------------------------------------------------------------- rendering */
const curProject = () => projectOf(M, state.project);
const ownerId = () => (state.project === M.projects[0].id ? null : state.project);
const projectSwitchList = () => projectSwitches(M, curProject());
const projectCompilers = () => M.compilers.filter((c) => curProject().compilers.includes(c.id));
const presetFor = (config, compiler) => projectPresets(M, curProject())[config][compiler];
const projectConfigs = () => M.configs.filter((c) => Object.keys(projectPresets(M, curProject())[c.id]).length);
/* The job the agent is sent for a resolved build; a MinGW compiler also says which toolchain to use. */
const jobOf = (r, extra = {}) => {
  const toolchain = (M.compilers.find((c) => c.id === r.compiler) || {}).toolchain;
  return { id: r.id, project: r.project, ref: r.ref || "", preset: r.preset, game: r.ci_game, args: r.cmake_args, ...(toolchain ? { toolchain } : {}), ...extra };
};
const shortCompiler = (id) => ({ vc6: "VC6", msvc: "MSVC", "mingw-clang": "Clang", "mingw-gcc": "GCC", linux: "Linux", macos: "macOS" }[id] || id);
const compName = (id) => M.compilers.find((c) => c.id === id).name;
const cfgName = (id) => M.configs.find((c) => c.id === id).name;
const GAME_SHORT = { universal: "Both", zh: "ZH", generals: "Gen" };
const shortName = (b) => `${GAME_SHORT[b.game]} · ${shortCompiler(b.compiler)} · ${cfgName(b.config)}`;
const selRef = () => { const r = refFor(state); return state.refMode === "commit" && !r ? "none" : r; };
const resolveSel = (over = {}) => resolve(M, { project: state.project, game: state.game, compiler: state.compiler, config: state.config, switches: state.switches, ref: selRef(), ...over });

/* Why this PC cannot build the selection right now, or null when it can (or when no agent is connected). */
function blockReason(r) {
  const ag = window.PortalAgent;
  if (!r.valid || !ag || !ag.connected) return null;
  const ready = ag.readiness(r.preset);
  return ready.ready ? null : "Not ready: " + ready.problems.map((p) => p.name).join(", ");
}

function seg(container, items, current, onPick, isDisabled) {
  container.replaceChildren(...items.map(([id, label]) => {
    const b = el("button", { type: "button", role: "radio", "aria-checked": String(id === current), text: label });
    if (isDisabled && isDisabled(id)) b.disabled = true;
    b.addEventListener("click", () => onPick(id));
    return b;
  }));
}

function statusOf(r) {
  const ag = window.PortalAgent;
  if (r.valid && ag && ag.connected && ag.current === r.id) return { cls: "info", text: "Building on your PC…" };
  if (r.valid && ag && ag.connected && ag.queue.includes(r.id)) return { cls: "info", text: "Queued on your PC" };
  const mine = r.valid && ag && ag.connected && ag.resultFor(r.id);
  if (mine && mine.ok && mine.file) return { cls: "ok", text: "Built on your PC" };
  if (mine && !mine.ok) return { cls: "bad", text: "Failed on your PC" };
  if (!r.valid) return { cls: "bad", text: "Not a valid combination" };
  const b = publishedOf(r);
  if (b && b.status === "built") return { cls: "ok", text: "Ready to download" };
  if (b && b.status === "failed") return { cls: "bad", text: "Last build failed" };
  if (b) return { cls: "info", text: "Scheduled, not built yet" };
  if (r.valid && ag && ag.connected) return { cls: "info", text: "Not built on your PC yet" };
  return { cls: "info", text: "Not built. Connect your PC to build it." };
}

/* One button per main executable of a build that is done on this PC, labelled with the file name. */
function exeButtons(id) {
  const ag = window.PortalAgent;
  const done = ag && ag.connected && ag.resultFor(id);
  if (!done || !done.ok || ag.current === id || ag.queue.includes(id)) return [];
  return (done.exes || []).map((x) => el("button", { type: "button", class: "btn alt", text: `Download ${x.name}`, onclick: () => ag.downloadExe(id, x.name) }));
}

function renderResult(r) {
  const st = statusOf(r);
  $("status").className = "status " + st.cls;
  $("status").textContent = st.text;
  $("res-id").textContent = r.valid ? r.id : "";
  const msgs = $("res-msgs");
  msgs.replaceChildren(...r.errors.map((e) => el("li", { class: "err", text: e })), ...r.warnings.map((w) => el("li", { class: "warn", text: w })));
  const acts = $("res-actions");
  acts.replaceChildren();
  const b = publishedOf(r);
  // Published downloads only exist for a real published run; the sample data has no files behind its links.
  if (b && b.status === "built" && B.commit && B.commit !== "demo-sample") {
    for (const f of b.files || []) acts.append(el("a", { class: "btn", href: f.url, text: `Download ${f.name}` }));
    if (b.log_url) acts.append(el("a", { class: "btn alt", href: b.log_url, text: "Build log" }));
  } else if (r.valid && !b && M.repo) {
    const title = `Build request: ${r.id}`;
    const body = `Please build this configuration.\n\n- id: \`${r.id}\`\n- command: \`${r.command}\`\n`;
    acts.append(el("a", { class: "btn", href: `https://github.com/${M.repo}/issues/new?labels=build-request&title=${encodeURIComponent(title)}&body=${encodeURIComponent(body)}`, text: "Request this build" }));
  }
  const ag = window.PortalAgent;
  if (r.valid && ag && ag.connected) ag.setPreset(r.preset);
  if (r.valid && ag && ag.connected) {
    const done = ag.resultFor(r.id);
    const running = ag.current === r.id, queued = ag.queue.includes(r.id);
    // A rebuild deletes the old zip when it starts, so nothing is offered for download while the build is going.
    if (done && done.ok && done.file && !running && !queued) acts.append(el("button", { type: "button", class: "btn", text: "Download from my PC", onclick: () => ag.download(r.id) }), ...exeButtons(r.id));
    if (running || queued) {
      const busy = el("button", { type: "button", class: "btn" }, running ? "Building on my PC…" : "Queued on my PC…");
      busy.disabled = true;
      acts.append(busy, el("button", { type: "button", class: "btn alt", text: "Cancel", onclick: () => ag.cancel() }));
    } else {
      const why = blockReason(r);
      const buildBtn = el("button", { type: "button", class: done && done.ok ? "btn alt" : "btn", text: done ? "Rebuild on my PC" : "Build on my PC",
        onclick: () => ag.queueBuild(jobOf(r)) });
      buildBtn.disabled = !!why; // a property, not an attribute: setAttribute("disabled", anything) always disables
      if (why) buildBtn.title = why;
      acts.append(buildBtn);
    }
    if (done && !done.ok && !running && !queued) acts.append(el("p", { class: "not-ready" }, "The last build failed: " + (done.error || "no reason given")));
    const why = blockReason(r);
    if (why && !(running || queued)) acts.append(el("p", { class: "not-ready" }, why + ". ", el("a", { href: "#desktop", text: "See what to fix" })));
  } else if (r.valid) {
    acts.append(el("a", { class: "btn alt", href: "#desktop", text: "Build on my PC…" }));
  }
  renderRange(r);
  $("res-cmd").textContent = r.valid ? r.command : "Fix the problems above to see the build command.";
  $("copy-cmd").disabled = !r.valid;
  const kv = $("res-kv");
  kv.replaceChildren();
  const add = (k, v) => kv.append(el("dt", { text: k }), el("dd", { text: v }));
  if (r.valid) {
    add("Project", curProject().name);
    add("Source version", refLabel(r.ref));
    const mine = ag && ag.connected && ag.resultFor(r.id);
    if (mine && mine.sha) add("Built from", mine.sha.slice(0, 10) + (mine.label ? ` (${mine.label})` : ""));
    add("Retail CRC", r.retail_crc ? "compatible" : "not compatible");
    add("Preset", r.preset);
    add("Changed switches", Object.keys(r.switches).length ? Object.entries(r.switches).map(([k, v]) => `${k}=${v}`).join(", ") : "none");
    if (b && b.commit) add("Commit", b.commit.slice(0, 10));
    if (b && b.built_at) add("Built", b.built_at);
  }
}

/* ---- source version ---- */
function branchName() {
  const ag = window.PortalAgent, d = ag && ag.commits && ag.commits[state.project];
  return (d && d.branch) || curProject().branch || "main";
}
function refLabel(ref) {
  if (!ref) return "your own folder as it is";
  if (ref === "latest") {
    const ag = window.PortalAgent, d = ag && ag.commits && ag.commits[state.project];
    return `latest ${branchName()}` + (d && d.head ? ` (${d.head.slice(0, 7)})` : "");
  }
  return `commit ${ref.slice(0, 7)}`;
}

let commitSig = "";
function renderRef() {
  const ag = window.PortalAgent, on = !!(ag && ag.connected);
  const local = on && ag.isLocal && ag.isLocal(state.project);
  if (state.refMode === "local" && on && ag.projects && !local) state.refMode = "latest"; // that project builds from the download
  const modes = [["latest", `Latest ${branchName()}`], ["commit", "A specific commit…"], ...(local || state.refMode === "local" ? [["local", "My own folder as it is"]] : [])];
  seg($("seg-ref"), modes, state.refMode, (v) => { state.refMode = v; if (v === "commit" && !state.sha && ag && ag.commits && ag.commits[state.project] && ag.commits[state.project].commits[0]) state.sha = ag.commits[state.project].commits[0].sha; render(); },
    (v) => !on && v !== state.refMode);
  const d = on && ag.commits ? ag.commits[state.project] : null;
  const box = $("ref-commit");
  box.hidden = state.refMode !== "commit";
  const input = $("ref-sha"), sel = $("ref-select");
  input.disabled = !on; sel.disabled = !on;
  if (document.activeElement !== input && input.value !== state.sha) input.value = state.sha;
  const sig = state.project + "|" + (d ? d.commits.map((c) => c.sha).join(",") : "");
  if (sig !== commitSig) {
    commitSig = sig;
    sel.replaceChildren(el("option", { value: "", text: d && d.commits.length ? "Recent commits…" : "No commits loaded" }),
      ...(d ? d.commits.map((c) => el("option", { value: c.sha, text: `${c.short || c.sha.slice(0, 7)}  ${c.subject}  (${(c.date || "").slice(0, 10)})` })) : []));
  }
  sel.value = d && d.commits.some((c) => c.sha === state.sha) ? state.sha : "";
  let hint = "";
  if (!on) hint = state.refMode === "latest" ? "Connect to your PC to choose a specific commit." : "";
  else if (d && d.loading) hint = "Loading the commit list…";
  else if (d && d.fetched === false) hint = "Could not reach GitHub; showing the commits already downloaded.";
  else if (state.refMode === "latest" && d && d.commits && d.commits[0]) hint = `Latest ${branchName()} is ${d.commits[0].short || d.commits[0].sha.slice(0, 7)}: ${d.commits[0].subject}`;
  else if (state.refMode === "latest") hint = `The source is downloaded from GitHub (${branchName()} branch) when you build.`;
  else if (state.refMode === "local") hint = "Builds whatever is in the folder you set under Source, uncommitted changes included.";
  $("ref-hint").textContent = state.refMode === "commit" && on && !hint ? "Pick a commit, or type any commit id (7 to 40 hex digits)." : hint;
  $("ref-hint").className = "hint" + (d && d.fetched === false ? " bad" : "");
}

/* ---- build across history ---- */
const RANGE_LIMIT = 100;
function rangeSpec() {
  return { unit: $("range-unit").value, from: parseInt($("range-from").value, 10), to: parseInt($("range-to").value, 10), step: parseInt($("range-step").value, 10) };
}
function rangeProblem(s) {
  if (![s.from, s.to, s.step].every(Number.isInteger)) return "Enter whole numbers.";
  if (s.from < 1) return "From must be at least 1.";
  if (s.to < s.from) return "To must not be lower than From.";
  if (s.step < 1) return "Every must be at least 1.";
  const n = Math.floor((s.to - s.from) / s.step) + 1;
  if (n > RANGE_LIMIT) return `That is ${n} builds; the limit is ${RANGE_LIMIT}. Use a larger interval.`;
  return null;
}
function rangePoints(s) { const out = []; for (let n = s.from; n <= s.to; n += s.step) out.push(n); return out; }

function renderRange(r) {
  const s = rangeSpec(), problem = rangeProblem(s), ag = window.PortalAgent;
  const pts = problem ? [] : rangePoints(s), tag = s.unit === "pr" ? "PR #" : "commit #";
  const show = pts.length > 6 ? [...pts.slice(0, 3), "…", ...pts.slice(-2)] : pts;
  $("range-preview").textContent = problem || `${pts.length} build${pts.length === 1 ? "" : "s"}: ${show.map((n) => (n === "…" ? n : tag + n)).join(", ")}`;
  $("range-preview").className = "hint" + (problem ? " bad" : "");
  const go = $("range-go");
  const why = blockReason(r);
  go.disabled = !(r.valid && !problem && ag && ag.connected && !why);
  go.title = !ag || !ag.connected ? "Connect to your PC first (see Build on my PC)" : why ? why + " (see Build on my PC)" : "";
}

function wireRange() {
  for (const id of ["range-unit", "range-from", "range-to", "range-step"]) $(id).addEventListener("input", render);
  $("range-go").addEventListener("click", () => {
    // History builds always use the repository's own history, so they are not tied to the source version chosen above.
    const r = resolveSel({ ref: "" }), s = rangeSpec();
    if (!r.valid || rangeProblem(s) || !window.PortalAgent.connected) return;
    window.PortalAgent.queueBuild(jobOf(r, { range: s }));
  });
}

const cap = (s) => s.charAt(0).toUpperCase() + s.slice(1);

function switchRow(s, r) {
  const off = s.id in r.disabled;
  const def = r.defaults[s.id];
  const val = off ? def : state.switches[s.id] ?? def;
  const row = el("div", { class: "sw" + (off ? " off" : "") + (val !== def ? " changed" : "") });
  row.append(el("div", { class: "name", text: (s.title || cap(s.id.replace(/_/g, " "))) + " " }, el("code", { text: s.define || s.cmake })));
  const values = s.type === "tristate" ? ["DEFAULT", "ON", "OFF"] : s.type === "choice" ? s.choices.map((c) => c.id) : ["ON", "OFF"];
  const label = (v) => (v === "DEFAULT" ? "Auto" : s.type === "choice" ? s.choices.find((c) => c.id === v).label : (s.labels && s.labels[v]) || v);
  const ctl = el("div", { class: "seg", role: "radiogroup", "aria-label": s.title || s.id });
  seg(ctl, values.map((v) => [v, label(v)]), val, (v) => { if (v === def) delete state.switches[s.id]; else state.switches[s.id] = v; render(); }, () => off);
  ctl.querySelectorAll("button").forEach((b, k) => { if (values[k] === def) { b.classList.add("src"); b.title = "As the source has it"; } });
  row.append(ctl, el("div", { class: "help", text: s.help }));
  if (off) row.append(el("div", { class: "why", text: s.why || "Not available here." }));
  else if (s.warn && val !== def) row.append(el("div", { class: "why", text: s.warn }));
  return row;
}

const changedCount = (items, r) => items.filter((s) => !(s.id in r.disabled) && (state.switches[s.id] ?? r.defaults[s.id]) !== r.defaults[s.id]).length;

function renderSwitches(r) {
  // The big switches are always on show; the many small ones are folded away below them.
  const all = projectSwitchList();
  const featured = all.filter((s) => s.featured), rest = all.filter((s) => !s.featured);
  const main = $("main-switch-list");
  main.replaceChildren(...featured.map((s) => switchRow(s, r)));
  const mc = changedCount(featured, r);
  $("main-count").textContent = mc ? `(${mc} changed)` : "";

  const root = $("switch-groups");
  const wasOpen = new Set([...root.querySelectorAll("details.group[open]")].map((d) => d.dataset.group));
  root.replaceChildren();
  for (const g of M.groups) {
    const items = rest.filter((x) => x.group === g.id);
    if (!items.length) continue;
    const changed = changedCount(items, r);
    const box = el("details", { class: "group", "data-group": g.id });
    if (wasOpen.has(g.id) || changed) box.open = true;
    box.append(el("summary", {}, g.name, " ", el("span", { class: "count", text: `${items.length} switches` + (changed ? `, ${changed} changed` : "") })));
    if (g.note) box.append(el("p", { class: "hint", text: g.note }));
    for (const s of items) box.append(switchRow(s, r));
    root.append(box);
  }
  const rc = changedCount(rest, r);
  $("adv-count").textContent = `(${rest.length} switches${rc ? `, ${rc} changed` : ""})`;
  // keep the folded section open while one of its switches is changed
  if (rc) $("advanced").open = true;
}

/* Every buildable cell of the matrix for the selected project and source version, with all switches as the source has them. */
function matrixCells() {
  const out = [];
  for (const c of projectCompilers()) for (const cfg of M.configs) for (const g of M.games) {
    if (!presetFor(cfg.id, c.id)) continue;
    const r = resolveSel({ game: g.id, compiler: c.id, config: cfg.id, switches: {} });
    if (r.valid) out.push({ g, c, cfg, r });
  }
  return out;
}

/* What happened to a cell on this PC: running, queued, built, failed, or null when the agent has not touched it. */
function localState(id) {
  const ag = window.PortalAgent;
  if (!ag || !ag.connected) return null;
  if (ag.current === id) return "running";
  if (ag.queue.includes(id)) return "queued";
  const res = ag.resultFor(id);
  return res ? (res.ok ? "built" : "failed") : null;
}


function renderMatrixSummary(cells) {
  const ag = window.PortalAgent, box = $("matrix-summary"), btn = $("matrix-build");
  const on = !!(ag && ag.connected);
  const count = { built: 0, failed: 0, running: 0, queued: 0, none: 0 };
  for (const x of cells) count[localState(x.r.id) || "none"]++;
  const buildable = cells.filter((x) => ag && ag.platformReady(x.c.id)).length;
  btn.disabled = !on || buildable === 0 || count.running + count.queued > 0;
  btn.textContent = on && buildable ? `Build the whole matrix on my PC (${buildable} builds)` : "Build the whole matrix on my PC";
  btn.title = !on ? "Connect to your PC first (see Build on my PC)" : "";
  box.className = "matrix-summary";
  if (!on) { box.textContent = ""; return; }
  const total = cells.length, busy = count.running + count.queued;
  let cls, text;
  if (count.failed) { cls = "bad"; text = `Matrix: ${count.failed} of ${total} FAILED, ${count.built} built${busy ? `, ${busy} still going` : ""}.`; }
  else if (busy) { cls = "info"; text = `Matrix: building… ${count.built} of ${total} done.`; }
  else if (count.built === total) { cls = "ok"; text = `Matrix: all ${total} builds compiled.`; }
  else if (buildable && count.built >= buildable) { cls = "ok"; text = `Matrix: all ${buildable} builds this PC can do compiled. ${total - buildable} need a platform that is not ready.`; }
  else if (count.built) { cls = "info"; text = `Matrix: ${count.built} of ${total} built on this PC, ${count.none} not built yet.`; }
  else { cls = "info"; text = `Matrix: nothing built on this PC yet. ${buildable} of ${total} can be built here.`; }
  box.classList.add(cls); box.textContent = text;
}

/* Only the selected project's cells, at the selected source version. */
function buildMatrix() {
  const ag = window.PortalAgent;
  if (!ag || !ag.connected) return;
  const cells = matrixCells();
  const todo = cells.filter((x) => ag.platformReady(x.c.id));
  const skipped = cells.length - todo.length;
  for (const x of todo) ag.queueBuild(jobOf(x.r));
  if (skipped) ag.note(`Queued ${todo.length} matrix builds. ${skipped} skipped because this PC is not ready for them (see Build platforms).`);
}

function renderGrid() {
  const grid = $("grid");
  $("matrix-title").textContent = `Matrix: ${curProject().name}`;
  const cells = matrixCells();
  const head = el("tr", {}, el("th", { text: "Compiler / configuration" }), ...M.games.map((g) => el("th", { text: g.name })));
  const rows = [];
  for (const c of projectCompilers()) for (const cfg of projectConfigs()) {
    if (!presetFor(cfg.id, c.id)) continue;
    const row = el("tr", {}, el("th", { text: `${c.name.split(" (")[0]} · ${cfg.name}` }));
    for (const g of M.games) {
      const x = cells.find((y) => y.g.id === g.id && y.c.id === c.id && y.cfg.id === cfg.id);
      if (!x) { row.append(el("td", { class: "na", text: "n/a" })); continue; }
      const loc = localState(x.r.id); // what happened on this PC; the matrix shows nothing else
      const status = loc ? { running: "running", queued: "planned", built: "built", failed: "failed" }[loc] : "none";
      const label = { running: "Building…", queued: "Queued", built: "✓ Built", failed: "✗ Failed" }[loc] || "Not built";
      const btn = el("button", { type: "button" }, label, el("span", { class: "sub", text: c.retail_crc && curProject().retail_crc !== false ? "retail CRC" : "not retail CRC" }));
      btn.addEventListener("click", () => { Object.assign(state, { game: g.id, compiler: c.id, config: cfg.id, switches: {} }); render(); $("configure").scrollIntoView(); });
      row.append(el("td", { class: status }, btn));
    }
    rows.push(row);
  }
  grid.replaceChildren(head, ...rows);
  renderMatrixSummary(cells);
}

function renderCards() {
  const quick = [
    ["universal", "vc6", "Universal", "Both games in one package, built with VC6. Retail-compatible: use this for online play."],
    ["universal", "msvc", "Universal", "Both games, modern compiler. Easier to debug; not retail-compatible."],
  ];
  const ag = window.PortalAgent, onPc = !!(ag && ag.connected);
  const realData = B.commit && B.commit !== "demo-sample"; // the sample data has no files behind its links
  const first = state.project === M.projects[0].id;
  $("quick-cards").replaceChildren(...quick.map(([g, c, n, blurb]) => {
    const r = resolveSel({ game: g, compiler: c, config: "release", switches: {} });
    if (!r.valid) return "";
    const b = first ? publishedOf(r) : null;
    const head = el("h4", { text: `${n} · ${shortCompiler(c) === "MSVC" ? "Modern MSVC" : shortCompiler(c)}` });
    const card = el("div", { class: "card" }, head, el("p", { text: blurb }));
    const row = el("div", { class: "row" });
    const loc = onPc ? localState(r.id) : null;
    const ready = onPc && ag.platformReady(c);
    const btn = (text, onclick, cls = "btn", disabled = false) => { const x = el("button", { type: "button", class: cls, text, onclick }); x.disabled = disabled; return x; };
    if (loc === "running" || loc === "queued") row.append(btn(loc === "running" ? "Building on my PC…" : "Queued on my PC…", null, "btn", true), btn("Cancel", () => ag.cancel(), "btn alt"));
    else if (onPc) {
      if (loc === "built") row.append(btn("Download from my PC", () => ag.download(r.id)), ...exeButtons(r.id));
      if (ready) row.append(btn(loc === "built" ? "Rebuild" : "Build on my PC", () => ag.queueBuild(jobOf(r)), loc === "built" ? "btn alt" : "btn"));
      else row.append(el("span", { class: "tag", text: "this PC is not ready for " + shortCompiler(c) }));
      if (loc === "failed") card.append(el("p", { class: "hint bad", text: "The last build failed: " + ((ag.resultFor(r.id) || {}).error || "no reason given") }));
    } else {
      if (realData && b && b.status === "built" && b.files && b.files[0]) row.append(el("a", { class: "btn", href: b.files[0].url, text: "Download" }));
      row.append(el("a", { class: "btn alt", href: "#desktop", text: "Build on my PC…" }));
    }
    card.append(row);
    return card;
  }));

  const groups = new Map();
  if (first) for (const b of B.builds.filter((x) => x.tier === 2)) {
    if (!groups.has(b.name)) groups.set(b.name, { blurb: b.blurb, items: [] });
    groups.get(b.name).items.push(b);
  }
  $("variant-cards").replaceChildren(...(first ? [...groups].map(([name, v]) => {
    const card = el("div", { class: "card" }, el("h4", { text: name }), el("p", { text: v.blurb }));
    const row = el("div", { class: "row" });
    for (const b of v.items) {
      const t = el("button", { type: "button", class: "ghost", text: shortName(b), onclick: () => loadBuild(b) });
      row.append(t);
    }
    card.append(row);
    return card;
  }) : [el("p", { class: "hint", text: `Curated variants are prepared for ${M.projects[0].name}. For ${curProject().name}, use the configurator above.` })]));
}

function renderProject() {
  const p = curProject();
  seg($("seg-project"), M.projects.map((x) => [x.id, x.name]), state.project, (v) => {
    if (v === state.project) return;
    Object.assign(state, { project: v, switches: {}, refMode: state.refMode, sha: "" });
    render();
  });
  $("project-note").replaceChildren(el("a", { href: p.repo, target: "_blank", rel: "noopener", text: p.repo.replace(/^https:\/\/github.com\//, "") }), ` · branch ${p.branch}`);
}

function render() {
  // Keep the selection inside what the project has.
  const pc = projectCompilers();
  if (!pc.some((c) => c.id === state.compiler)) state.compiler = pc[0].id;
  if (!projectPresets(M, curProject())[state.config] || !presetFor(state.config, state.compiler)) state.config = "release";
  const allowed = (curProject().preset_games || {})[presetFor(state.config, state.compiler)];
  if (allowed && !allowed.includes(state.game)) state.game = allowed[0]; // that preset builds only some of the games
  if (window.PortalAgent) { window.PortalAgent.setProject(state.project); window.PortalAgent.setCompiler(state.compiler); }
  const r = resolveSel();

  renderProject();
  renderRef();
  seg($("seg-game"), M.games.map((g) => [g.id, g.name]), state.game, (v) => { state.game = v; render(); },
    (id) => { const pr = presetFor(state.config, state.compiler); return !!pr && !((curProject().preset_games || {})[pr] || [id]).includes(id); });
  seg($("seg-compiler"), pc.map((c) => [c.id, c.name]), state.compiler, (v) => { state.compiler = v; render(); });
  $("compiler-note").textContent = M.compilers.find((c) => c.id === state.compiler).note;
  renderGrid(); renderCards();
  seg($("seg-config"), projectConfigs().map((c) => [c.id, c.name]), state.config, (v) => { state.config = v; render(); }, (id) => !presetFor(id, state.compiler));

  renderSwitches(r);
  renderResult(r);
  writeHash();
}

async function init() {
  try {
    if ((window.PORTAL_EMBEDDED || location.protocol === "file:") && window.PORTAL_DATA) throw new Error("file://");
    [M, B] = await Promise.all(["data/schema.json", "data/builds.json"].map((u) => fetch(u).then((r) => { if (!r.ok) throw new Error(u); return r.json(); })));
  } catch (e) {
    // Opened from disk (file://): fetch is blocked, use the copy embedded by data/portal-data.js.
    if (window.PORTAL_DATA) ({ schema: M, builds: B } = window.PORTAL_DATA);
    else {
      document.querySelector("main").prepend(el("p", { class: "tag bad", text: "Could not load build data. Run scripts/portal.py generate first." }));
      return;
    }
  }
  // The sample data describes builds nobody made: use published results only when they are real, and otherwise show nothing prefilled.
  byId = B.commit && B.commit !== "demo-sample" ? Object.fromEntries(B.builds.map((b) => [b.id, b])) : {};
  const real = B.commit && B.commit !== "demo-sample";
  $("commit-line").textContent = real ? `Built from commit ${B.commit.slice(0, 10)}.` : "";
  window.addEventListener("portal:agent", render);
  // Only a real press starts it: a script calling .click() has isTrusted === false and is ignored. Nothing else ever queues the matrix.
  $("matrix-build").addEventListener("click", (e) => { if (e.isTrusted) buildMatrix(); });
  window.addEventListener("portal:pick-compiler", (e) => {
    if (!projectCompilers().some((c) => c.id === e.detail)) return;
    state.compiler = e.detail; render(); $("configure").scrollIntoView();
  });
  $("ref-select").addEventListener("change", (e) => { if (e.target.value) { state.sha = e.target.value; render(); } });
  $("ref-sha").addEventListener("input", (e) => { state.sha = e.target.value.trim(); render(); });
  wireRange();
  readHash();
  if (!projectOf(M, state.project)) state.project = M.projects[0].id;
  renderGrid();
  renderCards();
  render();
  $("reset").addEventListener("click", () => { state.switches = {}; render(); });
  $("copy-cmd").addEventListener("click", () => { try { navigator.clipboard.writeText($("res-cmd").textContent); } catch (e) { /* clipboard unavailable */ } });
  window.addEventListener("hashchange", () => { readHash(); render(); });
}
if (typeof document !== "undefined" && document.getElementById("grid")) init();
if (typeof module !== "undefined") module.exports = { resolve, buildId, sha1hex, refSuffix, hashOf, parseHash };
