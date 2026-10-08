#!/usr/bin/env python3
"""Local build agent: builds portal configurations on this PC and publishes the results to the site.

Run it from a shell where the compiler is already set up (a VS Developer Command Prompt for modern
MSVC, or the shell where your VC6 vcvars32.bat has been run), with CMake and Ninja on PATH.

  agent.py plan                              list builds that have no artifact yet
  agent.py enqueue --id zh-vc6-release       queue a planned build
  agent.py enqueue --select game=zh compiler=vc6 config=release switch=debug_cheats:ON
  agent.py build  --src C:\\src\\GeneralsGameCode --id zh-vc6-release   build now
  agent.py watch  --src C:\\src\\GeneralsGameCode                      keep building whatever lands in queue/

Everything the agent runs is derived from matrix.yaml through the same resolver the website uses, so a
queue file can only ever select known switches with allowed values. It never executes text from a
queue file. Use --dry-run to see the commands without running them.

Results go to site/builds/<id>/ (zip + manifest) and site/data/builds.json. Nothing is pushed anywhere;
commit and push the site yourself (or let your host redeploy) once you are happy with the output.
"""
import argparse
import datetime
import hashlib
import json
import shutil
import subprocess
import sys
import time
import zipfile
from pathlib import Path

HERE = Path(__file__).resolve().parent
sys.path.insert(0, str(HERE.parent / "scripts"))
import portal  # noqa: E402

KEEP_EXT = {".exe", ".dll", ".pdb"}


# --------------------------------------------------------------------------- data

def builds_path(site):
    return site / "data" / "builds.json"


def load_builds(site):
    return json.loads(builds_path(site).read_text(encoding="utf-8"))


def save_builds(site, doc):
    builds_path(site).write_text(json.dumps(doc, indent=1), encoding="utf-8")
    portal.write_data_js(site / "data")


def parse_selection(items):
    """['game=zh', 'switch=debug_cheats:ON', ...] -> selection dict."""
    sel = {"switches": {}}
    for it in items:
        k, _, v = it.partition("=")
        if k == "switch":
            sk, _, sv = v.partition(":")
            sel["switches"][sk] = sv
        elif k in ("game", "compiler", "config") and v:
            sel[k] = v
        else:
            raise SystemExit(f"bad --select item {it!r} (use game=, compiler=, config=, switch=id:VALUE)")
    return sel


def resolve_job(m, doc, job):
    """A queue job is {"id": planned id} or {"selection": {...}}. Returns the resolved build dict."""
    if "id" in job:
        b = next((x for x in doc["builds"] if x["id"] == job["id"]), None)
        if b is None:
            raise ValueError(f"unknown build id {job['id']!r}; use --select for a custom combination")
        sel = {"game": b["game"], "compiler": b["compiler"], "config": b["config"], "switches": b["switches"]}
    else:
        sel = job["selection"]
    r = portal.resolve(m, sel)
    if not r["valid"]:
        raise ValueError("; ".join(r["errors"]))
    return r


# --------------------------------------------------------------------------- build

def run(cmd, cwd, log):
    """Run a command (argument list, never a shell string); stream output to the log. Returns exit code."""
    log.write(f"\n$ {' '.join(cmd)}\n")
    log.flush()
    return subprocess.run(cmd, cwd=cwd, stdout=log, stderr=subprocess.STDOUT).returncode


def collect(src, preset, ci_game):
    """Binaries from the build tree, laid out as the CI workflow collects them."""
    base = Path(src) / "build" / preset
    if preset.startswith("win32"):
        cfg = "Debug" if "debug" in preset else "Release"
        dirs = [base / "Core" / cfg, base / ci_game / cfg]
    else:
        dirs = [base / "Core", base / ci_game]
    files = {}
    for d in dirs:
        if d.is_dir():
            for f in sorted(d.iterdir()):
                if f.is_file() and f.suffix.lower() in KEEP_EXT:
                    files[f.name] = f
    return list(files.values())


