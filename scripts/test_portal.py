#!/usr/bin/env python3
"""Checks matrix.yaml, portal.py and site/app.js agree. Run: python3 scripts/test_portal.py"""
import json
import os
import re
import shutil
import subprocess
import sys
import unittest
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))
import portal  # noqa: E402

ROOT = portal.ROOT
M = portal.load_matrix()


def sel(game="zh", compiler="vc6", config="release", **sw):
    return {"game": game, "compiler": compiler, "config": config, "switches": sw}


class Rules(unittest.TestCase):
    def test_defaults_valid_and_hash_free(self):
        r = portal.resolve(M, sel())
        self.assertTrue(r["valid"])
        self.assertEqual(r["id"], "zh-vc6-release")
        self.assertTrue(r["retail_crc"])

    def test_msvc_is_never_retail_crc(self):
        r = portal.resolve(M, sel(compiler="msvc"))
        self.assertFalse(r["retail_crc"])
        self.assertTrue(any("CRC" in w for w in r["warnings"]))

    def test_retail_off_on_vc6_not_crc(self):
        self.assertFalse(portal.resolve(M, sel(retail_compat="OFF"))["retail_crc"])

    def test_vc6_rejects_modern_only_switches(self):
        for s in ("asan", "ffmpeg"):
            self.assertFalse(portal.resolve(M, sel(**{s: "ON"}))["valid"], s)

    def test_config_not_available(self):
        self.assertFalse(portal.resolve(M, sel(compiler="msvc", config="releaselog"))["valid"])

    def test_stacktrace_needs_logging(self):
        self.assertFalse(portal.resolve(M, sel(debug_stacktrace="ON", debug_logging="OFF"))["valid"])
        self.assertTrue(portal.resolve(M, sel(debug_stacktrace="ON"))["valid"])

    def test_memory_children_disabled_without_gamememory(self):
        r = portal.resolve(M, sel(gamememory="OFF"))
        self.assertTrue(r["valid"])
        self.assertIn("mp_debug", r["disabled"])
        self.assertEqual(r["switches"], {"gamememory": "OFF"})
        self.assertFalse(portal.resolve(M, sel(gamememory="OFF", mp_debug_stacktrace="ON"))["valid"])

    def test_id_independent_of_key_order_and_default_noise(self):
        a = portal.resolve(M, sel(debug_cheats="ON", debug_multi_instance="ON"))
        b = portal.resolve(M, sel(debug_multi_instance="ON", debug_cheats="ON", crashdump="ON"))
        self.assertEqual(a["id"], b["id"])

    def test_ffmpeg_uses_vcpkg_preset(self):
        r = portal.resolve(M, sel(compiler="msvc", ffmpeg="ON"))
        self.assertEqual(r["preset"], "win32-vcpkg")


class CarryOver(unittest.TestCase):
    def test_generate_keeps_results_and_on_request_builds(self):
        planned = portal.expand(M)
        prev = [dict(b) for b in planned]
        prev[0].update(status="built", commit="abc", files=[{"name": "x.zip"}])
        adhoc = dict(prev[0], id="zh-vc6-release-deadbeef", tier=3, status="built")
        merged = portal.carry_over(portal.expand(M), prev + [adhoc])
        by_id = {b["id"]: b for b in merged}
        self.assertEqual(by_id[prev[0]["id"]]["status"], "built")
        self.assertEqual(by_id[prev[0]["id"]]["files"], [{"name": "x.zip"}])
        self.assertIn("zh-vc6-release-deadbeef", by_id)
        self.assertEqual(sum(b["status"] == "planned" for b in merged), len(planned) - 1)


