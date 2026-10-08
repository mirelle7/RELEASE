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
    else n.setAttribute(k, v);
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
  if (c.switch) {
    const v = ctx.values[c.switch];
    if (c.in && !c.in.includes(v)) return false;
    if (c.not_in && c.not_in.includes(v)) return false;
  }
  return true;
}

function buildId(game, compiler, config, switches) {
  const base = `${game}-${compiler}-${config}`;
  const keys = Object.keys(switches).sort();
  if (!keys.length) return base;
  const sorted = {};
  keys.forEach((k) => (sorted[k] = switches[k]));
  const canon = JSON.stringify({ compiler, config, game, switches: sorted });
  return `${base}-${sha1hex(canon).slice(0, 8)}`;
}

function resolve(m, sel) {
  const errors = [], warnings = [], disabled = {};
  const { game, compiler, config } = sel;
  const user = sel.switches || {};
  const games = Object.fromEntries(m.games.map((g) => [g.id, g]));
  const compilers = Object.fromEntries(m.compilers.map((c) => [c.id, c]));
  const configs = Object.fromEntries(m.configs.map((c) => [c.id, c]));
  const swById = Object.fromEntries(m.switches.map((s) => [s.id, s]));
  if (!games[game] || !compilers[compiler] || !configs[config]) return { valid: false, errors: ["unknown selection"], warnings, disabled };

  const preset = configs[config].presets[compiler];
  if (!preset) errors.push(`${configs[config].name} is not available with ${compilers[compiler].name}`);

  const values = {};
  const ctx = { compiler, config, values };
  for (const s of m.switches) {
    let want = user[s.id] ?? s.default;
    const allowed = s.type === "tristate" ? ["DEFAULT", "ON", "OFF"] : ["ON", "OFF"];
    if (!allowed.includes(want)) { errors.push(`${s.id}: ${want} is not one of ${allowed}`); want = s.default; }
    values[s.id] = want;
    if (!(s.when || []).every((c) => condOk(c, ctx))) {
      disabled[s.id] = s.why || "not available in this configuration";
      if (want !== s.default) errors.push(`${s.id}: ${disabled[s.id]}`);
      values[s.id] = s.type === "bool" ? "OFF" : s.default;
    } else if (want !== s.default && s.warn) warnings.push(`${s.id}: ${s.warn}`);
  }
  for (const r of m.rules || []) if (condOk(r.if, ctx) && !condOk(r.require, ctx)) errors.push(r.message);

  const switches = {};
  for (const s of m.switches) if (!(s.id in disabled) && values[s.id] !== s.default) switches[s.id] = values[s.id];
  const retail = compilers[compiler].retail_crc && values.retail_compat !== "OFF";
  if (!retail && values.retail_compat !== "OFF") warnings.push("Not CRC-compatible with retail: " + compilers[compiler].note);

  const out = { valid: errors.length === 0, errors, warnings, disabled, game, compiler, config, switches, retail_crc: retail, values, id: buildId(game, compiler, config, switches) };
  if (preset && !errors.length) {
    const p = values.ffmpeg === "ON" ? (m.vcpkg_presets[preset] || preset) : preset;
    const args = Object.entries(games[game].cmake).map(([k, v]) => `-D${k}=${v}`);
    for (const [k, v] of Object.entries(switches)) args.push(`-D${swById[k].cmake}=${v}`);
    out.preset = p;
    out.cmake_args = args;
    out.ci_game = games[game].ci_name;
    out.command = ["cmake", "--preset", p, ...args].join(" ");
  }
  return out;
}

/* ---------------------------------------------------------------- state */
let M, B, byId;
const state = { game: "universal", compiler: "vc6", config: "release", switches: {} };

function readHash() {
  const p = new URLSearchParams(location.hash.slice(1).replace(/^configure\?/, ""));
  for (const k of ["game", "compiler", "config"]) if (p.get(k)) state[k] = p.get(k);
  state.switches = {};
  (p.get("sw") || "").split(",").filter(Boolean).forEach((kv) => { const [k, v] = kv.split(":"); state.switches[k] = v; });
}
function writeHash() {
  const sw = Object.entries(state.switches).map(([k, v]) => `${k}:${v}`).join(",");
  const p = new URLSearchParams({ game: state.game, compiler: state.compiler, config: state.config });
  if (sw) p.set("sw", sw);
  history.replaceState(null, "", "#" + p.toString());
}
function loadBuild(b) {
  Object.assign(state, { game: b.game, compiler: b.compiler, config: b.config, switches: { ...b.switches } });
  render();
  $("configure").scrollIntoView();
}

