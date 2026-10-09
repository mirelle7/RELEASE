# RELEASE

wasm builder and runner

## Generals Build Portal (demo)

A demo website for [GeneralsGameCode](https://github.com/TheSuperHackers/GeneralsGameCode) builds: Generals, Zero Hour, or
both in one package (Universal), with VC6 SP6 or modern MSVC. A configurator tells you whether a combination is valid and
gives you the exact `cmake` command, and the site can run the build on your own PC, including across a range of pull
requests or commits.

## Just look at it

Nothing to install. Open `portal.html` (the whole site in one file) or `site/index.html` in a browser, or visit the hosted
copy at https://mirelle7.github.io/RELEASE/. Any static host works: upload the contents of `site/`.

## Switches

Every compile-time flag the source has is a switch (70 of them). If that is too many, the fix is fewer flags in the source:
the tests fail if the source gains or loses a flag that the list does not match.

| Group | What it is | How it reaches the build |
|---|---|---|
| Retail compatibility | the master switch: Auto leaves the detail switches as the source has them; ON or OFF sets them all | CMake option |
| Retail bugs and quirks (19) | each `PRESERVE_*` flag: **Keep retail** behaviour or **Fixed** | compile definition |
| Retail compatibility, in detail (8) | each `RETAIL_COMPATIBLE_*` flag (CRC, data, saves, pathfinding, networking, ...) | compile definition |
| Gameplay and interface (8), Files and loading (3) | `ENABLE_*`, `ALLOW_*`, `USE_*`, `PRIORITIZE_*`, `TELL_*`, `WW3D_*` flags | compile definition |
| Debug features, Memory, Extras | every CMake option the project defines: logging, crash dumps, the memory pool, ASAN, Tracy, FFmpeg, tools, extras and docs per game, VC6 full debug info | CMake option |

A dot on a button marks the value the source ships with. Code flags are passed in the presets' `RTS_FLAGS` list as
`/DNAME=0` or `/DNAME=1`, which works because the source wraps each one in `#ifndef`. In the source both compilers share the
same values for these flags; the compiler only changes the old-compiler (`_MSC_VER < 1300`) code paths and whether the build
can stay CRC-compatible with retail.

Left out on purpose, because they are not on/off flags: `RTS_BUILD_OUTPUT_SUFFIX` (free text), the key-code and platform
constants in the headers, and `IG_DEBUG_STACKTRACE`, which the source tests with `defined()` so it cannot be turned off with
`=0` (the stack trace switch under Debug features covers it).

## Build on my PC

Runs the builds on your computer with your compiler. It is one small program with nothing to install (no Python, no runtime).

1. Download `build-agent-windows-x64.exe` (or `-x86`) from the site's "Build on my PC" section.
2. Open the command prompt that has your compiler and CMake set up (a VS Developer Command Prompt, or after your VC6
   `vcvars32.bat`), then run the agent from it. It downloads each project's source from GitHub itself the first time you
   build it (a few hundred MB) and keeps it up to date, so it needs nothing but the agent and `git` on the PC (it checks,
   and explains how to get git if missing). It never asks for a game folder; `--src` / `--project` are optional overrides,
   and the page's Source panel can point a project at your own copy instead ("Use my own copy instead (advanced)").
3. It opens the page already connected. Pick a configuration and press **Build on my PC**. When it finishes, press
   **Download from my PC**. Builds and logs are kept in `agent-output\` next to the program.

Windows SmartScreen may warn about the program because it is not code-signed ("More info", then "Run anyway"). Its source is
in `agent/` and `sh agent/build.sh` rebuilds it; the SHA-256 of each build is in `build-agent.sha256` next to the download.

### Build platforms, the whole matrix, and the live view

* **Build platforms on this PC** lists every compiler (Visual C++ 6, modern MSVC, MinGW-w64 + Clang, MinGW-w64 + GCC) with
  a green *ready* or red *not ready* (and what to fix). Click one to select it in the configurator.
* **Build the whole matrix on my PC** (above the matrix) queues every game, compiler and configuration that this PC is
  ready for. Each cell then turns green (built), red (failed) or amber (building), and the line beside the button says
  whether the whole matrix compiled.
* **Live tasks** shows the running build with a timer and its log following along, and a bar of how much of the batch is done.

What has been run for real on one Windows 11 PC (Podman for the containers, Visual Studio Build Tools 18 for MSVC):

| Platform | Result |
|---|---|
| Visual C++ 6 (Podman + Wine) | Release, Profile, Debug, Release + logging, Weekly: green for Generals, Zero Hour and Universal |
| Modern MSVC (native, `--env-script vcvarsall.bat --env-args x86`) | Release, Profile, Debug: green for all three |
| MinGW-w64 + Clang 18 (Podman) | Release, Profile, Debug: green for all three. Profile has no function-level call tracing, because the source writes it in MSVC-only assembly |
| MinGW-w64 + GCC (Podman) | Release, Debug: green for all three. There is no Profile build: gcc cannot compile that assembly |

Not verified: WSL as the container route, the `vcpkg` presets, VC6 or MSVC *in Windows containers*, a second PC.

### Build across history

Under the configurator, **Build across history** builds the configuration you have chosen at many points in the project's
history. Choose pull request numbers or commits, a from and a to, and an interval: from 500, to 600, every 10 builds
500, 510, 520 ... 600 (at most 100 builds). Each finished step is listed under its range with the commit it was built from
and its own download, which makes it easy to find the change where something started to behave differently.

* **Pull request numbers** fetch `pull/N/head` from your git remote (`origin`, or `--remote name`). A number that is not a
  pull request there is reported as failed and the range carries on.
* **Commits** count from the oldest commit of your current branch's first-parent history: commit 1 is the oldest.
* Every step is built in a separate temporary git worktree. Your branch, working files and index are not touched, and the
  worktree is removed afterwards. The only change to your repository is the fetched objects. (A single build, not a range,
  uses CMake's own `build/` folder in the source folder.)
* A revision that predates `CMakePresets.json`, or lacks the preset, is reported as failed for that step.
* **Cancel** next to the progress line stops the running build, drops what is queued, and stops a range before its next step.

### Where the source comes from

Each project is either **auto** (default) or **local** (you gave a folder). In auto mode the agent keeps one blobless git clone per project in `<user cache dir>/gpa/<project>` (on Windows `%LocalAppData%\gpa\ggc`), fetches before each build, checks out the requested commit (or the newest default branch for `latest`, found with `git ls-remote --symref`) and builds there; `build/` is kept so rebuilds are incremental. `GET /api/projects` reports mode, cache size and checked-out commit; `POST /api/projects {"id","dir":""}` switches back to auto and `POST /api/projects/clear-cache {"id"}` deletes the download. Known projects: ggc, generalsx, bobtista, generalsonline; any other id needs a folder.

### Options and safety

Options: `--src <folder>`, `--port 8787`, `--out <folder>`, `--remote origin`, `--dry-run` (accept jobs, build nothing),
`--no-browser`, and `--allow-origin https://you.github.io` to let a copy of the page hosted elsewhere connect (paste the
printed token into the page). A page opened straight from disk (`portal.html`) can browse and configure but cannot connect to
the agent or fetch its download; use the link the agent opens. Some browsers (Safari) will not let a secure page talk to a
local program.

