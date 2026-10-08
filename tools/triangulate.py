#!/usr/bin/env python3
"""PR triangulator: find duplicate, conflicting and related pull requests.

Each pair of PRs is scored on three independent signals and labelled by how many agree:

  text   what the change says: TF-IDF cosine over title, description, touched path words and
         identifiers from the added lines. This is lexical similarity, not neural embeddings.
  files  which files it touches: Jaccard overlap of changed paths (and a weaker same-directory score)
  lines  where in those files: do the changed line ranges of a shared file overlap

  all 3 agree           -> "duplicate" when the text is very close, else "conflict"
  files + lines, no text -> "conflict": same code touched for a different purpose (will likely collide on merge)
  any other 2           -> "related"
  1 signal              -> "weak" (reported only with --all)

Input is a JSON file (see tests/fixtures) or the GitHub REST API:

  triangulate.py --input prs.json [--pr 123] [--json] [--all]
  triangulate.py --repo owner/name [--token-env GITHUB_TOKEN] [--api-url URL] ...

Standard library only. Output is deterministic.
"""
import argparse
import itertools
import json
import math
import os
import re
import sys
import urllib.request
from collections import Counter

STOP = set("""a an and are as at be by for from has have in is it its of on or that the this to was were will with
we you i not no but if then so do does did can could should would pr pull request fix fixes fixed add adds added
update updates change changes use uses using when which what why how also into than""".split())

TEXT_T, FILES_T, LINES_T = 0.30, 0.20, 1  # a signal "fires" when its score reaches the threshold


def split_words(s):
    """Split identifiers and paths into lowercase words: GameLogic/AIGroup.cpp -> game logic ai group cpp."""
    s = re.sub(r"([a-z0-9])([A-Z])", r"\1 \2", s)
    s = re.sub(r"([A-Z]+)([A-Z][a-z])", r"\1 \2", s)
    return [w for w in re.findall(r"[a-z0-9]+", s.lower()) if len(w) > 1 and w not in STOP]


def added_lines(patch):
    return "\n".join(l[1:] for l in (patch or "").splitlines() if l.startswith("+") and not l.startswith("+++"))


def changed_ranges(patch):
    """New-file line ranges touched by a unified-diff patch: [(start, end), ...]."""
    out = []
    for m in re.finditer(r"^@@ -\d+(?:,\d+)? \+(\d+)(?:,(\d+))? @@", patch or "", re.M):
        start, n = int(m.group(1)), int(m.group(2) if m.group(2) is not None else 1)
        out.append((start, start + max(n, 1) - 1))
    return out


def tokens(pr):
    words = []
    words += split_words(pr.get("title", "")) * 3
    words += split_words(pr.get("body", "") or "")
    for f in pr.get("files", []):
        words += split_words(f["path"])
        words += split_words(added_lines(f.get("patch")))
    return words


def tfidf_vectors(prs):
    docs = [Counter(tokens(p)) for p in prs]
    df = Counter(w for d in docs for w in d)
    n = len(docs)
    vecs = []
    for d in docs:
        v = {w: (1 + math.log(c)) * (math.log((1 + n) / (1 + df[w])) + 1) for w, c in d.items()}
        norm = math.sqrt(sum(x * x for x in v.values())) or 1.0
        vecs.append({w: x / norm for w, x in v.items()})
    return vecs


def cosine(a, b):
    if len(a) > len(b):
        a, b = b, a
    return sum(x * b.get(w, 0.0) for w, x in a.items())


def file_score(a, b):
    pa, pb = {f["path"] for f in a["files"]}, {f["path"] for f in b["files"]}
    if not pa or not pb:
        return 0.0, []
    shared = sorted(pa & pb)
    jac = len(shared) / len(pa | pb)
    da = {p.rsplit("/", 1)[0] for p in pa}
    db = {p.rsplit("/", 1)[0] for p in pb}
    dirs = len(da & db) / len(da | db)
    return max(jac, 0.5 * dirs), shared


def line_overlap(a, b, shared):
    """Shared files where the changed line ranges actually intersect."""
    fa = {f["path"]: changed_ranges(f.get("patch")) for f in a["files"]}
    fb = {f["path"]: changed_ranges(f.get("patch")) for f in b["files"]}
    hits = []
    for path in shared:
        ra, rb = fa.get(path, []), fb.get(path, [])
        if not ra or not rb:
            continue  # no patch data (binary or truncated): cannot tell
        if any(s1 <= e2 and s2 <= e1 for (s1, e1) in ra for (s2, e2) in rb):
            hits.append(path)
    return hits


def classify(text, files, line_hits):
    sig = {"text": text >= TEXT_T, "files": files >= FILES_T, "lines": bool(line_hits)}
    n = sum(sig.values())
    if n == 3:
        label = "duplicate" if text >= 0.5 else "conflict"
    elif n == 2:
        label = "conflict" if sig["files"] and sig["lines"] else "related"
    elif n == 1:
        label = "weak"
    else:
        label = "none"
    return label, sig


