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
import re
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
    if "game" in cond and ctx["game"] not in cond["game"]:
        return False
    if "switch" in cond:
        val = ctx["values"][cond["switch"]]
        if "in" in cond and val not in cond["in"]:
            return False
        if "not_in" in cond and val in cond["not_in"]:
            return False
    return True


# --------------------------------------------------------------------------- resolve

def canonical(game, compiler, config, switches, project=None):
    """Canonical text hashed into the build id. Keys sorted, no whitespace.
    The project is part of it only for projects other than the first, so the first project's ids never change."""
    obj = {"compiler": compiler, "config": config, "game": game, "switches": dict(sorted(switches.items()))}
    if project:
        obj["project"] = project
    return json.dumps(obj, sort_keys=True, separators=(",", ":"))


def build_id(game, compiler, config, switches, project=None):
    """project is None for the first project (plain ids), else its id (becomes a prefix)."""
    base = f"{game}-{compiler}-{config}"
    if project:
        base = f"{project}-{base}"
    if not switches:
        return base
    digest = hashlib.sha1(canonical(game, compiler, config, switches, project).encode()).hexdigest()[:8]
    return f"{base}-{digest}"


SHA_RE = re.compile(r"^[0-9a-fA-F]{7,40}$")


def ref_suffix(ref):
    """'' for my checkout as it is, '-latest', or '-c' + the first 7 characters of a commit. None when the ref is invalid."""
    if not ref:
        return ""
    if ref == "latest":
        return "-latest"
    if isinstance(ref, str) and SHA_RE.match(ref):
        return "-c" + ref[:7].lower()
    return None


def project_of(m, pid):
    projects = m["projects"]
    if pid is None:
        return projects[0], projects
    return next((p for p in projects if p["id"] == pid), None), projects


def project_presets(m, proj):
    """{config: {compiler: preset}} for one project: the base presets of its compilers, plus its own."""
    out = {}
    for c in m["configs"]:
        d = {k: v for k, v in c["presets"].items() if k in proj["compilers"]}
        d.update((proj.get("presets") or {}).get(c["id"], {}))
        out[c["id"]] = d
    return out


def project_switches(m, proj):
    return [s for s in m["switches"] if "projects" not in s or proj["id"] in s["projects"]]


def switch_allowed(s):
    if s["type"] == "tristate":
        return ["DEFAULT", "ON", "OFF"]
    if s["type"] == "choice":
        return [c["id"] for c in s["choices"]]
    return ["ON", "OFF"]