/* ---------------------------------------------------------------- rendering */
const gameName = (id) => M.games.find((g) => g.id === id).name;
const compName = (id) => M.compilers.find((c) => c.id === id).name;
const cfgName = (id) => M.configs.find((c) => c.id === id).name;
const GAME_SHORT = { universal: "Both", zh: "ZH", generals: "Gen" };
const shortName = (b) => `${GAME_SHORT[b.game]} · ${b.compiler === "vc6" ? "VC6" : "MSVC"} · ${cfgName(b.config)}`;

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
  const b = byId[r.id];
  if (b && b.status === "built") return { cls: "ok", text: "Ready to download" };
  if (b && b.status === "failed") return { cls: "bad", text: "Last build failed" };
  if (b) return { cls: "info", text: "Scheduled, not built yet" };
  return { cls: "info", text: "Not built. Can be built on request." };
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
  const b = r.valid && byId[r.id];
  if (b && b.status === "built") {
    for (const f of b.files || []) acts.append(el("a", { class: "btn", href: f.url, text: `Download ${f.name}` }));
    if (b.log_url) acts.append(el("a", { class: "btn alt", href: b.log_url, text: "Build log" }));
  } else if (r.valid && !b && M.repo) {
    const title = `Build request: ${r.id}`;
    const body = `Please build this configuration.\n\n- id: \`${r.id}\`\n- command: \`${r.command}\`\n`;
    acts.append(el("a", { class: "btn", href: `https://github.com/${M.repo}/issues/new?labels=build-request&title=${encodeURIComponent(title)}&body=${encodeURIComponent(body)}`, text: "Request this build" }));
  }
  const ag = window.PortalAgent;
  if (r.valid && ag && ag.connected) {
    const done = ag.resultFor(r.id);
    if (done && done.ok && done.file) acts.append(el("button", { type: "button", class: "btn", text: "Download from my PC", onclick: () => ag.download(r.id) }));
    acts.append(el("button", { type: "button", class: done && done.ok ? "btn alt" : "btn", text: done ? "Rebuild on my PC" : "Build on my PC",
      onclick: () => ag.queueBuild({ id: r.id, preset: r.preset, game: r.ci_game, args: r.cmake_args }) }));
  } else if (r.valid) {
    acts.append(el("a", { class: "btn alt", href: "#desktop", text: "Build on my PC…" }));
  }
  $("res-cmd").textContent = r.valid ? r.command : "Fix the problems above to see the build command.";
  $("copy-cmd").disabled = !r.valid;
  const kv = $("res-kv");
  kv.replaceChildren();
  const add = (k, v) => kv.append(el("dt", { text: k }), el("dd", { text: v }));
  if (r.valid) {
    add("Retail CRC", r.retail_crc ? "compatible" : "not compatible");
    add("Preset", r.preset);
    add("Changed switches", Object.keys(r.switches).length ? Object.entries(r.switches).map(([k, v]) => `${k}=${v}`).join(", ") : "none");
    if (b && b.commit) add("Commit", b.commit.slice(0, 10));
    if (b && b.built_at) add("Built", b.built_at);
  }
}

function renderSwitches(r) {
  const root = $("switch-groups");
  root.replaceChildren();
  for (const g of M.groups) {
    const group = el("div", { class: "group" }, el("h5", { text: g.name }));
    for (const s of M.switches.filter((x) => x.group === g.id)) {
      const off = s.id in r.disabled;
      const val = off ? s.default : state.switches[s.id] ?? s.default;
      const row = el("div", { class: "sw" + (off ? " off" : "") + (val !== s.default ? " changed" : "") });
      const name = el("div", { class: "name", text: s.id.replace(/_/g, " ") + " " }, el("code", { text: s.cmake }));
      const opts = (s.type === "tristate" ? ["DEFAULT", "ON", "OFF"] : ["ON", "OFF"]).map((v) => [v, v === "DEFAULT" ? "Default" : v]);
      const ctl = el("div", { class: "seg", role: "radiogroup", "aria-label": s.id });
      seg(ctl, opts, val, (v) => { if (v === s.default) delete state.switches[s.id]; else state.switches[s.id] = v; render(); }, () => off);
      row.append(name, ctl, el("div", { class: "help", text: s.help + (s.type === "bool" ? ` (default ${s.default})` : "") }));
      if (off) row.append(el("div", { class: "why", text: s.why || "Not available here." }));
      else if (s.warn && val !== s.default) row.append(el("div", { class: "why", text: s.warn }));
      group.append(row);
    }
    root.append(group);
  }
  const n = Object.keys(r.switches || {}).length;
  $("adv-count").textContent = n ? `(${n} changed)` : "";
}

