# RELEASE

wasm builder and runner

## Generals Build Portal (demo)

A demo of a website that lists every build of [GeneralsGameCode](https://github.com/TheSuperHackers/GeneralsGameCode)
(Generals and Zero Hour) across both compilers (VC6 SP6 and modern MSVC) and the major CMake switches,
with a configurator that tells you whether a combination is valid and gives you the exact `cmake` command.

**Everything here is sample data.** The "downloads" are placeholder zips containing a text file. There are no
game binaries and no game data in this repo.

## Use it locally

No install needed to just look at it. Either:

* open `site/index.html` in a browser (works straight from disk), or
* `python3 serve.py` and visit http://localhost:8000

## One file

`portal.html` is the whole site in a single self-contained file (styles, script, data and placeholder downloads inlined, 80 KB).
Email it, double-click it, or upload it to any host. Rebuild it with `python3 scripts/bundle.py` after `generate` / `make_demo.py`.
Note GitHub itself shows `.html` files as source, not as a page.

## Use it from the internet (Vercel, Netlify, any static host)

It is plain static files, no build step. On Vercel: **Add New > Project**, import this repo, accept the defaults
(`vercel.json` already sets the output directory to `site`), and deploy. Visitors only ever see your Vercel URL.
Or from a terminal: `npx vercel --prod` in this folder.

### GitHub Pages

Settings > Pages > Source must be **GitHub Actions** (the repo must be public, or on a plan that allows private Pages).
`.github/workflows/static.yml` publishes `site/` on every push to `main`, at `https://mirelle7.github.io/RELEASE/`.

Add a workflow that uploads `site/` with `actions/upload-pages-artifact` and `actions/deploy-pages`, or upload the contents of `site/` to any host.

## Regenerate

```sh
pip install pyyaml
python3 scripts/test_portal.py     # rules + JS/Python parity
python3 scripts/portal.py generate # matrix.yaml -> site/data, ci-matrix.json
python3 scripts/make_demo.py       # sample built/failed/planned states and placeholder zips
```

`matrix.yaml` is the single source of truth (games, compilers, configs, switches, dependency rules, which
builds are made). `site/app.js` re-implements the resolver from `scripts/portal.py`; the tests check they agree.

To turn this into the real portal, point CI at the game repo's build workflow and replace `make_demo.py` with
`package_builds.py` + `portal.py publish`. The full version lives in `build-portal/` of the game fork.

## Tools

### PR triangulator (`tools/triangulate.py`)

Finds duplicate, conflicting and related pull requests by scoring every pair on three signals: what the change
*says* (TF-IDF over title, description, path words and added-line identifiers), *which files* it touches, and
*which lines* of those files. This is lexical similarity, not neural embeddings, so it works offline with no
dependencies but will miss paraphrases that share no vocabulary.

```sh
python3 tools/triangulate.py --input tests/fixtures/prs.json           # sample PRs
python3 tools/triangulate.py --repo owner/name --pr 123                 # live; set GITHUB_TOKEN for private repos / rate limits
python3 tools/triangulate.py --repo owner/name --json > triangles.json
```

Labels: **duplicate** (text, files and lines all agree), **conflict** (same lines touched for a different purpose),
**related** (any other two signals). A single signal is "weak" and only shown with `--all`.
The GitHub fetch is tested against a local mock API, not against live GitHub.

### Local build agent (`tools/agent.py`)

Builds portal configurations on your own Windows PC (where VC6 / VS2022 live) and publishes them into `site/`.
Start it from a shell where the compiler is set up (VS Developer Command Prompt, or after your VC6 `vcvars32.bat`),
with CMake and Ninja on PATH.

```sh
python tools/agent.py plan                                   # what has no artifact yet
python tools/agent.py enqueue --id zh-vc6-release            # or: --select game=zh compiler=vc6 config=release switch=debug_cheats:ON
python tools/agent.py watch --src C:\src\GeneralsGameCode    # builds whatever is queued; --dry-run to preview
python tools/agent.py build --src C:\src\GeneralsGameCode --id zh-vc6-release
```

Each build runs `cmake --preset ...` and `cmake --build --preset ...` in your checkout, zips the `.exe/.dll/.pdb`
files into `site/builds/<id>/`, and records the result in `site/data/builds.json` (the page's status updates on reload).
Custom combinations are added to the site as "on request" builds. Failures are recorded as failed, with the log in
`agent-logs/`. Nothing is pushed; commit `site/` yourself, then run `python3 scripts/bundle.py` if you want the single file.

Safety: jobs go through the same resolver as the website, so a queue file can only select known switches with allowed
values, and the agent passes argument lists to CMake, never shell strings.
The agent has been tested with a fake CMake (no VC6 or MSVC is available where it was written), so the first real
build on your machine is the real test.
