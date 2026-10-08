#!/usr/bin/env python3
"""Tests for the triangulator and the build agent. Run: python3 tools/test_tools.py"""
import http.server
import json
import shutil
import subprocess
import threading
import unittest
from pathlib import Path

import sys
sys.path.insert(0, str(Path(__file__).resolve().parent))
import triangulate as T  # noqa: E402

SAMPLE = Path(__file__).resolve().parent.parent / "site" / "sample-prs.js"
PRS = T.load_sample()


def edge(result, a, b):
    return next((e for e in result["edges"] if {e["a"], e["b"]} == {a, b}), None)


class Triangulator(unittest.TestCase):
    def setUp(self):
        self.r = T.triangulate(json.loads(json.dumps(PRS)))

    def test_duplicates_found(self):
        e = edge(self.r, 101, 102)
        self.assertEqual(e["label"], "duplicate")
        self.assertTrue(all(e["signals"].values()))

    def test_same_lines_different_intent_is_conflict(self):
        e = edge(self.r, 101, 103)
        self.assertEqual(e["label"], "conflict")
        self.assertFalse(e["signals"]["text"])
        self.assertTrue(e["overlapping_files"])

    def test_same_topic_same_file_different_lines_is_related(self):
        e = edge(self.r, 101, 107)
        self.assertEqual(e["label"], "related")
        self.assertTrue(e["signals"]["text"] and e["signals"]["files"] and not e["signals"]["lines"])

    def test_one_weak_signal_is_not_claimed_as_related(self):
        # #105 shares a directory and some vocabulary with #101, but not enough on its own.
        self.assertEqual(edge(self.r, 101, 105)["label"], "weak")

    def test_unrelated_prs_not_linked(self):
        for n in (104, 106):  # neither shares files or vocabulary with the rest
            self.assertFalse(any(n in (e["a"], e["b"]) and e["label"] != "weak" for e in self.r["edges"]), n)

    def test_groups(self):
        self.assertEqual(self.r["groups"], [[101, 102, 103, 107]])

    def test_deterministic(self):
        again = T.triangulate(json.loads(json.dumps(list(reversed(PRS)))))
        self.assertEqual(json.dumps(self.r, sort_keys=True), json.dumps(again, sort_keys=True))

    def test_line_ranges_must_actually_intersect(self):
        a = {"files": [{"path": "x.cpp", "patch": "@@ -10,3 +10,3 @@\n"}]}
        b = {"files": [{"path": "x.cpp", "patch": "@@ -500,3 +500,3 @@\n"}]}
        self.assertEqual(T.line_overlap(a, b, ["x.cpp"]), [])
        b["files"][0]["patch"] = "@@ -12,3 +12,3 @@\n"
        self.assertEqual(T.line_overlap(a, b, ["x.cpp"]), ["x.cpp"])

    def test_missing_patch_is_not_a_line_conflict(self):
        a = {"files": [{"path": "x.bin"}]}
        self.assertEqual(T.line_overlap(a, a, ["x.bin"]), [])

    def test_identifier_splitting(self):
        self.assertEqual(T.split_words("AIGroup::removeAll"), ["ai", "group", "remove", "all"])

    def test_report_and_single_pr_filter(self):
        txt = T.report(self.r, only=104)
        self.assertIn("No related", txt)
        self.assertIn("DUPLICATE", T.report(self.r, only=102))

    def test_empty_and_single(self):
        self.assertEqual(T.triangulate([])["edges"], [])
        self.assertEqual(T.triangulate([PRS[0]])["groups"], [])