class Expansion(unittest.TestCase):
    def test_every_planned_build_valid_and_unique(self):
        builds = portal.expand(M)
        ids = [b["id"] for b in builds]
        self.assertEqual(len(ids), len(set(ids)))
        self.assertGreater(len(ids), 20)

    def test_tier1_covers_every_game_including_universal(self):
        t1 = [b for b in portal.expand(M) if b["tier"] == 1]
        self.assertEqual({b["game"] for b in t1}, {"universal", "generals", "zh"})
        self.assertEqual(len(t1), len(M["games"]) * len(M["tier1"]))

    def test_universal_builds_both_games_and_has_no_variants(self):
        r = portal.resolve(M, sel(game="universal"))
        self.assertEqual(r["id"], "universal-vc6-release")
        self.assertIn("-DRTS_BUILD_GENERALS=ON", r["cmake_args"])
        self.assertIn("-DRTS_BUILD_ZEROHOUR=ON", r["cmake_args"])
        self.assertTrue(r["retail_crc"])
        self.assertFalse([b for b in portal.expand(M) if b["game"] == "universal" and b["tier"] == 2])
        self.assertEqual(M["games"][0]["id"], "universal")  # first = the default selection

    def test_ids_unique_per_project_and_stable_for_the_first(self):
        ids = {}
        for p in M["projects"]:
            for r in all_cells(p["id"]):
                self.assertNotIn(r["id"], ids, f"{r['id']} is in {ids.get(r['id'])} and {p['id']}")
                ids[r["id"]] = p["id"]
                if p["id"] == "ggc":
                    self.assertFalse(r["id"].startswith("ggc-"), r["id"])
                else:
                    self.assertTrue(r["id"].startswith(p["id"] + "-"), r["id"])
        self.assertIn("zh-vc6-release", ids)
        self.assertIn("generalsx-zh-linux-release", ids)


def all_cells(pid, **extra):
    """Every valid (game, compiler, config) of a project with all switches as the source has them."""
    proj = next(p for p in M["projects"] if p["id"] == pid)
    out = []
    for c in M["configs"]:
        for comp in proj["compilers"]:
            for g in M["games"]:
                r = portal.resolve(M, {"project": pid, "game": g["id"], "compiler": comp, "config": c["id"], "switches": {}, **extra})
                if r["valid"]:
                    out.append(r)
    return out


