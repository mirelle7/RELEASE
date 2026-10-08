#!/usr/bin/env python3
"""Build portal generator.

  portal.py generate [--out DIR]   expand matrix.yaml -> site/data/{schema,builds}.json + ci-matrix.json
  portal.py resolve                read a selection as JSON on stdin, print the resolved build
  portal.py publish MANIFESTS_DIR  merge per-build manifest.json files into site/data/builds.json

The resolve() logic is mirrored in site/app.js. Keep the two in step;
scripts/test_portal.py checks both against shared vectors.
"""
import argparse
import hashlib
import itertools
import json
import sys
from pathlib import Path

import yaml

ROOT = Path(__file__).resolve().parent.parent
MATRIX = ROOT / "matrix.yaml"
DATA_DIR = ROOT / "site" / "data"


def load_matrix(path=MATRIX):
    with open(path, encoding="utf-8") as f:
        return yaml.safe_load(f)


# --------------------------------------------------------------------------- conditions

def cond_ok(cond, ctx):
    if "compiler" in cond and ctx["compiler"] not in cond["compiler"]:
        return False
    if "config" in cond and ctx["config"] not in cond["config"]:
        return False
    if "switch" in cond:
        val = ctx["values"][cond["switch"]]
        if "in" in cond and val not in cond["in"]:
            return False
        if "not_in" in cond and val in cond["not_in"]:
            return False
    return True


# --------------------------------------------------------------------------- resolve

def canonical(game, compiler, config, switches):
    """Canonical text hashed into the build id. Keys sorted, no whitespace."""
    obj = {"compiler": compiler, "config": config, "game": game, "switches": dict(sorted(switches.items()))}
    return json.dumps(obj, sort_keys=True, separators=(",", ":"))


def build_id(game, compiler, config, switches):
    base = f"{game}-{compiler}-{config}"
    if not switches:
        return base
    digest = hashlib.sha1(canonical(game, compiler, config, switches).encode()).hexdigest()[:8]
    return f"{base}-{digest}"


def resolve(m, sel):
    """Validate a selection and return the normalised build description.

    sel = {"game", "compiler", "config", "switches": {id: value}}
    Result always has "valid", "errors", "warnings", "disabled" (id -> reason).
    """
    errors, warnings, disabled = [], [], {}
    game, compiler, config = sel.get("game"), sel.get("compiler"), sel.get("config")
    user = sel.get("switches", {})

    games = {g["id"]: g for g in m["games"]}
    compilers = {c["id"]: c for c in m["compilers"]}
    configs = {c["id"]: c for c in m["configs"]}
    sw_by_id = {s["id"]: s for s in m["switches"]}

    if game not in games:
        errors.append(f"unknown game {game!r}")
    if compiler not in compilers:
        errors.append(f"unknown compiler {compiler!r}")
    if config not in configs:
        errors.append(f"unknown config {config!r}")
    for k in user:
        if k not in sw_by_id:
            errors.append(f"unknown switch {k!r}")
    if errors:
        return {"valid": False, "errors": errors, "warnings": [], "disabled": {}}

    preset = configs[config]["presets"].get(compiler)
    if preset is None:
        errors.append(f"{configs[config]['name']} is not available with {compilers[compiler]['name']}")

    # Effective values, in declaration order (a switch may only depend on earlier ones).
    values = {}
    ctx = {"compiler": compiler, "config": config, "values": values}
    for s in m["switches"]:
        want = user.get(s["id"], s["default"])
        allowed = ["DEFAULT", "ON", "OFF"] if s["type"] == "tristate" else ["ON", "OFF"]
        if want not in allowed:
            errors.append(f"{s['id']}: {want!r} is not one of {allowed}")
            want = s["default"]
        values[s["id"]] = want
        if not all(cond_ok(c, ctx) for c in s.get("when", [])):
            disabled[s["id"]] = s.get("why", "not available in this configuration")
            if want != s["default"]:
                errors.append(f"{s['id']}: {disabled[s['id']]}")
            # A disabled bool counts as OFF for the switches that depend on it.
            values[s["id"]] = "OFF" if s["type"] == "bool" else s["default"]
        elif want != s["default"] and s.get("warn"):
            warnings.append(f"{s['id']}: {s['warn']}")

    for r in m.get("rules", []):
        if cond_ok(r["if"], ctx) and not cond_ok(r["require"], ctx):
            errors.append(r["message"])

    switches = {s["id"]: values[s["id"]] for s in m["switches"]
                if s["id"] not in disabled and values[s["id"]] != s["default"]}

    # Retail CRC compatibility: VC6 and the master switch not forced OFF.
    retail = compilers[compiler]["retail_crc"] and values["retail_compat"] != "OFF"
    if not retail and values["retail_compat"] != "OFF":
        warnings.append("Not CRC-compatible with retail: " + compilers[compiler]["note"])

    out = {
        "valid": not errors,
        "errors": errors,
        "warnings": warnings,
        "disabled": disabled,
        "game": game,
        "compiler": compiler,
        "config": config,
        "switches": switches,
        "retail_crc": retail,
        "id": build_id(game, compiler, config, switches),
    }
    if preset and not errors:
        out.update(command_for(m, game, preset, values, switches))
    return out


def command_for(m, game, preset, values, switches):
    sw_by_id = {s["id"]: s for s in m["switches"]}
    if values.get("ffmpeg") == "ON":
        preset = m["vcpkg_presets"].get(preset, preset)
    g = next(g for g in m["games"] if g["id"] == game)
    args = [f"-D{k}={v}" for k, v in g["cmake"].items()]
    args += [f"-D{sw_by_id[k]['cmake']}={v}" for k, v in switches.items()]
    return {
        "preset": preset,
        "ci_game": g["ci_name"],
        "cmake_args": args,
        "command": " ".join(["cmake", "--preset", preset] + args),
    }


