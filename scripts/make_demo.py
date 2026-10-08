#!/usr/bin/env python3
"""Fill the site with sample data so the portal looks lived-in.

Marks a handful of planned builds as built (with a placeholder zip), one as failed,
and leaves the rest planned. The zips contain only a text file saying so; there are
no game binaries anywhere in this repo.
"""
import hashlib
import json
import zipfile
from pathlib import Path

SITE = Path(__file__).resolve().parent.parent / "site"
BUILT = {
    "zh-vc6-release", "generals-vc6-release", "zh-msvc-release", "generals-msvc-release",
    "zh-vc6-debug", "zh-msvc-debug", "zh-vc6-profile", "generals-vc6-releaselog",
}
FAILED = {"generals-msvc-profile"}
STAMP = "2026-10-08 03:00 UTC (sample)"

doc = json.loads((SITE / "data" / "builds.json").read_text(encoding="utf-8"))
doc["commit"] = "demo-sample"
for b in doc["builds"]:
    bid = b["id"]
    if bid in FAILED:
        b.update(status="failed", built_at=STAMP)
    elif bid in BUILT or (b["tier"] == 2 and b["game"] == "zh" and b["compiler"] == "vc6" and b["config"] == "release"):
        d = SITE / "builds" / bid
        d.mkdir(parents=True, exist_ok=True)
        z = d / f"{bid}.zip"
        with zipfile.ZipFile(z, "w", zipfile.ZIP_DEFLATED) as zf:
            zf.writestr("DEMO-README.txt",
                        f"PLACEHOLDER for build {bid}\n\nThis is a demo. This archive contains no executable.\n"
                        f"A real build of this configuration would be produced with:\n\n  {b['command']}\n")
        digest = hashlib.sha256(z.read_bytes()).hexdigest()
        b.update(status="built", commit="demo-sample", built_at=STAMP,
                 files=[{"name": z.name, "url": f"builds/{bid}/{z.name}", "size": z.stat().st_size, "sha256": digest}])
(SITE / "data" / "builds.json").write_text(json.dumps(doc, indent=1), encoding="utf-8")
(SITE / "data" / "portal-data.js").write_text(
    "window.PORTAL_DATA=" + json.dumps({"schema": json.loads((SITE / "data" / "schema.json").read_text(encoding="utf-8")), "builds": doc}) + ";\n",
    encoding="utf-8")
print(sum(b["status"] == "built" for b in doc["builds"]), "built,", sum(b["status"] == "failed" for b in doc["builds"]), "failed,",
      sum(b["status"] == "planned" for b in doc["builds"]), "planned")
