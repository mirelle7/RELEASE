# RELEASE

wasm builder and runner

## Generals Build Portal (demo)

A demo website for [GeneralsGameCode](https://github.com/TheSuperHackers/GeneralsGameCode) builds (Generals and Zero Hour,
VC6 SP6 and modern MSVC, the major CMake switches). A configurator tells you whether a combination is valid and gives you
the exact `cmake` command, and the site can run the build on your own PC.

**Universal** is the default build: both games in one package (the game's CMake builds both by default), VC6, Release,
retail-compatible. Single-game and other-compiler builds are still there in the configurator.

## Just look at it

Nothing to install. Open `portal.html` (the whole site in one file) or `site/index.html` in a browser, or visit the hosted
copy at https://mirelle7.github.io/RELEASE/. Any static host works: upload the contents of `site/`.

## Build on my PC

Runs the builds on your computer with your compiler. It is one small program with nothing to install (no Python, no runtime).

1. Download `build-agent-windows-x64.exe` (or `-x86`) from the site's "Build on my PC" section.
2. Open the command prompt that has your compiler and CMake set up (a VS Developer Command Prompt, or after your VC6
   `vcvars32.bat`), then run the agent from it. The first time it asks for the folder your GeneralsGameCode checkout is in
   and remembers it.
3. It opens the page already connected. Pick a configuration and press **Build on my PC**. When it finishes, press
   **Download from my PC**. Builds and logs are kept in `agent-output\` next to the program.

Windows SmartScreen may warn about the program because it is not code-signed ("More info", then "Run anyway"). Its source is
in `agent/` and `sh agent/build.sh` rebuilds it; the SHA-256 of each build is in `build-agent.sha256` next to the download.

Options: `--src <folder>`, `--port 8787`, `--out <folder>`, `--dry-run` (accept jobs, build nothing), `--no-browser`, and
`--allow-origin https://you.github.io` to let a copy of the page hosted elsewhere connect (paste the printed token into the
page). A page opened straight from disk (`portal.html`) can browse and configure but cannot connect to the agent or fetch its download; use the
link the agent opens. Some browsers (Safari) will not let a secure page talk to a local program.

**Safety.** The agent listens on 127.0.0.1 only, requires a token for everything except an "is an agent here?" ping, rejects
requests whose Host header is not loopback (DNS-rebinding guard), and answers other websites only if you allow their origin.
A job is only a CMake preset that must exist in *your* `CMakePresets.json` plus `-DRTS_*=ON|OFF|DEFAULT` switches; anything
else is refused, and CMake is run with an argument list, never a shell string. The page's token is kept in the tab's
session storage only.

## For maintainers

Users need nothing but a browser and, for local builds, the agent. These are only for changing the site:

* **Agent** (Go 1.22+): `cd agent && go test ./...` (includes a real CMake build of a tiny project; skipped if CMake or a C
  compiler is missing). `sh agent/build.sh` embeds the current `site/` and writes the Windows programs to
  `site/downloads/` (git-ignored; the Pages workflow builds them on every deploy).
* **Site data** (Python 3 + PyYAML): `matrix.yaml` is the single source of truth (games, compilers, configs, switches,
  dependency rules, planned builds). `python3 scripts/test_portal.py` checks the rules and that `site/app.js` and
  `scripts/portal.py` agree; `python3 scripts/portal.py generate` rebuilds `site/data/`; `python3 scripts/make_demo.py`
  fills in sample built/failed states; `python3 scripts/bundle.py` rebuilds `portal.html`.
* **Deploy**: `.github/workflows/static.yml` tests and builds the agent, then publishes `site/` to GitHub Pages
  (Settings > Pages > Source: GitHub Actions).
