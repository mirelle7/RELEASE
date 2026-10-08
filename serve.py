#!/usr/bin/env python3
"""Serve the site locally: python3 serve.py [port]   ->  http://localhost:8000"""
import functools
import http.server
import sys
from pathlib import Path

port = int(sys.argv[1]) if len(sys.argv) > 1 else 8000
handler = functools.partial(http.server.SimpleHTTPRequestHandler, directory=str(Path(__file__).parent / "site"))
print(f"http://localhost:{port}  (Ctrl+C to stop)")
http.server.ThreadingHTTPServer(("127.0.0.1", port), handler).serve_forever()
