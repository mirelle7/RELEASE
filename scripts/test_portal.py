#!/usr/bin/env python3
"""Checks matrix.yaml, portal.py and site/app.js agree. Run: python3 scripts/test_portal.py"""
import json
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


class Expansion(unittest.TestCase):
    def test_every_planned_build_valid_and_unique(self):
        builds = portal.expand(M)
        ids = [b["id"] for b in builds]
        self.assertEqual(len(ids), len(set(ids)))
        self.assertGreater(len(ids), 20)

    def test_tier1_covers_both_games(self):
        t1 = [b for b in portal.expand(M) if b["tier"] == 1]
        self.assertEqual({b["game"] for b in t1}, {"generals", "zh"})
        self.assertEqual(len(t1), 2 * len(M["tier1"]))

    @unittest.skipUnless((ROOT.parent / "CMakePresets.json").exists(), "needs the GeneralsGameCode checkout")
    def test_every_tier1_preset_exists_in_cmakepresets(self):
        presets = {p["name"] for p in json.loads((ROOT.parent / "CMakePresets.json").read_text())["configurePresets"]}
        for b in portal.expand(M):
            self.assertIn(b["preset"], presets, b["id"])

    @unittest.skipUnless((ROOT.parent / "cmake").exists(), "needs the GeneralsGameCode checkout")
    def test_every_switch_cmake_var_exists_in_cmake(self):
        text = "".join(p.read_text() for p in (ROOT.parent / "cmake").glob("config-*.cmake"))
        for s in M["switches"]:
            self.assertIn(s["cmake"], text, s["id"])


@unittest.skipUnless(shutil.which("node"), "node not installed")
class JsParity(unittest.TestCase):
    def test_js_matches_python(self):
        vectors = [sel(), sel(compiler="msvc", config="debug"), sel(debug_cheats="ON"),
                   sel(game="generals", gamememory="OFF"), sel(compiler="msvc", config="debug", asan="ON", gamememory="OFF"),
                   sel(debug_stacktrace="ON", debug_logging="OFF"), sel(compiler="msvc", ffmpeg="ON"),
                   sel(retail_compat="OFF", debug_logging="ON", debug_multi_instance="ON"), sel(compiler="msvc", config="releaselog")]
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