def git_head(src):
    try:
        out = subprocess.run(["git", "rev-parse", "HEAD"], cwd=src, capture_output=True, text=True, timeout=20)
        return out.stdout.strip() or "unknown"
    except (OSError, subprocess.SubprocessError):
        return "unknown"


def now():
    return datetime.datetime.now(datetime.timezone.utc).strftime("%Y-%m-%d %H:%M UTC")


def record(site, doc, r, update):
    """Create-or-update the build's entry in builds.json."""
    b = next((x for x in doc["builds"] if x["id"] == r["id"]), None)
    if b is None:
        b = {"id": r["id"], "game": r["game"], "compiler": r["compiler"], "config": r["config"],
             "switches": r["switches"], "retail_crc": r["retail_crc"], "preset": r["preset"],
             "ci_game": r["ci_game"], "cmake_args": r["cmake_args"], "command": r["command"],
             "tier": 3, "name": "On request", "blurb": "Built on request by the local build agent.",
             "warnings": r["warnings"], "status": "planned"}
        doc["builds"].append(b)
    b.update(update)
    save_builds(site, doc)


def build_one(m, site, src, r, dry_run=False, logs_dir=None, runner=run):
    """Configure, build, package and record one resolved build. Returns True on success."""
    doc = load_builds(site)
    preset = r["preset"]
    steps = [["cmake", "--preset", preset, *r["cmake_args"]], ["cmake", "--build", "--preset", preset]]
    if dry_run:
        for s in steps:
            print("would run:", " ".join(s), f"(in {src})")
        print(f"would package binaries from {Path(src) / 'build' / preset} into site/builds/{r['id']}/")
        return True

    if shutil.which("cmake") is None and runner is run:
        raise SystemExit("cmake was not found on PATH. Open a developer shell with the compiler and CMake set up.")
    logs_dir = Path(logs_dir or HERE.parent / "agent-logs")
    logs_dir.mkdir(parents=True, exist_ok=True)
    log_path = logs_dir / f"{r['id']}.log"
    ok = False
    with open(log_path, "w", encoding="utf-8") as log:
        log.write(f"build {r['id']}\n")
        ok = runner(steps[0], src, log) == 0 and runner(steps[1], src, log) == 0
    if not ok:
        record(site, doc, r, {"status": "failed", "built_at": now()})
        print(f"FAILED {r['id']} (log: {log_path})")
        return False

    files = collect(src, preset, r["ci_game"])
    if not files:
        record(site, doc, r, {"status": "failed", "built_at": now()})
        print(f"FAILED {r['id']}: the build succeeded but produced no .exe/.dll/.pdb under build/{preset}")
        return False

    dest = site / "builds" / r["id"]
    dest.mkdir(parents=True, exist_ok=True)
    zpath = dest / f"{r['id']}.zip"
    commit = git_head(src)
    with zipfile.ZipFile(zpath, "w", zipfile.ZIP_DEFLATED) as z:
        for f in files:
            z.write(f, f.name)
        z.writestr("BUILD-INFO.txt", f"id: {r['id']}\ncommit: {commit}\nbuilt: {now()}\ncommand: {r['command']}\n")
    digest = hashlib.sha256(zpath.read_bytes()).hexdigest()
    entry = {"name": zpath.name, "url": f"builds/{r['id']}/{zpath.name}", "size": zpath.stat().st_size, "sha256": digest}
    update = {"status": "built", "commit": commit, "built_at": now(), "files": [entry]}
    (dest / "manifest.json").write_text(json.dumps({"id": r["id"], **update}, indent=1), encoding="utf-8")
    record(site, doc, r, update)
    print(f"built {r['id']}: {len(files)} files, {entry['size'] // 1024} KB, sha256 {digest[:12]}")
    return True


# --------------------------------------------------------------------------- commands