@unittest.skipUnless(shutil.which("node"), "node not installed")
class JsParity(unittest.TestCase):
    """site/triangulate.js must agree with triangulate.py on the same input."""

    def run_js(self, prs):
        js = Path(__file__).resolve().parent.parent / "site" / "triangulate.js"
        script = f"const T=require({str(js)!r});const p=JSON.parse(require('fs').readFileSync(0));console.log(JSON.stringify(T.triangulate(p)))"
        out = subprocess.run(["node", "-e", script], input=json.dumps(prs), capture_output=True, text=True, check=True).stdout
        return json.loads(out)

    def test_same_edges_labels_and_groups(self):
        py = T.triangulate(json.loads(json.dumps(PRS)))
        js = self.run_js(PRS)
        self.assertEqual(py["groups"], js["groups"])
        self.assertEqual([(e["a"], e["b"], e["label"]) for e in py["edges"]], [(e["a"], e["b"], e["label"]) for e in js["edges"]])
        for p, j in zip(py["edges"], js["edges"]):
            self.assertAlmostEqual(p["text"], j["text"], delta=0.002)
            self.assertAlmostEqual(p["files"], j["files"], delta=0.002)
            self.assertEqual(p["signals"], j["signals"])
            self.assertEqual(p["overlapping_files"], j["overlapping_files"])

    def test_empty_and_single(self):
        self.assertEqual(self.run_js([])["edges"], [])
        self.assertEqual(self.run_js([PRS[0]])["groups"], [])


class FakeGitHub(http.server.BaseHTTPRequestHandler):
    def do_GET(self):
        if self.path.startswith("/repos/o/r/pulls?"):
            body = [{"number": p["number"], "title": p["title"], "body": p["body"]} for p in PRS[:2]]
        elif "/files" in self.path:
            n = int(self.path.split("/")[5])
            body = [{"filename": f["path"], "patch": f["patch"]} for f in next(p for p in PRS if p["number"] == n)["files"]]
        else:
            self.send_error(404)
            return
        data = json.dumps(body).encode()
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.end_headers()
        self.wfile.write(data)

    def log_message(self, *a):
        pass


class Fetch(unittest.TestCase):
    def test_fetch_against_local_mock_api(self):
        srv = http.server.HTTPServer(("127.0.0.1", 0), FakeGitHub)
        threading.Thread(target=srv.serve_forever, daemon=True).start()
        try:
            prs = T.fetch_github("o/r", None, f"http://127.0.0.1:{srv.server_port}")
        finally:
            srv.shutdown()
        self.assertEqual([p["number"] for p in prs], [101, 102])
        self.assertEqual(T.triangulate(prs)["edges"][0]["label"], "duplicate")


import shutil  # noqa: E402
import tempfile  # noqa: E402
import zipfile  # noqa: E402

import agent as A  # noqa: E402
sys.path.insert(0, str(Path(__file__).resolve().parent.parent / "scripts"))
import portal  # noqa: E402