# --------------------------------------------------------------------------- expansion

def expand(m):
    """All tier 1 and tier 2 builds, validated. Raises on a bad matrix."""
    builds = {}

    def add(game, compiler, config, switches, tier, name, blurb):
        r = resolve(m, {"game": game, "compiler": compiler, "config": config, "switches": switches})
        if not r["valid"]:
            raise SystemExit(f"matrix.yaml: {tier} entry {name!r} ({game}/{compiler}/{config}) is invalid: {r['errors']}")
        entry = builds.setdefault(r["id"], {
            "id": r["id"], "game": game, "compiler": compiler, "config": config,
            "switches": r["switches"], "retail_crc": r["retail_crc"],
            "preset": r["preset"], "ci_game": r["ci_game"], "cmake_args": r["cmake_args"],
            "command": r["command"], "tier": tier, "name": name, "blurb": blurb,
            "warnings": r["warnings"], "status": "planned",
        })
        entry["tier"] = min(entry["tier"], tier)

    cfg_name = {c["id"]: c["name"] for c in m["configs"]}
    comp_name = {c["id"]: c["id"].upper() for c in m["compilers"]}
    for t in m["tier1"]:
        for g in m["games"]:
            add(g["id"], t["compiler"], t["config"], {}, 1,
                f"{cfg_name[t['config']]} ({comp_name[t['compiler']]})", "CI preset, default switches.")
    for v in m["tier2"]:
        for on in v["where"]:
            variant_games = (g["id"] for g in m["games"] if g.get("tier2", True))
            for g, comp, cfg in itertools.product(variant_games, on["compiler"], on["config"]):
                add(g, comp, cfg, v["switches"], 2, v["name"], v["blurb"])
    return sorted(builds.values(), key=lambda b: (b["tier"], b["game"], b["compiler"], b["config"], b["id"]))


def ci_matrix(builds):
    return {"include": [
        {"id": b["id"], "game": b["ci_game"], "preset": b["preset"], "tier": b["tier"],
         "cmake_args": " ".join(b["cmake_args"]),
         "extra_switches": bool(b["switches"])}
        for b in builds]}


# --------------------------------------------------------------------------- commands

def write_data_js(out):
    """Script-tag copy of the data, so the site also works when opened straight from disk (file://)."""
    schema = json.loads((out / "schema.json").read_text(encoding="utf-8"))
    builds = json.loads((out / "builds.json").read_text(encoding="utf-8"))
    (out / "portal-data.js").write_text(
        "window.PORTAL_DATA=" + json.dumps({"schema": schema, "builds": builds}) + ";\n", encoding="utf-8")


STATE_KEYS = ("status", "commit", "files", "log_url", "built_at")


def carry_over(builds, previous):
    """Keep recorded build results across `generate`, and keep on-request (tier 3) builds."""
    old = {b["id"]: b for b in previous}
    for b in builds:
        o = old.get(b["id"])
        if o:
            b.update({k: o[k] for k in STATE_KEYS if k in o})
    known = {b["id"] for b in builds}
    return builds + [b for b in previous if b["tier"] >= 3 and b["id"] not in known]


def cmd_generate(args):
    m = load_matrix()
    builds = expand(m)
    out = Path(args.out)
    out.mkdir(parents=True, exist_ok=True)
    prev_path = out / "builds.json"
    commit = None
    if prev_path.exists():
        prev = json.loads(prev_path.read_text(encoding="utf-8"))
        builds = carry_over(builds, prev["builds"])
        commit = prev.get("commit")
    (out / "schema.json").write_text(json.dumps(m, indent=1), encoding="utf-8")
    (out / "builds.json").write_text(json.dumps({"commit": commit, "builds": builds}, indent=1), encoding="utf-8")
    write_data_js(out)
    (ROOT / "ci-matrix.json").write_text(json.dumps(ci_matrix(builds)), encoding="utf-8")
    tiers = {t: sum(1 for b in builds if b["tier"] == t) for t in (1, 2)}
    print(f"{len(builds)} builds planned (tier1={tiers[1]}, tier2={tiers[2]}) -> {out}")


def cmd_resolve(_):
    print(json.dumps(resolve(load_matrix(), json.load(sys.stdin)), indent=1))


def cmd_publish(args):
    """Merge manifests written by CI (one manifest.json per build) into builds.json."""
    path = DATA_DIR / "builds.json"
    doc = json.loads(path.read_text(encoding="utf-8"))
    by_id = {b["id"]: b for b in doc["builds"]}
    for mf in sorted(Path(args.manifests).rglob("manifest.json")):
        info = json.loads(mf.read_text(encoding="utf-8"))
        b = by_id.get(info["id"])
        if b is None:
            continue
        b.update({k: info[k] for k in ("status", "commit", "files", "log_url", "built_at") if k in info})
        doc["commit"] = info.get("commit", doc["commit"])
    path.write_text(json.dumps(doc, indent=1), encoding="utf-8")
    write_data_js(DATA_DIR)
    done = sum(1 for b in doc["builds"] if b["status"] == "built")
    print(f"{done}/{len(doc['builds'])} builds have artifacts")


def main():
    p = argparse.ArgumentParser()
    sub = p.add_subparsers(dest="cmd", required=True)
    g = sub.add_parser("generate")
    g.add_argument("--out", default=str(DATA_DIR))
    g.set_defaults(fn=cmd_generate)
    sub.add_parser("resolve").set_defaults(fn=cmd_resolve)
    pb = sub.add_parser("publish")
    pb.add_argument("manifests")
    pb.set_defaults(fn=cmd_publish)
    args = p.parse_args()
    args.fn(args)


if __name__ == "__main__":
    main()
