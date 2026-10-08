#!/usr/bin/env python3
"""Zip each downloaded build artifact and write its manifest.json.

Artifact directories are named `<Game>-<preset>[+t][+e]-<build id>`; the build id is
recovered by matching the directory name against the ids in site/data/builds.json.

  package_builds.py ARTIFACTS_DIR OUT_DIR --commit SHA [--log-url URL]
"""
import argparse
import datetime
import hashlib
import json
import zipfile
from pathlib import Path

DATA = Path(__file__).resolve().parent.parent / "site" / "data" / "builds.json"


def sha256(path):
    h = hashlib.sha256()
    with open(path, "rb") as f:
        for chunk in iter(lambda: f.read(1 << 20), b""):
            h.update(chunk)
    return h.hexdigest()


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("artifacts")
    ap.add_argument("out")
    ap.add_argument("--commit", required=True)
    ap.add_argument("--log-url", default=None)
    args = ap.parse_args()

    ids = {b["id"] for b in json.loads(DATA.read_text(encoding="utf-8"))["builds"]}
    out = Path(args.out)
    now = datetime.datetime.now(datetime.timezone.utc).strftime("%Y-%m-%d %H:%M UTC")
    built = 0
    for d in sorted(p for p in Path(args.artifacts).iterdir() if p.is_dir()):
        # Longest id that the directory name ends with (ids can be prefixes of each other).
        match = max((i for i in ids if d.name.endswith("-" + i)), key=len, default=None)
        if match is None:
            print(f"skip {d.name}: no matching build id")
            continue
        dest = out / match
        dest.mkdir(parents=True, exist_ok=True)
        zpath = dest / f"{match}.zip"
        with zipfile.ZipFile(zpath, "w", zipfile.ZIP_DEFLATED) as z:
            for f in sorted(d.rglob("*")):
                if f.is_file():
                    z.write(f, f.relative_to(d))
        manifest = {
            "id": match, "status": "built", "commit": args.commit, "built_at": now,
            "files": [{"name": zpath.name, "url": f"builds/{match}/{zpath.name}",
                       "size": zpath.stat().st_size, "sha256": sha256(zpath)}],
        }
        if args.log_url:
            manifest["log_url"] = args.log_url
        (dest / "manifest.json").write_text(json.dumps(manifest, indent=1), encoding="utf-8")
        built += 1
    print(f"packaged {built} builds")


if __name__ == "__main__":
    main()
