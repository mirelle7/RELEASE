# Generals Build Portal: working notes for Claude Code

A static website plus a small local program (the "agent") for building GeneralsGameCode (Command & Conquer Generals and
Zero Hour). Read `README.md` for what it does. This file is what a new session needs to carry on.

## Layout

- `matrix.yaml`: single source of truth (games, compilers, configs, every switch, rules, planned builds).
- `site/`: the website (plain HTML/CSS/JS, no build step). `site/app.js` mirrors `scripts/portal.py` (the resolver).
- `scripts/`: Python, maintainers only (`portal.py generate`, `make_demo.py`, `bundle.py`, `test_portal.py`).
- `agent/`: the local build agent in Go, standard library only. Embeds `site/` (see `agent/build.sh`).
  - `main.go` jobs, queue, HTTP API, security. `range.go` PR/commit range builds in a temporary git worktree.
  - `doctor.go` pre-flight checks and failure explanations. `backend.go` Docker/Podman/WSL. `env.go` environment handling.
  - `toolchain/Dockerfile` the MinGW-w64 build environment image.
- `portal.html`: the whole site in one file (`python3 scripts/bundle.py`). Deployed: `.github/workflows/static.yml` -> GitHub Pages.

## Run the tests

```sh
GAME_SRC=/path/to/GeneralsGameCode python3 scripts/test_portal.py   # also checks every switch against the game's source
cd agent && go vet ./... && GOOS=windows go vet ./... && go test -race -count=1 ./...
```

After changing `site/`, copy it into `agent/site/` (`sh agent/build.sh` does this) or the agent tests run against a stale page.
Regenerate data and the bundle after touching `matrix.yaml`: `python3 scripts/portal.py generate && python3 scripts/make_demo.py && python3 scripts/bundle.py`.

## What the owner wants (do not undo these)

- **Every flag in the source is a switch.** If the interface is too big, the answer is fewer flags in the source. The tests enforce
  this against the game's source. The 15 `featured: true` switches are shown prominently; the other ~55 are folded away.
- **No concept of "default" in the UI** (no default badge, no "Default" button; Auto and "as the source has it" are fine).
- **Never assume the user's PC is set up**: no Python, no git, maybe not Windows, compiler maybe not configured. The agent
  checks and explains (`doctor.go`), and the page disables Build until the PC is ready.
- **No cloud builds.** Containers and WSL are the route for a ready-made environment. Visual C++ 6 and Visual Studio cannot be
  shipped (Microsoft licences), so they run natively on a Windows PC that has them; MinGW-w64 runs in a container or WSL.
- **No data/trademark disclaimer text on the page.** The PR triangulator was removed on purpose: do not bring it back.
- Universal (both games in one package) is a game option, not a "default".
- Do not open pull requests unless asked. Work is pushed straight to `main` of `mirelle7/RELEASE`.

## Never verified (do these first on a real machine)

1. **`agent/toolchain/Dockerfile` has never been built.** Package mirrors were unreachable where it was written. Run
   `build-agent --check-image` (it builds the image and checks gcc/g++/windres, cmake>=3.28, ninja, git, widl). Likely trouble spots:
   the Ubuntu package that provides `widl` and the Wine IDL headers (`wine64-tools`, `libwine-dev`), and the posix-threads alternative.
2. **A real MinGW-w64 build of the game** (`cmake --preset mingw-w64-i686`) inside that image: never run. The tiny-project tests
   prove the plumbing only.
3. **WSL detection** (`backend.go`) is tested with fake `wsl.exe` only. Real `wsl -l -q` output, `--cd`, and path mapping are unproven.
4. **`--env-script`** (`envscript_windows.go`) compiles for Windows but was never run. Test with real `vcvars32.bat` / `vcvarsall.bat x86`.
5. **The Windows executables** build in CI but were never run on Windows. Check SmartScreen behaviour and the first-run prompt.
6. **A real VC6 and modern MSVC build** through the agent: never run (no Windows available). The doctor's compiler detection
   parses the `cl` banner (`Version 12.00.8804 for 80x86`, `Version 19.x for x86`); confirm against real output.
7. Range builds with real git and CMake were tested on a tiny repository, not on the game.
8. The GitHub Pages actions in `static.yml` are referenced by tag, not commit SHA.
9. `build-portal/` in the game repo (`mirelle7/generalsgamecode`, branch `claude/exciting-meitner-kelqv6`) is an older copy of the matrix.

## Gotchas

- YAML 1.1 reads bare `ON`/`OFF`/`on` as booleans. Quote them (`labels: { "ON": ... }`). A test guards `labels`.
- Do not pass `undefined` to `el()` props expecting "no attribute"; set properties like `button.disabled` directly.
- Run `pkill` by exact name (`pkill -x`), never `-f` with text that also appears in your own command line.
- Code flags travel as `-DRTS_FLAGS="/W3;/DNAME=0;..."` (a `;` list). The agent validates this with a strict regex.
- Go's regexp rejects nested repeat counts above 1000: use `+` and check the count in code.

## Suggested next steps

Verify items 1-3 on the faster PC, fix whatever the real image and real game build turn up, then update `README.md` to say what
was verified. After that: bring `build-portal/` in the game repo in line, and pin the Pages actions by SHA.