class Agent(unittest.TestCase):
    """The agent is exercised with a fake cmake: it creates the binaries a real build would."""

    def setUp(self):
        self.tmp = Path(tempfile.mkdtemp())
        self.addCleanup(shutil.rmtree, self.tmp)
        self.site, self.src = self.tmp / "site", self.tmp / "src"
        (self.site / "data").mkdir(parents=True)
        self.m = portal.load_matrix()
        builds = portal.expand(self.m)
        (self.site / "data" / "builds.json").write_text(json.dumps({"commit": None, "builds": builds}))
        (self.site / "data" / "schema.json").write_text(json.dumps(self.m))
        self.calls = []

    def fake_runner(self, produce=True, fail_on=None):
        def runner(cmd, cwd, log):
            self.calls.append(cmd)
            if fail_on and fail_on in cmd:
                return 1
            if produce and cmd[1] == "--build":
                preset = cmd[3]
                d = self.src / "build" / preset / "GeneralsMD"
                if preset.startswith("win32"):
                    d = d / ("Debug" if "debug" in preset else "Release")
                d.mkdir(parents=True, exist_ok=True)
                (d / "generalszh.exe").write_bytes(b"MZ-fake")
                (d / "generalszh.pdb").write_bytes(b"pdb")
                (d / "ignore.obj").write_bytes(b"x")
            return 0
        return runner

    def resolve(self, **kw):
        sel = {"game": "zh", "compiler": "vc6", "config": "release", "switches": {}}
        sel.update(kw)
        r = portal.resolve(self.m, sel)
        self.assertTrue(r["valid"], r["errors"])
        return r

    def test_successful_build_packages_and_records(self):
        r = self.resolve()
        ok = A.build_one(self.m, self.site, self.src, r, logs_dir=self.tmp / "logs", runner=self.fake_runner())
        self.assertTrue(ok)
        self.assertEqual(self.calls[0], ["cmake", "--preset", "vc6", "-DRTS_BUILD_GENERALS=OFF", "-DRTS_BUILD_ZEROHOUR=ON"])
        self.assertEqual(self.calls[1], ["cmake", "--build", "--preset", "vc6"])
        with zipfile.ZipFile(self.site / "builds" / r["id"] / f"{r['id']}.zip") as z:
            self.assertEqual(sorted(z.namelist()), ["BUILD-INFO.txt", "generalszh.exe", "generalszh.pdb"])
        b = next(x for x in A.load_builds(self.site)["builds"] if x["id"] == r["id"])
        self.assertEqual(b["status"], "built")
        self.assertEqual(len(b["files"][0]["sha256"]), 64)
        self.assertIn(f'"id":"{r["id"]}"', (self.site / "data" / "portal-data.js").read_text().replace(" ", ""))

    def test_msvc_multiconfig_collects_from_config_dir(self):
        r = self.resolve(compiler="msvc", config="debug")
        self.assertTrue(A.build_one(self.m, self.site, self.src, r, logs_dir=self.tmp / "l", runner=self.fake_runner()))

    def test_custom_selection_is_added_as_on_request_build(self):
        r = self.resolve(switches={"debug_cheats": "ON", "debug_multi_instance": "ON"})  # not a planned variant
        A.build_one(self.m, self.site, self.src, r, logs_dir=self.tmp / "l", runner=self.fake_runner())
        b = next(x for x in A.load_builds(self.site)["builds"] if x["id"] == r["id"])
        self.assertEqual((b["tier"], b["status"]), (3, "built"))
        self.assertIn("-DRTS_DEBUG_CHEATS=ON", self.calls[0])
        self.assertIn("-DRTS_DEBUG_MULTI_INSTANCE=ON", self.calls[0])

    def test_failed_build_is_recorded_not_published(self):
        r = self.resolve()
        ok = A.build_one(self.m, self.site, self.src, r, logs_dir=self.tmp / "l", runner=self.fake_runner(fail_on="--build"))
        self.assertFalse(ok)
        b = next(x for x in A.load_builds(self.site)["builds"] if x["id"] == r["id"])
        self.assertEqual(b["status"], "failed")
        self.assertFalse((self.site / "builds" / r["id"]).exists())

    def test_build_with_no_binaries_is_a_failure(self):
        r = self.resolve()
        self.assertFalse(A.build_one(self.m, self.site, self.src, r, logs_dir=self.tmp / "l", runner=self.fake_runner(produce=False)))

    def test_dry_run_runs_nothing(self):
        r = self.resolve()
        self.assertTrue(A.build_one(self.m, self.site, self.src, r, dry_run=True, runner=self.fake_runner()))
        self.assertEqual(self.calls, [])

    def test_invalid_jobs_are_rejected(self):
        doc = A.load_builds(self.site)
        for job in ({"id": "nope"}, {"selection": {"game": "zh", "compiler": "vc6", "config": "release", "switches": {"asan": "ON"}}},
                    {"selection": {"game": "zh", "compiler": "vc6", "config": "release", "switches": {"rm -rf /": "ON"}}},
                    {"selection": {"game": "zh", "compiler": "vc6; calc", "config": "release"}}):
            with self.assertRaises(ValueError, msg=job):
                A.resolve_job(self.m, doc, job)

    def test_queue_file_cannot_inject_commands(self):
        job = {"selection": {"game": "zh", "compiler": "vc6", "config": "release", "switches": {"debug_cheats": "ON & calc.exe"}}}
        with self.assertRaises(ValueError):
            A.resolve_job(self.m, A.load_builds(self.site), job)

    def test_parse_selection(self):
        self.assertEqual(A.parse_selection(["game=zh", "compiler=vc6", "config=release", "switch=debug_cheats:ON"]),
                         {"switches": {"debug_cheats": "ON"}, "game": "zh", "compiler": "vc6", "config": "release"})
        with self.assertRaises(SystemExit):
            A.parse_selection(["bogus=1"])


