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

## Verified on a real PC (Windows 11, Podman, VS Build Tools 18) and what is still open

Verified: the VC6 (Wine) image and real VC6 builds of all 3 games x 5 configs; modern MSVC natively via `--env-script`
(`vcvarsall.bat x86`) for all 3 games x release/profile/debug; MinGW-w64 + Clang and + GCC in Podman (the shipped
Dockerfiles built and work). gcc has no Profile build and clang's Profile stubs out `profile_funclevel.cpp` (MSVC-only asm in
the game source; the real fix is a source `_MSC_VER` guard). `/api/platforms`, the whole-matrix button and the live task
view were exercised in a browser.

Still open:

1. **WSL detection** (`backend.go`) is tested with fake `wsl.exe` only. Real `wsl -l -q` output, `--cd`, and path mapping are unproven.
2. **VC6 / MSVC in Windows containers** (`Dockerfile.*.windows`) never run; `Dockerfile.msvc.linux` (msvc-wine) never run.
3. **The Windows executables** build in CI but SmartScreen behaviour and the first-run prompt are unchecked.
4. Range builds with real git and CMake were tested on a tiny repository, not on the game.
5. The GitHub Pages actions in `static.yml` are referenced by tag, not commit SHA.
6. `build-portal/` in the game repo (`mirelle7/generalsgamecode`, branch `claude/exciting-meitner-kelqv6`) is an older copy of the matrix.
7. The Go tests that fake `docker`/`podman`/`wsl` with shell scripts fail on Windows (they pass on Linux: run them with
   `podman run --rm -v <agent dir>:/w -w /w golang:1.23 go test ./...`). Two clang/gcc builds must not share `build/<preset>`
   at the same time (the agent clears the folder when the compiler changes, `prepareBuildDir`).
9. MinGW exes are linked `-static` (no libwinpthread-1.dll) and against Microsoft's static `d3dx8.lib` from min-dx8-sdk instead of the
   debug import library (which needs d3dx8d.dll from the DirectX SDK): `agent/dx8shim.go` + `agent/toolchain/gpa_msvc_compat.cpp`
   (a few MSVC runtime helpers the lib needs) build `build/gpa-override/libd3dx8d.a` after configure. Verified by inspecting the exe imports
   (clang on latest main, gcc on an older checkout); the built exe has never been run on Windows.
10. GCC cannot link the *latest* upstream main (2119cc9, undefined `MOTDSystem`, `OSDisplaySetBusyState`, winsock `_imp__listen`...) even
   without our flags: an upstream problem, older source links. Clang links it.
8. The VC6 image copies the source onto the container's own disk (the Windows bind mount is far too slow); bump its tag if you change the recipe.

## Gotchas

- YAML 1.1 reads bare `ON`/`OFF`/`on` as booleans. Quote them (`labels: { "ON": ... }`). A test guards `labels`.
- Do not pass `undefined` to `el()` props expecting "no attribute"; set properties like `button.disabled` directly.
- Run `pkill` by exact name (`pkill -x`), never `-f` with text that also appears in your own command line.
- Code flags travel as `-DRTS_FLAGS="/W3;/DNAME=0;..."` (a `;` list). The agent validates this with a strict regex.
- Go's regexp rejects nested repeat counts above 1000: use `+` and check the count in code.

## Suggested next steps

Fix item 7 (make the fake-tool tests portable), verify WSL (item 1), then bring `build-portal/` in the game repo in line and
pin the Pages actions by SHA.