def cmd_plan(args):
    doc = load_builds(args.site)
    todo = [b for b in doc["builds"] if b["status"] != "built"]
    for b in todo:
        print(f"{b['status']:8} tier{b['tier']} {b['id']}")
    print(f"{len(todo)} of {len(doc['builds'])} builds have no artifact")


def cmd_enqueue(args):
    m, doc = portal.load_matrix(), load_builds(args.site)
    job = {"id": args.id} if args.id else {"selection": parse_selection(args.select)}
    try:
        r = resolve_job(m, doc, job)  # validates before anything is written
    except ValueError as e:
        sys.exit(f"cannot queue: {e}")
    args.queue.mkdir(parents=True, exist_ok=True)
    f = args.queue / f"{r['id']}.json"
    f.write_text(json.dumps({"id": r["id"]} if args.id else job), encoding="utf-8")
    print(f"queued {r['id']} -> {f}")


def process(args, job_file):
    m, doc = portal.load_matrix(), load_builds(args.site)
    try:
        r = resolve_job(m, doc, json.loads(job_file.read_text(encoding="utf-8")))
    except (ValueError, KeyError, json.JSONDecodeError) as e:
        print(f"rejected {job_file.name}: {e}")
        return False
    return build_one(m, args.site, args.src, r, args.dry_run, args.logs)


def cmd_build(args):
    m, doc = portal.load_matrix(), load_builds(args.site)
    job = {"id": args.id} if args.id else {"selection": parse_selection(args.select)}
    try:
        r = resolve_job(m, doc, job)
    except ValueError as e:
        sys.exit(f"cannot build: {e}")
    sys.exit(0 if build_one(m, args.site, args.src, r, args.dry_run, args.logs) else 1)


def cmd_watch(args):
    done, failed = args.queue / "done", args.queue / "failed"
    for d in (args.queue, done, failed):
        d.mkdir(parents=True, exist_ok=True)
    print(f"watching {args.queue} (Ctrl+C to stop)")
    while True:
        for job in sorted(args.queue.glob("*.json")):
            ok = process(args, job)
            if not args.dry_run:
                shutil.move(str(job), str((done if ok else failed) / job.name))
        if args.once:
            return
        time.sleep(args.interval)


def main(argv=None):
    ap = argparse.ArgumentParser(description=__doc__.split("\n\n")[0])
    ap.add_argument("--site", type=Path, default=portal.ROOT / "site", help="site folder (default: ./site)")
    sub = ap.add_subparsers(dest="cmd", required=True)

    def common(p, need_src=True):
        p.add_argument("--src", type=Path, required=need_src, help="path to your GeneralsGameCode checkout")
        p.add_argument("--dry-run", action="store_true", help="print the commands, run nothing")
        p.add_argument("--logs", type=Path, help="where to keep build logs (default: ./agent-logs)")

    sub.add_parser("plan").set_defaults(fn=cmd_plan)
    e = sub.add_parser("enqueue")
    e.add_argument("--id")
    e.add_argument("--select", nargs="+", default=[])
    e.add_argument("--queue", type=Path, default=portal.ROOT / "queue")
    e.set_defaults(fn=cmd_enqueue)
    b = sub.add_parser("build")
    b.add_argument("--id")
    b.add_argument("--select", nargs="+", default=[])
    common(b)
    b.set_defaults(fn=cmd_build)
    w = sub.add_parser("watch")
    w.add_argument("--queue", type=Path, default=portal.ROOT / "queue")
    w.add_argument("--interval", type=int, default=10)
    w.add_argument("--once", action="store_true", help="process the queue once and exit")
    common(w)
    w.set_defaults(fn=cmd_watch)

    args = ap.parse_args(argv)
    if args.cmd in ("enqueue", "build") and bool(args.id) == bool(args.select):
        ap.error("give exactly one of --id or --select")
    args.fn(args)


if __name__ == "__main__":
    main()
