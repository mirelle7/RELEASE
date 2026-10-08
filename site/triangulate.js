"use strict";
/* PR triangulator, browser edition. Mirror of tools/triangulate.py: keep the two in step;
 * tools/test_tools.py runs both over the same fixture and compares the results.
 * Text similarity is TF-IDF (lexical), not neural embeddings. */
(function (root) {
  const STOP = new Set(("a an and are as at be by for from has have in is it its of on or that the this to was were will with " +
    "we you i not no but if then so do does did can could should would pr pull request fix fixes fixed add adds added " +
    "update updates change changes use uses using when which what why how also into than").split(" "));
  const TEXT_T = 0.30, FILES_T = 0.20;

  function splitWords(s) {
    s = s.replace(/([a-z0-9])([A-Z])/g, "$1 $2").replace(/([A-Z]+)([A-Z][a-z])/g, "$1 $2");
    return (s.toLowerCase().match(/[a-z0-9]+/g) || []).filter((w) => w.length > 1 && !STOP.has(w));
  }
  const addedLines = (patch) => (patch || "").split("\n").filter((l) => l.startsWith("+") && !l.startsWith("+++")).map((l) => l.slice(1)).join("\n");

  function changedRanges(patch) {
    const out = [], re = /^@@ -\d+(?:,\d+)? \+(\d+)(?:,(\d+))? @@/gm;
    let m;
    while ((m = re.exec(patch || ""))) {
      const start = +m[1], n = m[2] !== undefined ? +m[2] : 1;
      out.push([start, start + Math.max(n, 1) - 1]);
    }
    return out;
  }

  function tokens(pr) {
    let words = [];
    const title = splitWords(pr.title || "");
    for (let i = 0; i < 3; i++) words = words.concat(title);
    words = words.concat(splitWords(pr.body || ""));
    for (const f of pr.files || []) words = words.concat(splitWords(f.path), splitWords(addedLines(f.patch)));
    return words;
  }

  function tfidfVectors(prs) {
    const docs = prs.map((p) => { const c = new Map(); for (const w of tokens(p)) c.set(w, (c.get(w) || 0) + 1); return c; });
    const df = new Map();
    for (const d of docs) for (const w of d.keys()) df.set(w, (df.get(w) || 0) + 1);
    const n = docs.length;
    return docs.map((d) => {
      const v = new Map();
      for (const [w, c] of d) v.set(w, (1 + Math.log(c)) * (Math.log((1 + n) / (1 + df.get(w))) + 1));
      let norm = 0;
      for (const x of v.values()) norm += x * x;
      norm = Math.sqrt(norm) || 1;
      for (const [w, x] of v) v.set(w, x / norm);
      return v;
    });
  }

  function cosine(a, b) {
    if (a.size > b.size) [a, b] = [b, a];
    let s = 0;
    for (const [w, x] of a) s += x * (b.get(w) || 0);
    return s;
  }

  function fileScore(a, b) {
    const pa = new Set(a.files.map((f) => f.path)), pb = new Set(b.files.map((f) => f.path));
    if (!pa.size || !pb.size) return [0, []];
    const shared = [...pa].filter((p) => pb.has(p)).sort();
    const union = new Set([...pa, ...pb]);
    const jac = shared.length / union.size;
    const dir = (p) => p.slice(0, Math.max(p.lastIndexOf("/"), 0)) || p;
    const da = new Set([...pa].map(dir)), db = new Set([...pb].map(dir));
    const dshared = [...da].filter((d) => db.has(d)).length, dunion = new Set([...da, ...db]).size;
    return [Math.max(jac, 0.5 * (dshared / dunion)), shared];
  }

  function lineOverlap(a, b, shared) {
    const fa = new Map(a.files.map((f) => [f.path, changedRanges(f.patch)]));
    const fb = new Map(b.files.map((f) => [f.path, changedRanges(f.patch)]));
    const hits = [];
    for (const path of shared) {
      const ra = fa.get(path) || [], rb = fb.get(path) || [];
      if (!ra.length || !rb.length) continue; // no patch data: cannot tell
      if (ra.some(([s1, e1]) => rb.some(([s2, e2]) => s1 <= e2 && s2 <= e1))) hits.push(path);
    }
    return hits;
  }

  function classify(text, files, lineHits) {
    const sig = { text: text >= TEXT_T, files: files >= FILES_T, lines: lineHits.length > 0 };
    const n = +sig.text + +sig.files + +sig.lines;
    let label;
    if (n === 3) label = text >= 0.5 ? "duplicate" : "conflict";
    else if (n === 2) label = sig.files && sig.lines ? "conflict" : "related";
    else if (n === 1) label = "weak";
    else label = "none";
    return [label, sig];
  }

  const round3 = (x) => Math.round(x * 1000) / 1000;

  function groups(prs, edges) {
    const parent = new Map(prs.map((p) => [p.number, p.number]));
    const find = (x) => { while (parent.get(x) !== x) { parent.set(x, parent.get(parent.get(x))); x = parent.get(x); } return x; };
    for (const e of edges) if (e.label !== "weak") parent.set(find(e.a), find(e.b));
    const out = new Map();
    for (const n of parent.keys()) { const r = find(n); if (!out.has(r)) out.set(r, []); out.get(r).push(n); }
    return [...out.values()].filter((g) => g.length > 1).map((g) => g.sort((x, y) => x - y)).sort((x, y) => x[0] - y[0]);
  }

  function triangulate(input) {
    const prs = input.map((p) => ({ ...p, files: p.files || [] })).sort((a, b) => a.number - b.number);
    const vecs = tfidfVectors(prs);
    const edges = [];
    for (let i = 0; i < prs.length; i++) for (let j = i + 1; j < prs.length; j++) {
      const a = prs[i], b = prs[j];
      const text = cosine(vecs[i], vecs[j]);
      const [files, shared] = fileScore(a, b);
      const hits = lineOverlap(a, b, shared);
      const [label, signals] = classify(text, files, hits);
      if (label === "none") continue;
      edges.push({ a: a.number, b: b.number, label, signals, text: round3(text), files: round3(files), shared_files: shared, overlapping_files: hits, _t: text, _f: files });
    }
    const order = { duplicate: 0, conflict: 1, related: 2, weak: 3 };
    edges.sort((x, y) => order[x.label] - order[y.label] || (y._t + y._f) - (x._t + x._f) || x.a - y.a || x.b - y.b);
    edges.forEach((e) => { delete e._t; delete e._f; });
    return { prs: Object.fromEntries(prs.map((p) => [p.number, p.title || ""])), edges, groups: groups(prs, edges) };
  }

  /* GitHub REST, straight from the browser (the API allows CORS). Public repos work without a token, at 60 requests/hour. */
  async function fetchGitHub(repo, { token, limit = 30, apiUrl = "https://api.github.com", fetchImpl = root.fetch } = {}) {
    if (!/^[\w.-]+\/[\w.-]+$/.test(repo)) throw new Error("Enter the repo as owner/name");
    const get = async (path) => {
      const headers = { Accept: "application/vnd.github+json" };
      if (token) headers.Authorization = "Bearer " + token;
      const r = await fetchImpl(apiUrl + path, { headers });
      if (r.status === 403 || r.status === 429) throw new Error("GitHub rate limit reached. Add a token, or try again later.");
      if (r.status === 404) throw new Error("Repository not found (or private: add a token).");
      if (!r.ok) throw new Error("GitHub returned " + r.status);
      return r.json();
    };
    const list = await get(`/repos/${repo}/pulls?state=open&per_page=${Math.min(limit, 100)}`);
    const out = [];
    for (const pr of list) {
      const files = await get(`/repos/${repo}/pulls/${pr.number}/files?per_page=100`);
      out.push({ number: pr.number, title: pr.title, body: pr.body || "", files: files.map((f) => ({ path: f.filename, patch: f.patch || "" })) });
    }
    return out;
  }

  const api = { triangulate, fetchGitHub, splitWords, changedRanges, lineOverlap };
  root.Triangulator = api;
  if (typeof module !== "undefined") module.exports = api;
})(typeof window !== "undefined" ? window : globalThis);