class Projects(unittest.TestCase):
    def test_four_projects_ggc_first(self):
        self.assertEqual([p["id"] for p in M["projects"]], ["ggc", "generalsx", "bobtista", "generalsonline"])

    def test_every_project_compiler_and_preset_is_known(self):
        comp_ids = {c["id"] for c in M["compilers"]}
        for p in M["projects"]:
            self.assertTrue(set(p["compilers"]) <= comp_ids, p["id"])
            self.assertTrue(all_cells(p["id"]), p["id"])
            for cfg, d in (p.get("presets") or {}).items():
                self.assertIn(cfg, {c["id"] for c in M["configs"]})
                self.assertTrue(set(d) <= set(p["compilers"]), f"{p['id']}: {cfg} maps a compiler the project does not list")
            known = {pr for cell in all_cells(p["id"]) for pr in [cell["preset"]]}
            for key in list(p.get("preset_values") or {}) + list(p.get("preset_games") or {}):
                self.assertIn(key, known | {x for d in portal.project_presets(M, p).values() for x in d.values()} | {v for sw in p.get("preset_swaps") or [] for v in sw["map"].values()}, f"{p['id']}: {key}")

    def test_switch_visibility_per_project(self):
        def visible(pid):
            return {s["id"] for s in portal.project_switches(M, next(p for p in M["projects"] if p["id"] == pid))}
        ggc, gx, bob, go = visible("ggc"), visible("generalsx"), visible("bobtista"), visible("generalsonline")
        self.assertNotIn("tracy", go)
        self.assertIn("tracy", gx | bob | ggc)
        self.assertEqual({"render_backend", "bgfx_renderer", "ggc_diagnostic_tools"} & (ggc | gx | go), set())
        self.assertTrue({"render_backend", "bgfx_renderer", "sage_use_sdl3", "sage_use_openal", "tracy"} <= bob)
        self.assertTrue({"sage_use_sdl3", "sage_use_openal", "sage_use_moltenvk", "sage_dxvk_local_fork", "sage_use_deterministic_math", "sage_use_ccache", "tracy"} <= gx)
        self.assertFalse({"sage_use_sdl3", "sage_use_moltenvk"} & (ggc | go))
        self.assertNotIn("retail_compat", gx | go)
        self.assertIn("retail_compat", ggc | bob)
        # a switch of another project is refused
        r = portal.resolve(M, {"project": "ggc", "game": "zh", "compiler": "vc6", "config": "release", "switches": {"sage_use_sdl3": "ON"}})
        self.assertFalse(r["valid"])
        self.assertTrue(portal.resolve(M, {"project": "generalsx", "game": "zh", "compiler": "msvc", "config": "release", "switches": {"sage_use_sdl3": "ON"}})["valid"])

    def test_ggc_results_unchanged_and_carry_the_project(self):
        r = portal.resolve(M, sel())
        self.assertEqual((r["project"], r["id"], r["preset"]), ("ggc", "zh-vc6-release", "vc6"))
        self.assertEqual(portal.resolve(M, dict(sel(), project="ggc"))["id"], "zh-vc6-release")
        self.assertEqual(portal.resolve(M, sel(debug_cheats="ON"))["id"], portal.resolve(M, dict(sel(debug_cheats="ON"), project="ggc"))["id"])

    def test_generalsx_native_targets(self):
        def go(compiler, config="release", **sw):
            return portal.resolve(M, {"project": "generalsx", "game": "zh", "compiler": compiler, "config": config, "switches": sw})
        self.assertEqual(go("linux")["preset"], "linux64-deploy")
        self.assertEqual(go("macos")["preset"], "macos-vulkan")
        self.assertEqual(go("linux", "native")["preset"], "unix")
        self.assertFalse(go("linux", "debug")["valid"])
        self.assertEqual(go("linux")["id"], "generalsx-zh-linux-release")
        # the preset already turns SDL3 on: that is the switch's value, and turning it off is the change
        self.assertTrue(go("linux")["defaults"]["sage_use_sdl3"] == "ON")
        self.assertNotIn("-DSAGE_USE_SDL3=ON", go("linux")["cmake_args"])
        self.assertIn("-DSAGE_USE_SDL3=OFF", go("linux", sage_use_sdl3="OFF")["cmake_args"])
        self.assertFalse(go("msvc", sage_use_moltenvk="ON")["valid"])  # macOS only
        self.assertTrue(go("macos", sage_use_moltenvk="OFF")["valid"])
        # native code flags use -D, not /D
        r = go("linux", feat_use_buffered_io="OFF")
        self.assertIn("-DRTS_FLAGS=-DUSE_BUFFERED_IO=0", r["cmake_args"])

    def test_bobtista_targets_and_render_backend(self):
        def go(compiler, config="release", game="zh", **sw):
            return portal.resolve(M, {"project": "bobtista", "game": game, "compiler": compiler, "config": config, "switches": sw})
        self.assertEqual(go("linux")["preset"], "linux-generalsmd-sdl3-bgfx")
        self.assertEqual(go("macos")["preset"], "macos-generalsmd-sdl3-bgfx")
        self.assertEqual(go("macos", "profile")["preset"], "macos-generalsmd-sdl3-bgfx-profile")
        self.assertEqual(go("msvc", "win64")["preset"], "win64")
        self.assertEqual(go("msvc", "win64debug")["preset"], "win64-debug")
        self.assertEqual(go("msvc", "sdl3")["preset"], "win32-generalsmd-sdl3-bgfx")
        self.assertFalse(go("linux", game="generals")["valid"])  # that preset builds Zero Hour only
        self.assertEqual(go("msvc")["preset"], "win32")
        self.assertEqual(go("msvc", render_backend="bgfx")["preset"], "win32-bgfx")
        self.assertEqual(go("msvc", "debug", render_backend="bgfx")["preset"], "win32-debug-bgfx")
        self.assertEqual(go("msvc", render_backend="bgfx", bgfx_renderer="vulkan")["valid"], True)
        self.assertFalse(go("msvc", bgfx_renderer="vulkan")["valid"])  # needs the bgfx backend
        self.assertFalse(go("vc6", render_backend="bgfx")["valid"])
        self.assertFalse(go("msvc", render_backend="vulkan")["valid"])  # not a backend
        self.assertEqual(go("linux")["defaults"]["bgfx_renderer"], "vulkan")
        self.assertEqual(go("macos")["defaults"]["bgfx_renderer"], "metal")

    def test_generals_online_is_vc6_and_msvc_only_and_never_retail_crc(self):
        r = portal.resolve(M, {"project": "generalsonline", "game": "zh", "compiler": "vc6", "config": "release", "switches": {}})
        self.assertTrue(r["valid"])
        self.assertEqual(r["id"], "generalsonline-zh-vc6-release")
        self.assertFalse(r["retail_crc"])
        for comp in ("mingw-clang", "linux", "macos"):
            self.assertFalse(portal.resolve(M, {"project": "generalsonline", "game": "zh", "compiler": comp, "config": "release", "switches": {}})["valid"], comp)
        self.assertFalse(portal.resolve(M, {"project": "generalsonline", "game": "zh", "compiler": "msvc", "config": "profile", "switches": {"tracy": "ON"}})["valid"])

    def test_linux_and_macos_compilers(self):
        for c in ("linux", "macos"):
            comp = next(x for x in M["compilers"] if x["id"] == c)
            self.assertFalse(comp["retail_crc"])
            self.assertIn("natively", comp["note"])
        self.assertEqual(next(x for x in M["compilers"] if x["id"] == "linux")["name"], "Linux (native)")
        self.assertEqual(next(x for x in M["compilers"] if x["id"] == "macos")["name"], "macOS (Apple silicon, native)")

    def test_unknown_project(self):
        self.assertFalse(portal.resolve(M, dict(sel(), project="nope"))["valid"])


