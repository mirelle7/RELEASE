#!/usr/bin/env python3
"""Bundle the whole site into one self-contained HTML file (portal.html).

CSS, JS and data are inlined, and the placeholder download zips are embedded as data: URIs,
so the file works when emailed, opened from disk, or dropped on any host.
"""
import base64
import json
import re
from pathlib import Path

ROOT = Path(__file__).resolve().parent.parent
SITE = ROOT / "site"


def inline_js(src):
    return src.replace("</script", "<\\/script")


def main():
    data = json.loads((SITE / "data" / "portal-data.js").read_text(encoding="utf-8").removeprefix("window.PORTAL_DATA=").rstrip().removesuffix(";"))
    for b in data["builds"]["builds"]:
        for f in b.get("files", []):
            blob = (SITE / f["url"]).read_bytes()
            f["url"] = "data:application/zip;base64," + base64.b64encode(blob).decode()
    payload = "window.PORTAL_EMBEDDED=true;window.PORTAL_DATA=" + json.dumps(data, separators=(",", ":")) + ";"

    html = (SITE / "index.html").read_text(encoding="utf-8")
    css = (SITE / "style.css").read_text(encoding="utf-8")
    js = (SITE / "app.js").read_text(encoding="utf-8")
    html = html.replace('<link rel="stylesheet" href="style.css">', f"<style>\n{css}</style>")
    html = html.replace('<script src="data/portal-data.js"></script>', f"<script>{inline_js(payload)}</script>")
    html = html.replace('<script src="app.js"></script>', f"<script>\n{inline_js(js)}</script>")
    assert not re.search(r'(src|href)="(?!data:|#|http)[^"]*\.(js|css)"', html), "unresolved asset reference"
    out = ROOT / "portal.html"
    out.write_text(html, encoding="utf-8")
    print(f"{out.name}: {out.stat().st_size / 1024:.0f} KB")


if __name__ == "__main__":
    main()
