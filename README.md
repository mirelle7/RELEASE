# RELEASE

wasm builder and runner

## Generals Build Portal (demo)

A demo of a website that lists every build of [GeneralsGameCode](https://github.com/TheSuperHackers/GeneralsGameCode)
(Generals and Zero Hour) across both compilers (VC6 SP6 and modern MSVC) and the major CMake switches,
with a configurator that tells you whether a combination is valid and gives you the exact `cmake` command.

## Use it locally

No install needed to just look at it. Either:

* open `site/index.html` in a browser (works straight from disk), or
* `python3 serve.py` and visit http://localhost:8000

## One file

`portal.html` is the whole site in a single self-contained file (styles, script, data and placeholder downloads inlined, 80 KB).
Email it, double-click it, or upload it to any host. Rebuild it with `python3 scripts/bundle.py` after `generate` / `make_demo.py`.

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

## What is on the page

Besides the configurator, matrix and variants, the site has two working panels:

* **PR triangulator**: paste PR data (or type `owner/name` to fetch open PRs from GitHub in your browser) and it lists
  **duplicate**, **conflict** and **related** pull requests. It compares what each change says (TF-IDF over title,
  description, path words and added-line identifiers; lexical, not neural embeddings), which files it touches, and
  which lines. "Try the sample" works with no input. The same logic is in `tools/triangulate.py` for the command
  line (`python3 tools/triangulate.py --sample`); `site/triangulate.js` and the Python version are tested to agree.
* **Build on my PC**: connects the page to a build agent on your own Windows machine, adds a **Build on my PC**
  button to the configurator, and shows what is building, queued and done.

### Connecting the page to your PC

On the machine that has VC6 / Visual Studio, in a shell where the compiler is set up (VS Developer Command Prompt, or
after your VC6 `vcvars32.bat`) with CMake and Ninja on PATH:

```sh
python tools/agent.py serve --src C:\src\GeneralsGameCode
```

It prints a link like `http://127.0.0.1:8787/#agent-token=...`. Open it: the page connects by itself, and the agent also
serves this site, so finished builds appear without a refresh. To use a copy of the page hosted elsewhere, start the
agent with `--allow-origin https://you.example` and paste the printed token into the page's Token box.
(A page opened straight from disk, like `portal.html`, can use the triangulator but cannot connect to the agent;
use the agent's link for that.) Some browsers (Safari) block a secure page from reaching a local address.

Each build runs `cmake --preset ...` and `cmake --build --preset ...` in your checkout, zips the `.exe/.dll/.pdb` files
into `site/builds/<id>/`, and records the result in `site/data/builds.json`. Combinations nobody planned are added as
"on request" builds. Failures are recorded with a log in `agent-logs/`. Nothing is pushed anywhere; commit `site/`
yourself, then run `python3 scripts/bundle.py` for the single file.

Other commands: `agent.py plan`, `enqueue`, `build`, `watch` (a folder queue), all with `--dry-run`.

**Safety.** The agent listens on 127.0.0.1 only, requires the token for anything but a "is an agent here?" ping, rejects
requests whose Host header is not loopback (DNS-rebinding guard), and answers other websites only if you allow their
origin. A job can only select known switches with allowed values (the same resolver as the page), and CMake is run with an
argument list, never a shell string. The build queue is held in memory while serving. The agent has been tested with a
fake CMake, since VC6 and MSVC are not available where it was written, so your first real build is the real test.