class SourceVersion(unittest.TestCase):
    def test_suffix(self):
        self.assertEqual(portal.ref_suffix(""), "")
        self.assertEqual(portal.ref_suffix("latest"), "-latest")
        self.assertEqual(portal.ref_suffix("ABCDEF0123456789"), "-cabcdef0")
        for bad in ("abc", "xyz1234", "g" * 7, "a" * 41, "main"):
            self.assertIsNone(portal.ref_suffix(bad), bad)

    def test_ids_never_collide_across_versions(self):
        base = portal.resolve(M, sel())["id"]
        latest = portal.resolve(M, dict(sel(), ref="latest"))["id"]
        commit = portal.resolve(M, dict(sel(), ref="0123456789abcdef"))["id"]
        self.assertEqual((base, latest, commit), ("zh-vc6-release", "zh-vc6-release-latest", "zh-vc6-release-c0123456"))
        # with changed switches the suffix goes after the hash, and the hash does not depend on the version
        h = portal.resolve(M, sel(debug_cheats="ON"))["id"]
        self.assertEqual(portal.resolve(M, dict(sel(debug_cheats="ON"), ref="latest"))["id"], h + "-latest")
        self.assertEqual(portal.resolve(M, dict(sel(), project="bobtista", ref="latest"))["id"], "bobtista-zh-vc6-release-latest")

    def test_bad_ref_is_invalid(self):
        r = portal.resolve(M, dict(sel(), ref="nothex"))
        self.assertFalse(r["valid"])

    def test_ref_is_returned(self):
        self.assertEqual(portal.resolve(M, dict(sel(), ref="latest"))["ref"], "latest")
        self.assertEqual(portal.resolve(M, sel())["ref"], "")


PROJECT_SRC = {
    "ggc": os.environ.get("GAME_SRC"),
    "generalsx": os.environ.get("GAME_SRC_GENERALSX"),
    "bobtista": os.environ.get("GAME_SRC_BOBTISTA"),
    "generalsonline": os.environ.get("GAME_SRC_GENERALSONLINE"),
}
if PROJECT_SRC["ggc"] is None and (ROOT.parent / "cmake").exists():
    PROJECT_SRC["ggc"] = str(ROOT.parent)

SKIP_DIRS = ("build", "Dependencies", ".git", "references", "ThirdParty")


def cmake_text(root):
    parts = []
    for pat in ("CMakeLists.txt", "*.cmake"):
        for p in root.rglob(pat):
            if not any(x in p.parts for x in SKIP_DIRS):
                parts.append(p.read_text(encoding="utf-8", errors="ignore"))
    return "\n".join(parts)


def source_flags(root):
    """Every `#ifndef X / #define X (N)` flag in the project's headers: {name: value}."""
    pat = re.compile(r"^[ \t]*#ifndef[ \t]+(\w+)[ \t]*\n[ \t]*#define[ \t]+\1[ \t]+\(?(\w+)\)?", re.M)
    out = {}
    for p in root.rglob("*"):
        if p.suffix in (".h", ".hpp", ".inl") and not any(x in p.parts for x in SKIP_DIRS):
            for m in pat.finditer(p.read_text(encoding="latin-1")):
                out.setdefault(m.group(1), m.group(2))
    return out