The agent listens on 127.0.0.1 only, requires a token for everything except an "is an agent here?" ping, rejects requests
whose Host header is not loopback (DNS-rebinding guard), and answers other websites only if you allow their origin. A job is
only a CMake preset that must exist in *your* `CMakePresets.json`, `-DRTS_*=ON|OFF|DEFAULT` switches, and one `-DRTS_FLAGS`
list of `/DNAME=0|1` definitions; anything else is refused. CMake and git are run with argument lists, never a shell string,
git never prompts for a password, and a range is capped at 100 builds.

## For maintainers

Users need nothing but a browser and, for local builds, the agent. These are only for changing the site:

* **Agent** (Go 1.22+): `cd agent && go test ./...`. The tests include real git repositories (a range is built from the right
  revisions and your checkout is left exactly as it was) and a real CMake build of a tiny project; they skip when git, CMake
  or a C compiler is missing. `sh agent/build.sh` embeds the current `site/` and writes the Windows programs to
  `site/downloads/` (git-ignored; the Pages workflow builds them on every deploy).
* **Site data** (Python 3 + PyYAML): `matrix.yaml` is the single source of truth (games, compilers, configs, every switch,
  dependency rules, planned builds). `python3 scripts/test_portal.py` checks the rules and that `site/app.js` and
  `scripts/portal.py` agree. With the game's source around, `GAME_SRC=/path/to/GeneralsGameCode python3 scripts/test_portal.py`
  also checks every switch against it: each code flag must exist with the same value, and every CMake option and behaviour
  flag in the source must be a switch. `python3 scripts/portal.py generate` rebuilds `site/data/`;
  `python3 scripts/make_demo.py` fills in sample built/failed states; `python3 scripts/bundle.py` rebuilds `portal.html`.
* **Deploy**: `.github/workflows/static.yml` tests and builds the agent, then publishes `site/` to GitHub Pages
  (Settings > Pages > Source: GitHub Actions).