import http.client  # noqa: E402
import time  # noqa: E402


class AgentServer(unittest.TestCase):
    """The page <-> desktop API. Real HTTP on loopback with a fake cmake runner."""

    ALLOWED = "https://demo.example"

    def setUp(self):
        self.tmp = Path(tempfile.mkdtemp())
        self.addCleanup(shutil.rmtree, self.tmp)
        self.site, self.src = self.tmp / "site", self.tmp / "src"
        (self.site / "data").mkdir(parents=True)
        m = portal.load_matrix()
        (self.site / "data" / "builds.json").write_text(json.dumps({"commit": None, "builds": portal.expand(m)}))
        (self.site / "data" / "schema.json").write_text(json.dumps(m))
        (self.site / "hello.txt").write_text("hi")
        self.gate = threading.Event()
        self.gate.set()
        self.started = []

        def runner(cmd, cwd, log):
            if cmd[1] == "--build":
                self.started.append(cmd[3])
                self.gate.wait(5)
                d = self.src / "build" / cmd[3] / "GeneralsMD"
                d.mkdir(parents=True, exist_ok=True)
                (d / "g.exe").write_bytes(b"MZ")
            return 0

        self.httpd, self.token = A.make_server(self.site, self.src, 0, "secret-token", (self.ALLOWED,), False, self.tmp / "logs", runner)
        self.port = self.httpd.server_address[1]
        threading.Thread(target=self.httpd.serve_forever, daemon=True).start()
        self.addCleanup(self.httpd.shutdown)

    def call(self, method, path, body=None, token="secret-token", host=None, origin=None, raw=None):
        c = http.client.HTTPConnection("127.0.0.1", self.port, timeout=5)
        headers = {"Host": host or f"127.0.0.1:{self.port}"}
        if token is not None:
            headers["X-Agent-Token"] = token
        if origin:
            headers["Origin"] = origin
        data = raw if raw is not None else (json.dumps(body) if body is not None else None)
        if data is not None:
            headers["Content-Type"] = "application/json"
        c.request(method, path, data, headers)
        r = c.getresponse()
        payload = r.read()
        try:
            payload = json.loads(payload)
        except ValueError:
            pass
        return r.status, dict(r.getheaders()), payload

    def wait_for(self, pred, secs=5):
        end = time.time() + secs
        while time.time() < end:
            if pred():
                return True
            time.sleep(0.05)
        return False

    def test_ping_is_open_status_needs_token(self):
        self.assertEqual(self.call("GET", "/api/ping", token=None)[2]["agent"], "portal-build-agent")
        self.assertEqual(self.call("GET", "/api/status", token=None)[0], 401)
        self.assertEqual(self.call("GET", "/api/status", token="wrong")[0], 401)
        self.assertEqual(self.call("GET", "/api/status")[0], 200)

    def test_queue_needs_token(self):
        self.assertEqual(self.call("POST", "/api/queue", {"id": "zh-vc6-release"}, token=None)[0], 401)
        self.assertEqual(self.started, [])

    def test_queue_builds_and_publishes(self):
        code, _, body = self.call("POST", "/api/queue", {"id": "zh-vc6-release"})
        self.assertEqual((code, body["queued"]), (202, True))
        self.assertTrue(self.wait_for(lambda: self.call("GET", "/api/status")[2]["recent"]))
        recent = self.call("GET", "/api/status")[2]["recent"][0]
        self.assertEqual((recent["id"], recent["ok"]), ("zh-vc6-release", True))
        doc = json.loads((self.site / "data" / "builds.json").read_text())
        self.assertEqual(next(b for b in doc["builds"] if b["id"] == "zh-vc6-release")["status"], "built")

    def test_custom_selection_accepted(self):
        sel = {"game": "zh", "compiler": "msvc", "config": "release", "switches": {"debug_cheats": "ON", "debug_multi_instance": "ON"}}
        self.assertEqual(self.call("POST", "/api/queue", {"selection": sel})[0], 202)

    def test_invalid_and_hostile_jobs_rejected(self):
        bad = [{"id": "nope"}, {"id": "zh-vc6-release", "selection": {}}, {},
               {"selection": {"game": "zh", "compiler": "vc6", "config": "release", "switches": {"asan": "ON"}}},
               {"selection": {"game": "zh", "compiler": "vc6 & calc", "config": "release"}},
               {"selection": {"game": "zh", "compiler": "vc6", "config": "release", "switches": {"debug_cheats": "ON; calc"}}},
               [1, 2], "x"]
        for job in bad:
            code, _, body = self.call("POST", "/api/queue", job)
            self.assertEqual(code, 400, job)
        self.assertEqual(self.call("POST", "/api/queue", raw="{not json")[0], 400)
        self.assertEqual(self.call("POST", "/api/queue", raw="x" * (A.MAX_BODY + 1))[0], 400)
        time.sleep(0.2)
        self.assertEqual(self.started, [])

    def test_dns_rebinding_host_rejected(self):
        for path in ("/api/ping", "/hello.txt"):
            self.assertEqual(self.call("GET", path, host="evil.example")[0], 403)
        self.assertEqual(self.call("POST", "/api/queue", {"id": "zh-vc6-release"}, host="evil.example")[0], 403)

    def test_cors_only_for_allowed_origins(self):
        ok = self.call("GET", "/api/ping", origin=self.ALLOWED)[1]
        self.assertEqual(ok.get("Access-Control-Allow-Origin"), self.ALLOWED)
        self.assertNotIn("Access-Control-Allow-Origin", self.call("GET", "/api/ping", origin="https://evil.example")[1])
        code, headers, _ = self.call("OPTIONS", "/api/queue", origin=self.ALLOWED)
        self.assertEqual(code, 204)
        self.assertEqual(headers.get("Access-Control-Allow-Private-Network"), "true")
        self.assertIn("X-Agent-Token", headers.get("Access-Control-Allow-Headers", ""))
        self.assertEqual(self.call("OPTIONS", "/api/queue", origin="https://evil.example")[0], 403)

    def test_duplicate_request_is_not_queued_twice(self):
        self.gate.clear()
        self.assertEqual(self.call("POST", "/api/queue", {"id": "zh-vc6-release"})[2]["queued"], True)
        self.assertTrue(self.wait_for(lambda: self.started))
        again = self.call("POST", "/api/queue", {"id": "zh-vc6-release"})[2]
        self.assertEqual(again["queued"], False)
        self.assertEqual(self.call("GET", "/api/status")[2]["current"], "zh-vc6-release")
        self.gate.set()
        self.assertTrue(self.wait_for(lambda: self.call("GET", "/api/status")[2]["recent"]))
        self.assertEqual(self.started, ["vc6"])

    def test_serves_the_site_itself(self):
        code, _, body = self.call("GET", "/hello.txt", token=None)
        self.assertEqual((code, body), (200, b"hi"))

    def test_binds_loopback_only(self):
        self.assertEqual(self.httpd.server_address[0], "127.0.0.1")


if __name__ == "__main__":
    unittest.main()