class SourceParity(unittest.TestCase):
    """Every switch of a project is a real option in that project's source, and every option there is a switch.
    Set GAME_SRC (ggc), GAME_SRC_GENERALSX, GAME_SRC_BOBTISTA, GAME_SRC_GENERALSONLINE; a project without its variable is skipped.
    A partial snapshot (only some of the cmake files, as for bobtista) checks only the project's own switches."""

    def each(self):
        found = [(p, Path(PROJECT_SRC[p["id"]])) for p in M["projects"]
                 if PROJECT_SRC.get(p["id"]) and Path(PROJECT_SRC[p["id"]], "CMakePresets.json").exists()]
        if not found:
            self.skipTest("no project source set (GAME_SRC, GAME_SRC_GENERALSX, GAME_SRC_BOBTISTA, GAME_SRC_GENERALSONLINE)")
        return found

    def test_every_preset_exists(self):
        for p, root in self.each():
            names = {x["name"] for x in json.loads((root / "CMakePresets.json").read_text(encoding="utf-8"))["configurePresets"]}
            used = set()
            for cell in all_cells(p["id"]):
                used.add(cell["preset"])
            for cfg in portal.project_presets(M, p).values():
                used |= set(cfg.values())
            used |= set(p.get("preset_values") or {}) | set(p.get("preset_games") or {})
            for sw in p.get("preset_swaps") or []:
                used |= set(sw["map"]) | set(sw["map"].values())
            self.assertEqual(sorted(used - names), [], p["id"])

    def test_every_switch_exists_in_the_source(self):
        for p, root in self.each():
            text = cmake_text(root)
            partial = not (root / "cmake" / "config-debug.cmake").exists()
            flags = {} if partial else source_flags(root)
            for s in portal.project_switches(M, p):
                if "cmake" in s:
                    if partial and s.get("group") != "platform" and s["id"] not in ("tracy", "ggc_diagnostic_tools"):
                        continue  # defined in a cmake file the snapshot does not have
                    self.assertTrue(s["cmake"] in text, f"{p['id']}: {s['id']}: {s['cmake']} is not in the source")
                elif not partial:
                    self.assertTrue(s["define"] in flags, f"{p['id']}: {s['id']}: {s['define']} is not a guarded flag in the source")
                    self.assertEqual(s["default"], "ON" if flags[s["define"]] == "1" else "OFF", f"{p['id']}: {s['id']}: source value differs")

    def test_every_option_and_flag_in_the_source_is_a_switch(self):
        for p, root in self.each():
            text = cmake_text(root)
            options = set(re.findall(r"^\s*(?:option\(|set\()((?:RTS|SAGE|GGC)_\w+)", text, re.M))
            covered = {s["cmake"] for s in portal.project_switches(M, p) if "cmake" in s}
            axes = {"RTS_BUILD_ZEROHOUR", "RTS_BUILD_GENERALS", "RTS_BUILD_OPTION_PROFILE", "RTS_BUILD_OPTION_DEBUG"}
            text_options = {"RTS_BUILD_OUTPUT_SUFFIX", "SAGE_GAMEMATH_GIT_REPO", "SAGE_GAMEMATH_GIT_TAG"}  # free text, not flags
            derived = {n for n in options if n.endswith("_COMPILE_DEFINE")}  # computed from a switch, not an input
            self.assertEqual(sorted(options - covered - axes - text_options - derived), [], p["id"])
            if not (root / "cmake" / "config-debug.cmake").exists():
                continue  # partial snapshot: no headers to check
            have = {s["define"] for s in portal.project_switches(M, p) if "define" in s}
            behaviour = re.compile(r"^(PRESERVE|RETAIL_COMPATIBLE|ENABLE|ALLOW|USE|PRIORITIZE|TELL|WW3D_ENABLE)_")
            missing = sorted(n for n, v in source_flags(root).items() if behaviour.match(n) and v in ("0", "1") and n not in have)
            self.assertEqual(missing, [], f"{p['id']}: flags in the source with no switch")