function renderGrid() {
  const grid = $("grid");
  const head = el("tr", {}, el("th", { text: "Compiler / configuration" }), ...M.games.map((g) => el("th", { text: g.name })));
  const rows = [];
  for (const c of M.compilers) for (const cfg of M.configs) {
    const row = el("tr", {}, el("th", { text: `${c.id === "vc6" ? "VC6" : "MSVC"} · ${cfg.name}` }));
    for (const g of M.games) {
      if (!cfg.presets[c.id]) { row.append(el("td", { class: "na", text: "n/a" })); continue; }
      const b = byId[buildId(g.id, c.id, cfg.id, {})];
      const status = b ? b.status : "none";
      const label = status === "built" ? "Built" : status === "failed" ? "Failed" : status === "planned" ? "Planned" : "On request";
      const btn = el("button", { type: "button" }, label, el("span", { class: "sub", text: b ? (b.retail_crc ? "retail CRC" : "not retail CRC") : "" }));
      btn.addEventListener("click", () => { Object.assign(state, { game: g.id, compiler: c.id, config: cfg.id, switches: {} }); render(); $("configure").scrollIntoView(); });
      row.append(el("td", { class: status }, btn));
    }
    rows.push(row);
  }
  grid.replaceChildren(head, ...rows);
}

function renderCards() {
  const quick = [
    ["universal", "vc6", "Universal", "Both games in one package, built with VC6. Retail-compatible: use this for online play.", true],
    ["universal", "msvc", "Universal", "Both games, modern compiler. Easier to debug; not retail-compatible."],
    ["zh", "vc6", "Zero Hour", "Zero Hour only. Retail-compatible."],
    ["generals", "vc6", "Generals", "Generals only. Retail-compatible."],
    ["zh", "msvc", "Zero Hour", "Zero Hour only, modern compiler. Not retail-compatible."],
    ["generals", "msvc", "Generals", "Generals only, modern compiler. Not retail-compatible."],
  ];
  $("quick-cards").replaceChildren(...quick.map(([g, c, n, blurb, isDefault]) => {
    const b = byId[buildId(g, c, "release", {})];
    const head = el("h4", { text: `${n} · ${c === "vc6" ? "VC6" : "Modern MSVC"}` });
    if (isDefault) head.append(" ", el("span", { class: "tag ok", text: "default" }));
    const card = el("div", { class: "card" + (isDefault ? " primary" : "") }, head, el("p", { text: blurb }));
    const row = el("div", { class: "row" });
    if (b && b.status === "built" && b.files && b.files[0]) row.append(el("a", { class: "btn", href: b.files[0].url, text: "Download" }));
    else row.append(el("span", { class: "tag", text: b ? "not built yet" : "on request" }));
    card.append(row);
    return card;
  }));

  const groups = new Map();
  for (const b of B.builds.filter((x) => x.tier === 2)) {
    if (!groups.has(b.name)) groups.set(b.name, { blurb: b.blurb, items: [] });
    groups.get(b.name).items.push(b);
  }
  $("variant-cards").replaceChildren(...[...groups].map(([name, v]) => {
    const card = el("div", { class: "card" }, el("h4", { text: name }), el("p", { text: v.blurb }));
    const row = el("div", { class: "row" });
    for (const b of v.items) {
      const t = el("button", { type: "button", class: "ghost", text: shortName(b), onclick: () => loadBuild(b) });
      row.append(t);
    }
    card.append(row);
    return card;
  }));
}

function render() {
  const sel = { game: state.game, compiler: state.compiler, config: state.config, switches: state.switches };
  // If the config does not exist for the compiler, fall back to Release.
  if (!M.configs.find((c) => c.id === sel.config).presets[sel.compiler]) { state.config = sel.config = "release"; }
  const r = resolve(M, sel);

  seg($("seg-game"), M.games.map((g) => [g.id, g.name]), state.game, (v) => { state.game = v; render(); });
  seg($("seg-compiler"), M.compilers.map((c) => [c.id, c.name]), state.compiler, (v) => { state.compiler = v; render(); });
  $("compiler-note").textContent = M.compilers.find((c) => c.id === state.compiler).note;
  seg($("seg-config"), M.configs.map((c) => [c.id, c.name]), state.config, (v) => { state.config = v; render(); }, (id) => !M.configs.find((c) => c.id === id).presets[state.compiler]);

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
  byId = Object.fromEntries(B.builds.map((b) => [b.id, b]));
  const real = B.commit && B.commit !== "demo-sample";
  $("commit-line").textContent = real ? `Built from commit ${B.commit.slice(0, 10)}.` : "";
  window.addEventListener("portal:agent", render);
  readHash();
  renderGrid();
  renderCards();
  render();
  $("reset").addEventListener("click", () => { state.switches = {}; render(); });
  $("copy-cmd").addEventListener("click", () => { try { navigator.clipboard.writeText($("res-cmd").textContent); } catch (e) { /* clipboard unavailable */ } });
  window.addEventListener("hashchange", () => { readHash(); render(); });
}
if (typeof document !== "undefined" && document.getElementById("grid")) init();
if (typeof module !== "undefined") module.exports = { resolve, buildId, sha1hex };