def resolve(m, sel):
    """Validate a selection and return the normalised build description.

    sel = {"project", "game", "compiler", "config", "switches": {id: value}, "ref"}
    Result always has "valid", "errors", "warnings", "disabled" (id -> reason).
    """
    errors, warnings, disabled = [], [], {}
    game, compiler, config = sel.get("game"), sel.get("compiler"), sel.get("config")
    user = sel.get("switches", {})
    ref = sel.get("ref") or ""

    proj, projects = project_of(m, sel.get("project"))
    games = {g["id"]: g for g in m["games"]}
    compilers = {c["id"]: c for c in m["compilers"]}
    configs = {c["id"]: c for c in m["configs"]}
    if proj is None:
        return {"valid": False, "errors": [f"unknown project {sel.get('project')!r}"], "warnings": [], "disabled": {}}
    psw = project_switches(m, proj)
    in_proj = {s["id"] for s in psw}
    sw_by_id = {s["id"]: s for s in m["switches"]}

    if game not in games:
        errors.append(f"unknown game {game!r}")
    if compiler not in compilers or compiler not in proj["compilers"]:
        errors.append(f"unknown compiler {compiler!r}")
    if config not in configs:
        errors.append(f"unknown config {config!r}")
    for k in user:
        if k not in in_proj:
            errors.append(f"unknown switch {k!r}")
    suffix = ref_suffix(ref)
    if suffix is None:
        errors.append("Choose a commit, or type a valid commit id (7 to 40 hex digits).")
    if errors:
        return {"valid": False, "errors": errors, "warnings": [], "disabled": {}}

    preset = project_presets(m, proj)[config].get(compiler)
    if preset is None:
        errors.append(f"{configs[config]['name']} is not available with {compilers[compiler]['name']}")
    elif game not in (proj.get("preset_games") or {}).get(preset, list(games)):
        errors.append(f"{games[game]['name']} is not built by the {preset} preset")
    pvals = (proj.get("preset_values") or {}).get(preset, {})
    defaults = {s["id"]: pvals.get(s["id"], s["default"]) for s in m["switches"]}

    # Effective values, in declaration order (a switch may only depend on earlier ones).
    values = {}
    ctx = {"compiler": compiler, "config": config, "game": game, "values": values}
    for s in m["switches"]:
        d = defaults[s["id"]]
        if s["id"] not in in_proj:  # not a switch of this project: it is simply as the source has it
            values[s["id"]] = d
            continue
        want = user.get(s["id"], d)
        allowed = switch_allowed(s)
        if want not in allowed:
            errors.append(f"{s['id']}: {want!r} is not one of {allowed}")
            want = d
        values[s["id"]] = want
        if not all(cond_ok(c, ctx) for c in s.get("when", [])):
            disabled[s["id"]] = s.get("why", "not available in this configuration")
            if want != d:
                errors.append(f"{s['id']}: {disabled[s['id']]}")
            # A disabled bool counts as OFF for the switches that depend on it.
            values[s["id"]] = "OFF" if s["type"] == "bool" else d
        elif want != d and s.get("warn"):
            warnings.append(f"{s['id']}: {s['warn']}")

    for r in m.get("rules", []):
        if cond_ok(r["if"], ctx) and not cond_ok(r["require"], ctx):
            errors.append(r["message"])

    switches = {s["id"]: values[s["id"]] for s in psw
                if s["id"] not in disabled and values[s["id"]] != defaults[s["id"]]}

    # Retail CRC compatibility: VC6, the master switch not OFF, and the two CRC-related detail switches not OFF.
    def eff(i):  # a switch that is not available counts as its source value
        return sw_by_id[i]["default"] if (i in disabled or i not in in_proj) else values[i]
    retail = (compilers[compiler]["retail_crc"] and proj.get("retail_crc", True) and eff("retail_compat") != "OFF"
              and eff("compat_crc") != "OFF" and eff("compat_aigroup") != "OFF")
    if not compilers[compiler]["retail_crc"] and "retail_compat" in in_proj and values["retail_compat"] != "OFF":
        warnings.append("Not CRC-compatible with retail, so it cannot play online against retail clients.")

    pid = None if proj["id"] == projects[0]["id"] else proj["id"]
    out = {
        "valid": not errors,
        "errors": errors,
        "warnings": warnings,
        "disabled": disabled,
        "project": proj["id"],
        "game": game,
        "compiler": compiler,
        "config": config,
        "ref": ref,
        "switches": switches,
        "defaults": defaults,
        "retail_crc": retail,
        "id": build_id(game, compiler, config, switches, pid) + suffix,
    }
    if preset and not errors:
        out.update(command_for(m, proj, game, compiler, preset, values, switches))
    return out


def command_for(m, proj, game, compiler, preset, values, switches):
    sw_by_id = {s["id"]: s for s in m["switches"]}
    for sw in proj.get("preset_swaps") or []:
        if preset in sw["map"] and cond_ok(sw["if"], {"values": values}):
            preset = sw["map"][preset]
            break
    if values.get("ffmpeg") == "ON":
        preset = m["vcpkg_presets"].get(preset, preset)
    g = next(g for g in m["games"] if g["id"] == game)
    c = next(c for c in m["compilers"] if c["id"] == compiler)
    args = [f"-D{k}={v}" for k, v in g["cmake"].items()]
    args += [f"-D{sw_by_id[k]['cmake']}={v}" for k, v in switches.items() if "cmake" in sw_by_id[k]]
    # Code flags are compile definitions, carried in RTS_FLAGS (a ;-list, which the presets also use for /W3).
    prefix = c.get("flag_prefix", "/")
    defines = [f"{prefix}D{sw_by_id[k]['define']}={'1' if v == 'ON' else '0'}" for k, v in switches.items() if "define" in sw_by_id[k]]
    if defines:
        base = c.get("flags_base", m["flags_base"])
        args.append("-DRTS_FLAGS=" + ";".join(([base] if base else []) + defines))
    shown = [f'{a[:a.index("=") + 1]}"{a[a.index("=") + 1:]}"' if a.startswith("-DRTS_FLAGS=") else a for a in args]
    return {
        "preset": preset,
        "ci_game": g["ci_name"],
        "cmake_args": args,
        "command": " ".join(["cmake", "--preset", preset] + shown),
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
            "id": r["id"], "project": r["project"], "game": game, "compiler": compiler, "config": config,
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
                f"{cfg_name[t['config']]} ({comp_name[t['compiler']]})", "The CI preset, built as is.")
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