class Prominence(unittest.TestCase):
    """The big switches stay on show; the many small ones must not crowd them out."""

    def test_featured_switches_are_few_and_titled(self):
        for p in M["projects"]:
            featured = [s for s in portal.project_switches(M, p) if s.get("featured")]
            self.assertTrue(8 <= len(featured) <= 20, (p["id"], len(featured)))
            self.assertTrue(all(s.get("title") for s in featured))

    def test_small_flags_are_never_featured(self):
        for s in M["switches"]:
            if s["group"] in ("bugs", "compat", "gameplay", "files"):
                self.assertFalse(s.get("featured"), s["id"])
            if s["id"].startswith("mp_"):
                self.assertFalse(s.get("featured"), s["id"])

    def test_most_switches_are_not_featured(self):
        self.assertGreater(sum(1 for s in M["switches"] if not s.get("featured")), 3 * sum(1 for s in M["switches"] if s.get("featured")))


class Flags(unittest.TestCase):
    def test_labels_are_keyed_by_the_strings_ON_and_OFF(self):
        # a bare ON/OFF key in YAML is read as a boolean, which silently loses the labels
        for s in M["switches"]:
            if "labels" in s:
                self.assertEqual(set(s["labels"]), {"ON", "OFF"}, s["id"])

    def test_code_flags_travel_in_rts_flags_with_quoted_display(self):
        r = portal.resolve(M, sel(bug_tunnel_heal_stacking="OFF", feat_use_buffered_io="OFF"))
        self.assertTrue(r["valid"], r["errors"])
        flags = next(a for a in r["cmake_args"] if a.startswith("-DRTS_FLAGS="))
        self.assertEqual(flags, "-DRTS_FLAGS=/W3;/DPRESERVE_TUNNEL_HEAL_STACKING=0;/DUSE_BUFFERED_IO=0")
        self.assertIn('-DRTS_FLAGS="/W3;/DPRESERVE_TUNNEL_HEAL_STACKING=0;/DUSE_BUFFERED_IO=0"', r["command"])

    def test_no_rts_flags_when_no_code_flag_changed(self):
        r = portal.resolve(M, sel(debug_cheats="ON"))
        self.assertFalse(any(a.startswith("-DRTS_FLAGS=") for a in r["cmake_args"]))

    def test_detail_retail_switches_need_master_on_auto(self):
        self.assertTrue(portal.resolve(M, sel(compat_networking="OFF"))["valid"])
        r = portal.resolve(M, sel(retail_compat="ON", compat_networking="OFF"))
        self.assertFalse(r["valid"])

    def test_crc_compat_follows_the_detail_switches(self):
        self.assertTrue(portal.resolve(M, sel())["retail_crc"])
        self.assertFalse(portal.resolve(M, sel(compat_crc="OFF"))["retail_crc"])
        self.assertFalse(portal.resolve(M, sel(compat_aigroup="OFF"))["retail_crc"])
        self.assertTrue(portal.resolve(M, sel(retail_compat="ON"))["retail_crc"])  # detail switches are not available then

    def test_per_game_build_options_follow_the_game(self):
        self.assertTrue(portal.resolve(M, sel(game="zh", zh_docs="ON"))["valid"])
        self.assertFalse(portal.resolve(M, sel(game="generals", zh_docs="ON"))["valid"])
        self.assertTrue(portal.resolve(M, sel(game="universal", zh_docs="ON", generals_docs="ON"))["valid"])

    def test_mingw_is_a_compiler_with_its_own_presets(self):
        for config, preset in (("release", "mingw-w64-i686"), ("profile", "mingw-w64-i686-profile"), ("debug", "mingw-w64-i686-debug")):
            r = portal.resolve(M, sel(compiler="mingw-clang", config=config))
            self.assertTrue(r["valid"], r["errors"])
            self.assertEqual(r["preset"], preset)
            self.assertFalse(r["retail_crc"])
        self.assertFalse(portal.resolve(M, sel(compiler="mingw-clang", config="releaselog"))["valid"])

    def test_gcc_has_no_profile_build(self):
        for config, preset in (("release", "mingw-w64-i686"), ("debug", "mingw-w64-i686-debug")):
            r = portal.resolve(M, sel(compiler="mingw-gcc", config=config))
            self.assertTrue(r["valid"], r["errors"])
            self.assertEqual(r["preset"], preset)
        self.assertFalse(portal.resolve(M, sel(compiler="mingw-gcc", config="profile"))["valid"])

    def test_mingw_turns_the_tools_off_and_has_no_vc6_or_msvc_only_switches(self):
        self.assertFalse(portal.resolve(M, sel(compiler="mingw-clang", tools="OFF"))["valid"])
        self.assertTrue(portal.resolve(M, sel(compiler="msvc", tools="OFF"))["valid"])
        for sw in ("asan", "ffmpeg", "vc6_full_debug"):
            self.assertFalse(portal.resolve(M, sel(compiler="mingw-clang", **{sw: "ON"}))["valid"], sw)
        self.assertFalse(portal.resolve(M, sel(compiler="mingw-clang", game="universal", zh_tools="OFF"))["valid"])

    def test_vc6_only_and_debug_only_flags(self):
        self.assertTrue(portal.resolve(M, sel(vc6_full_debug="ON"))["valid"])
        self.assertFalse(portal.resolve(M, sel(compiler="msvc", vc6_full_debug="ON"))["valid"])
        self.assertFalse(portal.resolve(M, sel(feat_tell_computer_identity_in_lan_lobby="OFF"))["valid"])
        self.assertTrue(portal.resolve(M, sel(config="debug", feat_tell_computer_identity_in_lan_lobby="OFF"))["valid"])
        self.assertFalse(portal.resolve(M, sel(game="zh", feat_use_obsolete_generals_code="OFF"))["valid"])

    def test_every_variant_still_valid_and_flag_ids_stay_short(self):
        for b in portal.expand(M):
            self.assertLess(len(b["id"]), 60)