def triangulate(prs):
    prs = sorted(prs, key=lambda p: p["number"])
    for p in prs:
        p.setdefault("files", [])
    vecs = tfidf_vectors(prs)
    edges = []
    for (i, a), (j, b) in itertools.combinations(enumerate(prs), 2):
        text = cosine(vecs[i], vecs[j])
        files, shared = file_score(a, b)
        hits = line_overlap(a, b, shared)
        label, sig = classify(text, files, hits)
        if label == "none":
            continue
        edges.append({"a": a["number"], "b": b["number"], "label": label, "signals": sig,
                      "text": round(text, 3), "files": round(files, 3),
                      "shared_files": shared, "overlapping_files": hits})
    order = {"duplicate": 0, "conflict": 1, "related": 2, "weak": 3}
    edges.sort(key=lambda e: (order[e["label"]], -(e["text"] + e["files"]), e["a"], e["b"]))
    return {"prs": {p["number"]: p.get("title", "") for p in prs}, "edges": edges, "groups": groups(prs, edges)}


def groups(prs, edges, min_label=("duplicate", "conflict", "related")):
    parent = {p["number"]: p["number"] for p in prs}

    def find(x):
        while parent[x] != x:
            parent[x] = parent[parent[x]]
            x = parent[x]
        return x

    for e in edges:
        if e["label"] in min_label:
            parent[find(e["a"])] = find(e["b"])
    out = {}
    for n in parent:
        out.setdefault(find(n), []).append(n)
    return sorted((sorted(g) for g in out.values() if len(g) > 1), key=lambda g: g[0])


# --------------------------------------------------------------------------- input

def fetch_github(repo, token, api_url="https://api.github.com", limit=100):
    def get(path):
        req = urllib.request.Request(api_url + path, headers={"Accept": "application/vnd.github+json", "User-Agent": "pr-triangulator"})
        if token:
            req.add_header("Authorization", f"Bearer {token}")
        with urllib.request.urlopen(req, timeout=30) as r:
            return json.load(r)

    prs = []
    for pr in get(f"/repos/{repo}/pulls?state=open&per_page={min(limit, 100)}"):
        files = get(f"/repos/{repo}/pulls/{pr['number']}/files?per_page=100")
        prs.append({"number": pr["number"], "title": pr["title"], "body": pr.get("body") or "",
                    "files": [{"path": f["filename"], "patch": f.get("patch", "")} for f in files]})
    return prs


# --------------------------------------------------------------------------- output

def report(result, only=None, show_all=False):
    lines = []
    edges = [e for e in result["edges"] if show_all or e["label"] != "weak"]
    if only is not None:
        edges = [e for e in edges if only in (e["a"], e["b"])]
    if not edges:
        return "No related pull requests found." + ("" if show_all else " (use --all to include weak matches)")
    for e in edges:
        s = ",".join(k for k, v in e["signals"].items() if v)
        lines.append(f"{e['label'].upper():9} #{e['a']} <-> #{e['b']}   text={e['text']:.2f} files={e['files']:.2f}  [{s}]")
        lines.append(f"          {result['prs'][e['a']]!r} / {result['prs'][e['b']]!r}")
        if e["overlapping_files"]:
            lines.append("          same lines touched in: " + ", ".join(e["overlapping_files"]))
        elif e["shared_files"]:
            lines.append("          shared files: " + ", ".join(e["shared_files"]))
    if only is None and result["groups"]:
        lines.append("")
        lines.append("Groups: " + "; ".join("#" + ", #".join(map(str, g)) for g in result["groups"]))
    return "\n".join(lines)


def main(argv=None):
    ap = argparse.ArgumentParser(description=__doc__.split("\n\n")[0])
    src = ap.add_mutually_exclusive_group(required=True)
    src.add_argument("--input", help="JSON file: list of {number,title,body,files:[{path,patch}]}")
    src.add_argument("--repo", help="owner/name; reads open PRs through the GitHub API")
    ap.add_argument("--token-env", default="GITHUB_TOKEN", help="env var holding an API token (optional for public repos)")
    ap.add_argument("--api-url", default="https://api.github.com")
    ap.add_argument("--pr", type=int, help="only show matches involving this PR number")
    ap.add_argument("--all", action="store_true", help="include weak (single-signal) matches")
    ap.add_argument("--json", action="store_true", help="machine-readable output")
    args = ap.parse_args(argv)

    if args.input:
        with open(args.input, encoding="utf-8") as f:
            prs = json.load(f)
    else:
        prs = fetch_github(args.repo, os.environ.get(args.token_env), args.api_url)
    result = triangulate(prs)
    if args.pr is not None and args.pr not in result["prs"]:
        sys.exit(f"PR #{args.pr} is not in the input")
    if args.json:
        print(json.dumps(result, indent=1))
    else:
        print(report(result, args.pr, args.all))


if __name__ == "__main__":
    main()
