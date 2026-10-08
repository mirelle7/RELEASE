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
# The game's source tree, for checking the switches against it (skipped when it is not around).
GAME_SRC = Path(os.environ.get("GAME_SRC", ROOT.parent))
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

    @unittest.skipUnless((GAME_SRC / "CMakePresets.json").exists(), "needs the GeneralsGameCode checkout (set GAME_SRC)")
    def test_every_tier1_preset_exists_in_cmakepresets(self):
        presets = {p["name"] for p in json.loads((GAME_SRC / "CMakePresets.json").read_text())["configurePresets"]}
        for b in portal.expand(M):
            self.assertIn(b["preset"], presets, b["id"])

    @unittest.skipUnless((GAME_SRC / "cmake").exists(), "needs the GeneralsGameCode checkout (set GAME_SRC)")
    def test_every_switch_cmake_var_exists_in_cmake(self):
        text = "".join(p.read_text() for p in (GAME_SRC / "cmake").glob("config-*.cmake"))
        for s in M["switches"]:
            if "cmake" in s:
                self.assertIn(s["cmake"], text, s["id"])


def source_flags():
    """Every `#ifndef X / #define X (N)` flag in the game's headers: {name: value}."""
    pat = re.compile(r"^[ \t]*#ifndef[ \t]+(\w+)[ \t]*\n[ \t]*#define[ \t]+\1[ \t]+\(?(\w+)\)?", re.M)
    out = {}
    for p in GAME_SRC.rglob("*"):
        if p.suffix in (".h", ".hpp", ".inl") and not any(x in p.parts for x in ("Dependencies", "ThirdParty", ".git", "build")):
            for m in pat.finditer(p.read_text(encoding="latin-1")):
                out.setdefault(m.group(1), m.group(2))
    return out


@unittest.skipUnless((GAME_SRC / "cmake").exists(), "needs the GeneralsGameCode checkout (set GAME_SRC)")
class EveryFlagIsExposed(unittest.TestCase):
    """The switch list must cover the source: if a flag is too much, remove it from the source."""

    def test_every_define_switch_matches_the_source(self):
        flags = source_flags()
        for s in M["switches"]:
            if "define" in s:
                self.assertIn(s["define"], flags, f"{s['id']}: {s['define']} is not a guarded flag in the source")
                self.assertEqual(s["default"], "ON" if flags[s["define"]] == "1" else "OFF", f"{s['id']}: source value differs")

    def test_every_behaviour_flag_in_the_source_is_a_switch(self):
        have = {s["define"] for s in M["switches"] if "define" in s}
        behaviour = re.compile(r"^(PRESERVE|RETAIL_COMPATIBLE|ENABLE|ALLOW|USE|PRIORITIZE|TELL|WW3D_ENABLE)_")
        # IG_DEBUG_STACKTRACE is tested with defined(), so it cannot be turned off with =0; debug_stacktrace covers it.
        missing = sorted(n for n, v in source_flags().items() if behaviour.match(n) and v in ("0", "1") and n not in have)
        self.assertEqual(missing, [], "flags in the source with no switch")

    def test_every_cmake_option_is_a_switch_or_an_axis(self):
        text = "".join(p.read_text() for p in (GAME_SRC / "cmake").glob("config-*.cmake"))
        options = set(re.findall(r"^\s*(?:option\(|set\()(RTS_\w+)", text, re.M))
        covered = {s["cmake"] for s in M["switches"] if "cmake" in s}
        # axes of the matrix, not switches: the games, and the profile/debug configurations
        axes = {"RTS_BUILD_ZEROHOUR", "RTS_BUILD_GENERALS", "RTS_BUILD_OPTION_PROFILE", "RTS_BUILD_OPTION_DEBUG"}
        # free-text option, not a flag
        text_options = {"RTS_BUILD_OUTPUT_SUFFIX"}
        self.assertEqual(sorted(options - covered - axes - text_options), [])
        self.assertEqual(sorted(covered - options), [], "switch names that are not CMake options")


class Prominence(unittest.TestCase):
    """The big switches stay on show; the many small ones must not crowd them out."""

    def test_featured_switches_are_few_and_titled(self):
        featured = [s for s in M["switches"] if s.get("featured")]
        self.assertTrue(8 <= len(featured) <= 20, len(featured))
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
            r = portal.resolve(M, sel(compiler="mingw", config=config))
            self.assertTrue(r["valid"], r["errors"])
            self.assertEqual(r["preset"], preset)
            self.assertFalse(r["retail_crc"])
        self.assertFalse(portal.resolve(M, sel(compiler="mingw", config="releaselog"))["valid"])

    def test_mingw_turns_the_tools_off_and_has_no_vc6_or_msvc_only_switches(self):
        self.assertFalse(portal.resolve(M, sel(compiler="mingw", tools="OFF"))["valid"])
        self.assertTrue(portal.resolve(M, sel(compiler="msvc", tools="OFF"))["valid"])
        for sw in ("asan", "ffmpeg", "vc6_full_debug"):
            self.assertFalse(portal.resolve(M, sel(compiler="mingw", **{sw: "ON"}))["valid"], sw)
        self.assertFalse(portal.resolve(M, sel(compiler="mingw", game="universal", zh_tools="OFF"))["valid"])

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
                   sel(vc6_full_debug="ON"), sel(compiler="mingw", config="debug"), sel(compiler="mingw", tools="OFF"), sel(compiler="msvc", config="debug", feat_tell_computer_identity_in_lan_lobby="OFF", bug_perpetual_horde_bonus="OFF")]
        py = [portal.resolve(M, v) for v in vectors]
        script = (
            "const a=require(%r);const m=JSON.parse(require('fs').readFileSync(%r));"
            "const v=JSON.parse(require('fs').readFileSync(0));"
            "console.log(JSON.stringify(v.map(s=>{const r=a.resolve(m,s);return {valid:r.valid,id:r.id,command:r.command,retail:r.retail_crc}})))"
            % (str(ROOT / "site" / "app.js"), str(ROOT / "site" / "data" / "schema.json"))
        )
        out = subprocess.run(["node", "-e", script], input=json.dumps(vectors), capture_output=True, text=True, check=True).stdout
        for p, j in zip(py, json.loads(out)):
            self.assertEqual(p["valid"], j["valid"], p)
            if p["valid"]:
                self.assertEqual(p["id"], j["id"])
                self.assertEqual(p["command"], j["command"])
                self.assertEqual(p["retail_crc"], j["retail"])


if __name__ == "__main__":
    unittest.main()