@unittest.skipUnless(shutil.which("node"), "node not installed")
class JsParity(unittest.TestCase):
    def test_js_matches_python(self):
        vectors = [sel(), sel(compiler="msvc", config="debug"), sel(debug_cheats="ON"),
                   sel(game="generals", gamememory="OFF"), sel(compiler="msvc", config="debug", asan="ON", gamememory="OFF"),
                   sel(debug_stacktrace="ON", debug_logging="OFF"), sel(compiler="msvc", ffmpeg="ON"),
                   sel(retail_compat="OFF", debug_logging="ON", debug_multi_instance="ON"), sel(compiler="msvc", config="releaselog"),
                   sel(bug_tunnel_heal_stacking="OFF", feat_use_buffered_io="OFF"), sel(game="universal", zh_docs="ON", generals_docs="ON"),
                   sel(game="generals", zh_docs="ON"), sel(compat_crc="OFF"), sel(retail_compat="ON", compat_networking="OFF"),
                   sel(vc6_full_debug="ON"), sel(compiler="mingw-clang", config="debug"), sel(compiler="mingw-clang", tools="OFF"), sel(compiler="msvc", config="debug", feat_tell_computer_identity_in_lan_lobby="OFF", bug_perpetual_horde_bonus="OFF"),
                   dict(sel(), project="generalsx", compiler="linux"), dict(sel(), project="generalsx", compiler="linux", sage_use_sdl3="OFF", feat_use_buffered_io="OFF"),
                   dict(sel(), project="bobtista", compiler="msvc", render_backend="bgfx", bgfx_renderer="vulkan"), dict(sel(), project="bobtista", compiler="macos", config="profile"),
                   dict(sel(), project="bobtista", compiler="linux", game="generals"), dict(sel(), project="generalsonline", compiler="msvc", debug_cheats="ON"),
                   dict(sel(), project="generalsonline", tracy="ON"), dict(sel(), ref="latest"), dict(sel(debug_cheats="ON"), ref="0123456789abcdef"),
                   dict(sel(), ref="nothex"), dict(sel(), project="generalsx", compiler="macos", sage_use_moltenvk="OFF", ref="latest")]
        py = [portal.resolve(M, v) for v in vectors]
        script = (
            "const a=require(%r);const m=JSON.parse(require('fs').readFileSync(%r));"
            "const v=JSON.parse(require('fs').readFileSync(0));"
            "console.log(JSON.stringify(v.map(s=>{const r=a.resolve(m,s);return {valid:r.valid,id:r.id,command:r.command,retail:r.retail_crc,defaults:r.defaults}})))"
            % (str(ROOT / "site" / "app.js"), str(ROOT / "site" / "data" / "schema.json"))
        )
        out = subprocess.run(["node", "-e", script], input=json.dumps(vectors), capture_output=True, text=True, check=True).stdout
        for p, j in zip(py, json.loads(out)):
            self.assertEqual(p["valid"], j["valid"], p)
            if p["valid"]:
                self.assertEqual(p["id"], j["id"])
                self.assertEqual(p["command"], j["command"])
                self.assertEqual(p["retail_crc"], j["retail"])
                self.assertEqual(p["defaults"], j["defaults"])

    def test_ref_suffix_and_hash_round_trip(self):
        script = (
            "const a=require(%r);const v=JSON.parse(require('fs').readFileSync(0));"
            "console.log(JSON.stringify({suf:v.refs.map(a.refSuffix),"
            "rt:v.states.map(s=>{const h=a.hashOf(s);const p=a.parseHash(h);return {h,p}})}))" % str(ROOT / "site" / "app.js")
        )
        refs = ["", "latest", "ABCDEF0123456789", "abc", "main"]
        states = [
            {"project": "ggc", "game": "zh", "compiler": "vc6", "config": "release", "switches": {}, "refMode": "latest", "sha": ""},
            {"project": "bobtista", "game": "universal", "compiler": "linux", "config": "release", "switches": {"render_backend": "bgfx"}, "refMode": "local", "sha": ""},
            {"project": "generalsx", "game": "zh", "compiler": "macos", "config": "release", "switches": {}, "refMode": "commit", "sha": "0123456789abcdef0123"},
        ]
        out = json.loads(subprocess.run(["node", "-e", script], input=json.dumps({"refs": refs, "states": states}), capture_output=True, text=True, check=True).stdout)
        self.assertEqual(out["suf"], [portal.ref_suffix(r) for r in refs])
        for st, rt in zip(states, out["rt"]):
            p = rt["p"]
            for k in ("project", "game", "compiler", "config", "switches", "refMode", "sha"):
                self.assertEqual(p[k], st[k], (rt["h"], k))
        self.assertNotIn("ref=", out["rt"][0]["h"])  # Latest main is the default and is not written
        self.assertIn("ref=local", out["rt"][1]["h"])
        self.assertIn("project=bobtista", out["rt"][1]["h"])

    def test_page_ref_defaults_and_local_option(self):
        """The page asks for the latest source by default; the own-folder option exists only in local mode."""
        app = (ROOT / "site" / "app.js").read_text(encoding="utf-8")
        agent = (ROOT / "site" / "agent.js").read_text(encoding="utf-8")
        script = (
            "const a=require(%r);const h=a.parseHash('');"
            "console.log(JSON.stringify({d:h.refMode,l:a.parseHash('ref=local').refMode,c:a.parseHash('ref=0123456789abcdef').refMode}))" % str(ROOT / "site" / "app.js")
        )
        out = json.loads(subprocess.run(["node", "-e", script], capture_output=True, text=True, check=True).stdout)
        self.assertEqual(out, {"d": "latest", "l": "local", "c": "commit"})
        self.assertIn('refMode: "latest"', app)
        self.assertIn("My own folder as it is", app)
        self.assertNotIn("My checkout as it is", app)
        self.assertIn("ag.isLocal", app)
        self.assertIn("Agent.isLocal", agent)
        # nothing disables a build because no folder is set
        self.assertNotIn("projectState", app + agent)
        self.assertNotIn("No folder set", app + agent)
        for text in (app, agent, (ROOT / "site" / "index.html").read_text(encoding="utf-8")):
            self.assertNotIn("your checkout", text.replace("your own folder", ""))

    def test_page_source_panel_text(self):
        agent = (ROOT / "site" / "agent.js").read_text(encoding="utf-8")
        for needle in ("Use my own copy instead (advanced)", "Use the download again", "Delete the downloaded copy", "/api/projects/clear-cache", "not downloaded yet"):
            self.assertIn(needle, agent)
        self.assertNotIn("Game folders", agent)


if __name__ == "__main__":
    unittest.main()
