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
`.github/workflows/static.yml` publishes `site/` on every push to `main`, at to `https://mirelle7.github.io/RELEASE/`.

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
