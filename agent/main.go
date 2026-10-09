// Build agent: a single program that serves the portal page and builds the configurations you pick,
// on this PC, with your own compiler. No runtime to install.
//
//	build-agent.exe                      downloads the game source itself when you build, and opens the page
//	build-agent.exe --src C:\src\Generals --port 8787 --allow-origin https://you.github.io
//
// Start it from a shell where the compiler is already set up (VS Developer Command Prompt, or after your VC6
// vcvars32.bat) with CMake and Ninja on PATH. Nothing here runs text from the page: a job names a CMake preset
// that must exist in your checkout's CMakePresets.json plus -DRTS_*=ON|OFF|DEFAULT switches, and is run as an
// argument list, never through a shell.
package main

import (
	"archive/zip"
	"bufio"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"
)

// The page itself. agent/build.sh copies ../site into ./site before compiling.
//
//go:embed all:site
var siteFS embed.FS

const (
	apiVersion  = 1
	maxBody     = 64 * 1024
	buildBudget = 90 * time.Minute
)

var (
	reID     = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,80}$`)
	rePreset = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)
	reArg    = regexp.MustCompile(`^-DRTS_[A-Z0-9_]{1,80}=(ON|OFF|DEFAULT)$`)
	// Code flags travel as compile definitions in RTS_FLAGS: the presets' /W3, then /DNAME=0 or /DNAME=1 entries.
	reFlags   = regexp.MustCompile(`^-DRTS_FLAGS=/W3(;/D[A-Z][A-Z0-9_]{2,80}=[01])+$`)
	reRemote  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,40}$`)
	reProject = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,30}$`)
	reRef     = regexp.MustCompile(`^[0-9a-f]{7,40}$`)
	games     = map[string]bool{"Generals": true, "GeneralsMD": true, "Universal": true}
)

// Job is what the page sends: an already-resolved build.
type Job struct {
	ID        string     `json:"id"`
	Preset    string     `json:"preset"`
	Game      string     `json:"game"`                // CI folder name: Generals, GeneralsMD or Universal
	Args      []string   `json:"args"`                // -DRTS_...=ON|OFF|DEFAULT, and -DRTS_FLAGS=/W3;/DNAME=0|1;...
	Range     *RangeSpec `json:"range,omitempty"`     // build this configuration at many points in history
	Toolchain string     `json:"toolchain,omitempty"` // MinGW-w64 only: "clang" or "gcc" (empty: what the agent was started with)
	Project   string     `json:"project,omitempty"`   // which game project's checkout to build ("" = ggc)
	Ref       string     `json:"ref,omitempty"`       // "" = the checkout as it is; "latest" = the newest default branch; or a commit sha
	Embed     bool       `json:"embed,omitempty"`     // put your game's binkw32.dll and mss32.dll inside the exe (VC6 and Visual Studio)
}

// RangeSpec builds the same configuration at From, From+Step, ... up to To.
// Unit "pr" checks out the head of that pull request number; "commit" the Nth commit (counted from the oldest,
// first-parent) of the history of your current checkout.
type RangeSpec struct {
	Unit string `json:"unit"`
	From int    `json:"from"`
	To   int    `json:"to"`
	Step int    `json:"step"`
}

const maxRangeSteps = 100

func (r RangeSpec) Points() []int {
	var out []int
	for n := r.From; n <= r.To; n += r.Step {
		out = append(out, n)
	}
	return out
}

func (r RangeSpec) Validate() error {
	switch {
	case r.Unit != "pr" && r.Unit != "commit":
		return errors.New(`range unit must be "pr" or "commit"`)
	case r.From < 1 || r.To < r.From || r.To > 100000000:
		return errors.New("range needs 1 <= from <= to")
	case r.Step < 1:
		return errors.New("range step must be at least 1")
	case len(r.Points()) > maxRangeSteps:
		return fmt.Errorf("that range is %d builds; the limit is %d. Use a larger interval", len(r.Points()), maxRangeSteps)
	}
	return nil
}

// Result is stored as <out>/<id>/manifest.json and reported by /api/status.
type Result struct {
	ID      string    `json:"id"`
	Project string    `json:"project,omitempty"`
	Exes    []ExeInfo `json:"exes"` // the main game executable(s), also kept unzipped next to the zip
	OK      bool      `json:"ok"`
	DryRun  bool      `json:"dry_run,omitempty"`
	Error   string    `json:"error,omitempty"`
	Note    string    `json:"note,omitempty"`  // something worth knowing about a build that succeeded
	Group   string    `json:"group,omitempty"` // the range this step belongs to
	Label   string    `json:"label,omitempty"` // e.g. "PR #510"
	Sha     string    `json:"sha,omitempty"`
	At      string    `json:"at"`
	AtUnix  int64     `json:"at_unix"`
	Files   int       `json:"files,omitempty"`
	Size    int64     `json:"size,omitempty"`
	SHA256  string    `json:"sha256,omitempty"`
	File    string    `json:"file,omitempty"`
}

// Runner runs one command in dir, writing its output to log. Replaced in tests.
type Runner func(ctx context.Context, dir string, log io.Writer, name string, args ...string) error

func execRunner(ctx context.Context, dir string, log io.Writer, name string, args ...string) error {
	fmt.Fprintf(log, "\n$ %s %s\n", name, strings.Join(args, " "))
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir, cmd.Stdout, cmd.Stderr = dir, log, log
	return cmd.Run()
}

type Agent struct {
	Src, Out, Token string            // Src is the folder of the "ggc" project; others are in Srcs
	Srcs            map[string]string // project id -> its source folder (besides ggc)
	srcMu           sync.Mutex
	curProject      string             // project of the job being built (under mu)
	DataDir         string             // your game's folder, where binkw32.dll and mss32.dll are (optional)
	ConfigPath      string             // agent-config.json; "" = do not save settings
	Exit            func(restart bool) // stops the agent, or restarts it (set in main; nil in tests that do not need it)
	Mingw           string             // --mingw-compiler: "clang" (the modern build) or "gcc"; NewAgent defaults to gcc, main to clang
	Native          bool               // --native (default on): VC6 / Visual Studio builds use a compiler installed on this PC when there is one, else a container
	AcceptLicenses  bool               // --accept-licenses: the player agrees to the licences of the compilers the container images download
	Allow           map[string]bool
	DryRun          bool
	Run             Runner
	Site            fs.FS

	RepoURLs map[string]string // project id -> git URL, overriding the built-in registry (tests use a local bare repo)
	CacheDir string            // where downloaded sources live (default: <user cache dir>/gpa)
	APIBase  string            // GitHub REST API base (default https://api.github.com; tests point it at a fake)
	cloneMus sync.Map          // project id -> *sync.Mutex
	sizeMu   sync.Mutex
	sizes    map[string]sizeEntry

	Remote string  // git remote that range builds fetch pull requests from
	Git    GitFunc // nil = the real git; replaced in tests

	env         []string // what every program the agent runs sees (the agent's own environment, plus --env-script)
	GOOS        string   // runtime.GOOS; replaced in tests
	SkipChecks  bool     // --skip-checks: build even if the pre-flight checks fail
	BackendPref string   // --backend: auto, docker, podman, wsl or wsl:<distro>
	Image       string   // container image for the MinGW build
	docMu       sync.Mutex
	docCache    map[string]docEntry

	mu        sync.Mutex
	current   string
	progress  string
	tickMu    sync.Mutex
	tickets   map[string]time.Time // one-time download tickets (see handleTicket)
	startedAt time.Time            // when the current job began (zero when idle)
	pending   []string
	jobs      chan queued
	cancelGen int
	cancel    context.CancelFunc
}

// queued remembers which generation of the queue a job was added in, so Cancel can drop everything older.
type queued struct {
	Job Job
	Gen int
}

func NewAgent(src, out, token string, allow []string, dryRun bool, run Runner) *Agent {
	a := &Agent{Src: src, Out: out, Token: token, Allow: map[string]bool{}, DryRun: dryRun, Run: run, Remote: "origin", Srcs: map[string]string{}, jobs: make(chan queued, 64),
		env: os.Environ(), Native: true, Mingw: "gcc", GOOS: runtime.GOOS, Image: defaultImage, BackendPref: "auto", docCache: map[string]docEntry{}}
	for _, o := range allow {
		a.Allow[strings.TrimRight(o, "/")] = true
	}
	if sub, err := fs.Sub(siteFS, "site"); err == nil {
		a.Site = sub
	}
	go a.worker()
	return a
}

// ---------------------------------------------------------------------------------------------- validation

func (a *Agent) presets() (map[string]bool, error) { return a.presetsFor("") }

func (a *Agent) presetsFor(project string) (map[string]bool, error) {
	names, err := a.presetNamesFor(project)
	if err != nil {
		return nil, err
	}
	out := map[string]bool{}
	for _, n := range names {
		out[n] = true
	}
	return out, nil
}

func (a *Agent) presetNamesFor(project string) ([]string, error) {
	if a.isAuto(project) {
		names, err := readPresetNames(a.cacheDir(normProject(project)))
		if err != nil {
			return nil, fmt.Errorf("the source of project %q has not been downloaded yet", normProject(project))
		}
		return names, nil
	}
	dir, err := a.checkSrc(project)
	if err != nil {
		return nil, err
	}
	names, err := readPresetNames(dir)
	if err != nil {
		return nil, fmt.Errorf("%v (project %q)", err, normProject(project))
	}
	return names, nil
}

func normProject(p string) string {
	if p == "" {
		return "ggc"
	}
	return p
}

// srcFor is the source folder of a project ("" and "ggc" are the GeneralsGameCode checkout); "" when none is set.
func (a *Agent) srcFor(project string) string {
	a.srcMu.Lock()
	defer a.srcMu.Unlock()
	if project == "" || project == "ggc" {
		if a.Src != "" {
			return a.Src
		}
		return a.Srcs["ggc"]
	}
	return a.Srcs[project]
}

// setSrc sets or clears (dir "") a project's folder.
func (a *Agent) setSrc(project, dir string) {
	a.srcMu.Lock()
	defer a.srcMu.Unlock()
	if project == "ggc" || project == "" {
		a.Src = dir
		delete(a.Srcs, "ggc")
		return
	}
	if a.Srcs == nil {
		a.Srcs = map[string]string{}
	}
	if dir == "" {
		delete(a.Srcs, project)
	} else {
		a.Srcs[project] = dir
	}
}

func howToSetProject(project string) string {
	return fmt.Sprintf("Set it on the page (source folders) or start the agent with: -project %s=<folder containing CMakePresets.json>", project)
}

// checkSrc returns the project's folder, or an error naming the project and how to set it.
func (a *Agent) checkSrc(project string) (string, error) {
	p := normProject(project)
	dir := a.srcFor(project)
	if dir == "" {
		return "", fmt.Errorf("no source folder is set for project %q. %s", p, howToSetProject(p))
	}
	if _, err := os.Stat(filepath.Join(dir, "CMakePresets.json")); err != nil {
		return "", fmt.Errorf("no CMakePresets.json in %s, the folder for project %q: is that the right checkout? %s", dir, p, howToSetProject(p))
	}
	return dir, nil
}

// readPresetNames lists the configure presets of a folder's CMakePresets.json.
func readPresetNames(dir string) ([]string, error) {
	raw, err := os.ReadFile(filepath.Join(dir, "CMakePresets.json"))
	if err != nil {
		return nil, fmt.Errorf("no CMakePresets.json in %s", dir)
	}
	var doc struct {
		Configure []struct {
			Name string `json:"name"`
		} `json:"configurePresets"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("CMakePresets.json is not valid JSON: %v", err)
	}
	out := []string{}
	for _, p := range doc.Configure {
		out = append(out, p.Name)
	}
	return out, nil
}

func (a *Agent) Validate(j Job) error {
	switch {
	case !reID.MatchString(j.ID):
		return errors.New("bad build id")
	case !rePreset.MatchString(j.Preset):
		return errors.New("bad preset name")
	case !games[j.Game]:
		return errors.New("game must be Generals, GeneralsMD or Universal")
	case j.Project != "" && !reProject.MatchString(j.Project):
		return errors.New("bad project id")
	case j.Ref != "" && j.Ref != "latest" && !reRef.MatchString(j.Ref):
		return errors.New(`ref must be empty, "latest" or a commit sha`)
	case j.Ref != "" && j.Range != nil:
		return errors.New("a ref cannot be combined with a range")
	case j.Toolchain != "" && j.Toolchain != "clang" && j.Toolchain != "gcc":
		return errors.New("toolchain must be clang or gcc")
	case len(j.Args) > 80:
		return errors.New("too many arguments")
	}
	flagArgs := 0
	for _, arg := range j.Args {
		if reFlags.MatchString(arg) {
			if strings.Count(arg, ";") > 120 {
				return errors.New("too many compile definitions")
			}
			if flagArgs++; flagArgs > 1 {
				return errors.New("only one -DRTS_FLAGS argument is allowed")
			}
		} else if !reArg.MatchString(arg) {
			return fmt.Errorf("argument %q is not an allowed -DRTS_*=ON|OFF|DEFAULT switch or RTS_FLAGS list", arg)
		}
	}
	if j.Range != nil {
		if err := j.Range.Validate(); err != nil {
			return err
		}
	}
	if a.isAuto(j.Project) {
		return nil // the source is downloaded at build time; the preset is checked against the revision then
	}
	known, err := a.presetsFor(j.Project)
	if err != nil {
		return err
	}
	if !known[j.Preset] {
		return fmt.Errorf("preset %q does not exist in the CMakePresets.json of project %q", j.Preset, normProject(j.Project))
	}
	return nil
}

// ---------------------------------------------------------------------------------------------- building

func (a *Agent) worker() {
	for q := range a.jobs {
		j := q.Job
		a.mu.Lock()
		a.pending = remove(a.pending, j.ID)
		if q.Gen < a.cancelGen { // cancelled while it was waiting
			a.mu.Unlock()
			continue
		}
		ctx, cancel := context.WithCancel(context.Background())
		a.current, a.progress, a.cancel, a.startedAt = j.ID, "", cancel, time.Now()
		a.curProject = normProject(j.Project)
		a.mu.Unlock()

		if j.Range != nil {
			a.runRange(ctx, j)
		} else if a.isAuto(j.Project) {
			a.runAuto(ctx, j)
		} else if j.Ref != "" {
			a.runRef(ctx, j)
		} else if src, err := a.checkSrc(j.Project); err != nil {
			a.record(a.fail(j.ID, nil, err.Error()))
		} else {
			a.record(a.build(ctx, src, j, j.ID, nil))
		}

		cancel()
		a.mu.Lock()
		a.current, a.progress, a.cancel, a.startedAt = "", "", nil, time.Time{}
		a.mu.Unlock()
	}
}

// Cancel drops everything waiting and stops the build in progress (a range stops before its next step).
func (a *Agent) Cancel() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.cancelGen++
	a.pending = nil
	if a.cancel != nil {
		a.cancel()
	}
}

func (a *Agent) setCurrent(id, progress string) {
	a.mu.Lock()
	if id != a.current {
		a.startedAt = time.Time{}
		if id != "" {
			a.startedAt = time.Now()
		}
	}
	a.current, a.progress = id, progress
	a.mu.Unlock()
}

// record writes the result next to the build and reports it.
func (a *Agent) record(res Result) {
	if res.Project == "" {
		a.mu.Lock()
		res.Project = a.curProject
		a.mu.Unlock()
	}
	if raw, err := json.MarshalIndent(res, "", " "); err == nil {
		_ = os.MkdirAll(filepath.Join(a.Out, res.ID), 0o755)
		_ = os.WriteFile(filepath.Join(a.Out, res.ID, "manifest.json"), raw, 0o644)
	}
	if res.OK {
		fmt.Printf("built %s\n", res.ID)
	} else {
		fmt.Printf("FAILED %s: %s\n", res.ID, res.Error)
	}
}

func remove(list []string, s string) []string {
	out := list[:0:0]
	for _, x := range list {
		if x != s {
			out = append(out, x)
		}
	}
	return out
}

func now() (string, int64) {
	t := time.Now()
	return t.UTC().Format("2006-01-02 15:04 UTC"), t.Unix()
}

// stepMeta describes where in history a build was made.
type stepMeta struct {
	Group, Label, Sha string
	KeepLog           bool // the log already holds the source download; append to it
}

func (a *Agent) fail(id string, meta *stepMeta, msg string) Result {
	at, unix := now()
	r := Result{ID: id, At: at, AtUnix: unix, Error: msg, Exes: []ExeInfo{}}
	if meta != nil {
		r.Group, r.Label, r.Sha = meta.Group, meta.Label, meta.Sha
	}
	return r
}

// build runs one configuration in src (your checkout, or a temporary worktree for range builds).
func (a *Agent) build(ctx context.Context, src string, j Job, id string, meta *stepMeta) Result {
	dir := filepath.Join(a.Out, id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return a.fail(id, meta, err.Error())
	}
	logFlags := os.O_CREATE | os.O_WRONLY | os.O_TRUNC
	if meta != nil && meta.KeepLog {
		logFlags = os.O_CREATE | os.O_WRONLY | os.O_APPEND
	}
	logf, err := os.OpenFile(filepath.Join(dir, "build.log"), logFlags, 0o644)
	if err != nil {
		return a.fail(id, meta, err.Error())
	}
	defer logf.Close()
	_ = os.Remove(filepath.Join(dir, id+".zip"))
	removeOldExes(dir)

	steps := [][]string{append([]string{"--preset", j.Preset}, j.Args...), {"--build", "--preset", j.Preset}}
	note := ""
	if j.Embed {
		dlls, st := findGameDLLs(a.dataDir())
		switch {
		case !canEmbed(familyOf(j.Preset)):
			note = "binkw32.dll and mss32.dll were not embedded: only Visual C++ 6 and Visual Studio builds can do that."
		case !st.Ready:
			note = "binkw32.dll and mss32.dll were not embedded: " + missingDLLsMessage(st) + " Put them next to the exe to run the game."
		case a.DryRun:
			fmt.Fprintf(logf, "would embed: %s\n", strings.Join(st.Found, ", "))
		default:
			inc, err := prepareEmbed(filepath.Join(dir, "embed"), dlls)
			if err != nil {
				return a.fail(id, meta, "could not prepare the embedded DLLs: "+err.Error())
			}
			steps[0] = append(steps[0], "-DCMAKE_PROJECT_INCLUDE="+filepath.ToSlash(inc))
			fmt.Fprintf(logf, "embedding %s from %s\n", strings.Join(st.Found, ", "), st.Dir)
		}
		if note != "" {
			fmt.Fprintln(logf, note)
		}
	}
	if a.DryRun {
		for _, s := range steps {
			fmt.Fprintf(logf, "would run: cmake %s\n", strings.Join(s, " "))
		}
		r := a.fail(id, meta, "")
		r.OK, r.DryRun, r.Note = true, true, note
		return r
	}
	ctx, cancel := context.WithTimeout(ctx, buildBudget)
	defer cancel()
	fam := familyOf(j.Preset)
	run := a.Run
	if run == nil { // a real build: check this PC first, then run where the preset needs to run
		rep := a.DoctorCachedProj(ctx, j.Preset, j.Toolchain, j.Project)
		if !rep.Ready {
			return a.fail(id, meta, rep.Summary())
		}
		if fam == "linux" || fam == "macos" {
			run = a.nativeRunner()
			steps[1] = append(steps[1], "--parallel", fmt.Sprint(runtime.NumCPU()))
		} else if fam != "mingw" && rep.Image != "" { // no compiler on this PC: build in a container
			b := pickBackendFor(fam, a.Backends(), a.BackendPref)
			if b == nil {
				return a.fail(id, meta, "no container is available for this build")
			}
			spec, ok := imageHere(fam, *b)
			if !ok {
				return a.fail(id, meta, "no container image is defined for this compiler")
			}
			if err := a.ensureImageSpec(ctx, *b, spec, logf); err != nil {
				return a.fail(id, meta, err.Error())
			}
			run = a.containerRunnerImage(*b, spec.Name, spec.Windows)
			if !spec.Windows {
				// VC6's cl fails with C1060 (out of heap space) on the first file that uses a precompiled header under
				// Wine, whatever the job count (it maps the PCH at a fixed address), so Wine builds go without PCH.
				steps[0] = append(steps[0], "-DCMAKE_DISABLE_PRECOMPILE_HEADERS=ON")
				steps[1] = append(steps[1], "--parallel", fmt.Sprint(wineParallel))
			}
		} else if fam == "mingw" {
			b := pickBackend(a.Backends(), a.BackendPref)
			if b == nil {
				return a.fail(id, meta, "no container or WSL is available for the MinGW-w64 build")
			}
			spec, shipped := a.mingwImageFor(j.Toolchain)
			if err := prepareBuildDir(src, j.Preset, a.toolchainOf(j.Toolchain)+mingwStamp); err != nil {
				return a.fail(id, meta, err.Error())
			}
			if shipped && spec.Name != defaultImage { // the Clang recipe; the GCC one keeps its own path below
				if err := a.ensureImageSpec(ctx, *b, spec, logf); err != nil {
					return a.fail(id, meta, err.Error())
				}
			} else if err := a.ensureImage(ctx, *b, logf); err != nil {
				return a.fail(id, meta, err.Error())
			}
			run = a.containerRunnerImage(*b, spec.Name, false)
			// The MinGW preset uses Unix Makefiles, which compile one file at a time unless told otherwise.
			steps[1] = append(steps[1], "--parallel", fmt.Sprint(runtime.NumCPU()))
		} else if !a.Native {
			return a.fail(id, meta, "building with a compiler installed on this PC is paused; this build needs a container (see the checks on the page)")
		} else {
			run = a.nativeRunner()
		}
	}
	for _, s := range steps {
		var err error
		netFail := false
		for attempt := 1; attempt <= 3; attempt++ {
			off := fileSize(filepath.Join(dir, "build.log"))
			if err = run(ctx, src, logf, "cmake", s...); err == nil {
				break
			}
			if ctx.Err() != nil {
				break
			}
			netFail = isNetworkFailure(readFrom(filepath.Join(dir, "build.log"), off))
			if !netFail || attempt == 3 {
				break
			}
			fmt.Fprintf(logf, "\n-- the dependency download failed (network); retrying (%d of 3)...\n", attempt+1)
			select {
			case <-ctx.Done():
			case <-time.After(retryDelays[attempt-1]):
			}
		}
		if err == nil && fam == "mingw" && s[0] == "--preset" {
			a.prepareDX8(ctx, run, src, j.Preset, logf) // configuring downloaded min-dx8-sdk: link its static D3DX8
		}
		if err != nil {
			if errors.Is(ctx.Err(), context.Canceled) {
				return a.fail(id, meta, "cancelled")
			}
			msg := fmt.Sprintf("cmake %s failed: %v", s[0], err)
			if hints := explainFailure(fam, readTail(filepath.Join(dir, "build.log"))); len(hints) > 0 {
				msg += ". " + strings.Join(hints, " ")
			}
			if netFail {
				msg += " (the dependency download failed 3 times: check your internet connection)"
			}
			return a.fail(id, meta, msg+" (details: "+filepath.Join(dir, "build.log")+")")
		}
	}

	files := collect(src, j.Preset, j.Game)
	if len(files) == 0 {
		return a.fail(id, meta, "the build finished but produced no executables or libraries under build/"+j.Preset)
	}
	zpath := filepath.Join(dir, id+".zip")
	info := fmt.Sprintf("id: %s\nbuilt: %s\npreset: %s\nargs: %s\n", id, time.Now().UTC().Format(time.RFC3339), j.Preset, strings.Join(j.Args, " "))
	if meta != nil {
		info += fmt.Sprintf("source: %s %s\n", meta.Label, meta.Sha)
	}
	if err := writeZip(zpath, files, info); err != nil {
		return a.fail(id, meta, err.Error())
	}
	sum, size, err := hashFile(zpath)
	if err != nil {
		return a.fail(id, meta, err.Error())
	}
	at, unix := now()
	exes, err := publishExes(dir, files)
	if err != nil {
		return a.fail(id, meta, err.Error())
	}
	r := Result{ID: id, Exes: exes, Project: normProject(j.Project), OK: true, Note: note, At: at, AtUnix: unix, Files: len(files), Size: size, SHA256: sum, File: id + ".zip"}
	if meta != nil {
		r.Group, r.Label, r.Sha = meta.Group, meta.Label, meta.Sha
	}
	return r
}

func fileSize(path string) int64 {
	if st, err := os.Stat(path); err == nil {
		return st.Size()
	}
	return 0
}

// readFrom returns what was appended to a file after offset off (at most the last 16 KB of it).
func readFrom(path string, off int64) string {
	b, err := os.ReadFile(path)
	if err != nil || int64(len(b)) <= off {
		return ""
	}
	return string(b[off:])
}

// retryDelays are the pauses before the second and third attempt of a step whose dependency download failed.
var retryDelays = []time.Duration{10 * time.Second, 30 * time.Second}

var networkMarkers = []string{"could not read Username for", "the remote end hung up unexpectedly", "invalid index-pack output",
	"Could not resolve host", "Connection timed out", "Failed to connect", "Recv failure", "RPC failed"}

// isNetworkFailure is true when a build log's tail shows a transient dependency download error (not a compile error).
func isNetworkFailure(log string) bool {
	if len(log) > 16384 {
		log = log[len(log)-16384:]
	}
	for _, m := range networkMarkers {
		if strings.Contains(log, m) {
			return true
		}
	}
	return false
}

// collect gathers the binaries the way the CI workflow does.
func collect(src, preset, game string) []string {
	base := filepath.Join(src, "build", preset)
	folders := []string{"Core", game}
	if game == "Universal" { // both games in one package
		folders = []string{"Core", "Generals", "GeneralsMD"}
	}
	fam := familyOf(preset)
	unix := fam == "linux" || fam == "macos"
	cfg := ""
	if strings.HasPrefix(preset, "win32") { // multi-config generators put output in a Debug/Release folder
		cfg = "Release"
		if strings.Contains(preset, "debug") {
			cfg = "Debug"
		}
	}
	var dirs []string
	for _, f := range folders {
		dirs = append(dirs, filepath.Join(base, f, cfg))
	}
	seen := map[string]string{}
	for _, d := range dirs {
		entries, _ := os.ReadDir(d)
		for _, e := range entries {
			ext := strings.ToLower(filepath.Ext(e.Name()))
			if e.IsDir() {
				continue
			}
			keep := ext == ".exe" || ext == ".dll" || ext == ".pdb"
			if unix && !keep {
				keep = ext == ".so" || ext == ".dylib"
				if !keep && ext == "" {
					if info, err := e.Info(); err == nil && info.Mode().IsRegular() && info.Mode().Perm()&0o111 != 0 {
						keep = true
					}
				}
			}
			if keep {
				seen[e.Name()] = filepath.Join(d, e.Name())
			}
		}
	}
	names := make([]string, 0, len(seen))
	for n := range seen {
		names = append(names, n)
	}
	sort.Strings(names)
	out := make([]string, 0, len(names))
	for _, n := range names {
		out = append(out, seen[n])
	}
	return out
}

func writeZip(path string, files []string, info string) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	z := zip.NewWriter(f)
	for _, p := range files {
		w, err := z.Create(filepath.Base(p))
		if err != nil {
			return err
		}
		in, err := os.Open(p)
		if err != nil {
			return err
		}
		_, err = io.Copy(w, in)
		in.Close()
		if err != nil {
			return err
		}
	}
	w, _ := z.Create("BUILD-INFO.txt")
	_, _ = io.WriteString(w, info)
	return z.Close()
}

func hashFile(path string) (string, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	return hex.EncodeToString(h.Sum(nil)), n, err
}

// ---------------------------------------------------------------------------------------------- HTTP

func (a *Agent) results() []Result {
	out := []Result{}
	entries, _ := os.ReadDir(a.Out)
	for _, e := range entries {
		raw, err := os.ReadFile(filepath.Join(a.Out, e.Name(), "manifest.json"))
		if err != nil {
			continue
		}
		var r Result
		if json.Unmarshal(raw, &r) == nil && reID.MatchString(r.ID) {
			out = append(out, r)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].AtUnix > out[j].AtUnix })
	if len(out) > 120 {
		out = out[:120]
	}
	return out
}

func localPort(r *http.Request) string {
	if addr, ok := r.Context().Value(http.LocalAddrContextKey).(net.Addr); ok {
		if _, p, err := net.SplitHostPort(addr.String()); err == nil {
			return p
		}
	}
	return ""
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func (a *Agent) authed(r *http.Request) bool {
	return subtle.ConstantTimeCompare([]byte(r.Header.Get("X-Agent-Token")), []byte(a.Token)) == 1
}

func (a *Agent) allowedOrigin(r *http.Request) string {
	o := r.Header.Get("Origin")
	p := localPort(r)
	if o != "" && (a.Allow[o] || o == "http://127.0.0.1:"+p || o == "http://localhost:"+p) {
		return o
	}
	return ""
}

// Handler: loopback Host check (DNS-rebinding guard), CORS for allowed origins only, then the routes.
func (a *Agent) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/ping", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]any{"agent": "portal-build-agent", "version": apiVersion})
	})
	mux.HandleFunc("/api/status", a.auth(func(w http.ResponseWriter, r *http.Request) {
		a.mu.Lock()
		st := map[string]any{"busy": a.current != "", "current": a.current, "progress": a.progress, "started_unix": unixOrZero(a.startedAt), "queue": append([]string{}, a.pending...), "dry_run": a.DryRun, "skip_checks": a.SkipChecks}
		a.mu.Unlock()
		st["results"] = a.results()
		writeJSON(w, 200, st)
	}))
	mux.HandleFunc("/api/doctor", a.auth(func(w http.ResponseWriter, r *http.Request) {
		preset := r.URL.Query().Get("preset")
		if !rePreset.MatchString(preset) {
			writeJSON(w, 400, map[string]string{"error": "bad preset name"})
			return
		}
		project := r.URL.Query().Get("project")
		if project != "" && !reProject.MatchString(project) {
			writeJSON(w, 400, map[string]string{"error": "bad project id"})
			return
		}
		writeJSON(w, 200, a.DoctorCachedProj(r.Context(), preset, r.URL.Query().Get("toolchain"), project))
	}))
	mux.HandleFunc("/api/session", a.handleSession)
	mux.HandleFunc("/api/log", a.auth(a.handleLog))
	mux.HandleFunc("/api/platforms", a.auth(a.handlePlatforms))
	mux.HandleFunc("/api/projects", a.auth(a.handleProjects))
	mux.HandleFunc("/api/projects/clear-cache", a.auth(a.handleClearCache))
	mux.HandleFunc("/api/commits", a.auth(a.handleCommits))
	mux.HandleFunc("/api/data", a.auth(a.handleData))
	mux.HandleFunc("/api/quit", a.auth(a.handleExit(false)))
	mux.HandleFunc("/api/restart", a.auth(a.handleExit(true)))
	mux.HandleFunc("/api/queue", a.auth(a.handleQueue))
	mux.HandleFunc("/api/cancel", a.auth(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeJSON(w, 405, map[string]string{"error": "POST only"})
			return
		}
		a.Cancel()
		writeJSON(w, 200, map[string]bool{"cancelled": true})
	}))
	mux.HandleFunc("/api/ticket", a.auth(a.handleTicket))
	mux.HandleFunc("/api/download/", a.handleDownload)
	if a.Site != nil {
		mux.Handle("/", http.FileServer(http.FS(a.Site)))
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := localPort(r)
		if p != "" && r.Host != "127.0.0.1:"+p && r.Host != "localhost:"+p {
			http.Error(w, "bad host", http.StatusForbidden)
			return
		}
		if o := a.allowedOrigin(r); o != "" {
			h := w.Header()
			h.Set("Access-Control-Allow-Origin", o)
			h.Set("Vary", "Origin")
			if r.Method == http.MethodOptions {
				h.Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
				h.Set("Access-Control-Allow-Headers", "Content-Type, X-Agent-Token")
				h.Set("Access-Control-Allow-Private-Network", "true")
				h.Set("Access-Control-Max-Age", "600")
				w.WriteHeader(http.StatusNoContent)
				return
			}
		} else if r.Method == http.MethodOptions {
			http.Error(w, "origin not allowed", http.StatusForbidden)
			return
		}
		mux.ServeHTTP(w, r)
	})
}

func (a *Agent) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !a.authed(r) {
			writeJSON(w, 401, map[string]string{"error": "token required"})
			return
		}
		next(w, r)
	}
}

// handleExit answers first, then stops (or restarts) the agent: the build in progress is cancelled.
func (a *Agent) handleExit(restart bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeJSON(w, 405, map[string]string{"error": "POST only"})
			return
		}
		if a.Exit == nil {
			writeJSON(w, 501, map[string]string{"error": "this agent cannot stop itself"})
			return
		}
		writeJSON(w, 200, map[string]bool{"quitting": !restart, "restarting": restart})
		go a.Exit(restart)
	}
}

// quit cancels everything, frees the port and exits; with restart it first starts a copy of itself with the same
// arguments, port and token (so the open page reconnects) and without opening another browser tab.
func (a *Agent) quit(srv *http.Server, restart bool) {
	a.Cancel()
	time.Sleep(300 * time.Millisecond) // let the reply reach the page
	_ = srv.Close()
	if restart {
		if exe, err := os.Executable(); err == nil {
			cmd := exec.Command(exe, os.Args[1:]...)
			cmd.Env = append(os.Environ(), "PORTAL_AGENT_TOKEN="+a.Token, "PORTAL_AGENT_NO_BROWSER=1")
			cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
			if err := cmd.Start(); err != nil {
				fmt.Println("could not restart:", err)
			}
		}
	}
	os.Exit(0)
}

func (a *Agent) handleQueue(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, 405, map[string]string{"error": "POST only"})
		return
	}
	var j Job
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBody))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&j); err != nil {
		writeJSON(w, 400, map[string]string{"error": "bad request body"})
		return
	}
	if err := a.Validate(j); err != nil {
		writeJSON(w, 400, map[string]string{"error": err.Error()})
		return
	}
	a.mu.Lock()
	if a.current == j.ID || contains(a.pending, j.ID) {
		a.mu.Unlock()
		writeJSON(w, 200, map[string]any{"queued": false, "id": j.ID, "reason": "already queued"})
		return
	}
	select {
	case a.jobs <- queued{Job: j, Gen: a.cancelGen}:
		a.pending = append(a.pending, j.ID)
		pos := len(a.pending)
		a.mu.Unlock()
		writeJSON(w, 202, map[string]any{"queued": true, "id": j.ID, "position": pos})
	default:
		a.mu.Unlock()
		writeJSON(w, 429, map[string]string{"error": "queue is full"})
	}
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// handleTicket hands out a one-time, two-minute ticket for downloading one build. The page opens
// /api/download/<id>?ticket=... as an ordinary link, so the browser streams the zip to disk itself (with its own
// progress bar) instead of the page holding the whole file in memory. The token never appears in a URL.
func (a *Agent) handleTicket(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, 405, map[string]string{"error": "POST only"})
		return
	}
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	t := hex.EncodeToString(b)
	a.tickMu.Lock()
	if a.tickets == nil {
		a.tickets = map[string]time.Time{}
	}
	for k, exp := range a.tickets {
		if time.Now().After(exp) {
			delete(a.tickets, k)
		}
	}
	a.tickets[t] = time.Now().Add(2 * time.Minute)
	a.tickMu.Unlock()
	writeJSON(w, 200, map[string]string{"ticket": t})
}

// takeTicket spends a ticket: it works once.
func (a *Agent) takeTicket(t string) bool {
	a.tickMu.Lock()
	defer a.tickMu.Unlock()
	exp, ok := a.tickets[t]
	delete(a.tickets, t)
	return ok && time.Now().Before(exp)
}

func (a *Agent) handleDownload(w http.ResponseWriter, r *http.Request) {
	if !a.authed(r) && !a.takeTicket(r.URL.Query().Get("ticket")) {
		writeJSON(w, 401, map[string]string{"error": "token or ticket required"})
		return
	}
	id := strings.TrimPrefix(r.URL.Path, "/api/download/")
	if !reID.MatchString(id) {
		writeJSON(w, 400, map[string]string{"error": "bad id"})
		return
	}
	if name := r.URL.Query().Get("exe"); name != "" {
		a.serveExe(w, r, id, name)
		return
	}
	path := filepath.Join(a.Out, id, id+".zip")
	if _, err := os.Stat(path); err != nil {
		writeJSON(w, 404, map[string]string{"error": "no such build"})
		return
	}
	w.Header().Set("Content-Disposition", `attachment; filename="`+id+`.zip"`)
	http.ServeFile(w, r, path)
}

// ---------------------------------------------------------------------------------------------- startup

func newToken() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

type multiFlag []string

func (m *multiFlag) String() string     { return strings.Join(*m, ",") }
func (m *multiFlag) Set(s string) error { *m = append(*m, s); return nil }

func openBrowser(url string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	case "darwin":
		cmd = exec.Command("open", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	_ = cmd.Start()
}

func pause(msg string) {
	fmt.Println(msg)
	if runtime.GOOS == "windows" { // keep a double-clicked window open long enough to read
		fmt.Print("Press Enter to close.")
		_, _ = bufio.NewReader(os.Stdin).ReadString('\n')
	}
}

func exeDir() string {
	if p, err := os.Executable(); err == nil {
		return filepath.Dir(p)
	}
	return "."
}

func main() {
	var (
		src       = flag.String("src", "", "optional: use this GeneralsGameCode folder instead of downloading the source")
		port      = flag.Int("port", 8787, "port to listen on (127.0.0.1 only)")
		token     = flag.String("token", "", "fixed token (default: random each run)")
		out       = flag.String("out", filepath.Join(exeDir(), "agent-output"), "where finished builds and logs go")
		dryRun    = flag.Bool("dry-run", false, "accept jobs but build nothing")
		noBrowser = flag.Bool("no-browser", false, "do not open the page automatically")
		remote    = flag.String("remote", "origin", "git remote that range builds fetch pull requests from")
		envScript = flag.String("env-script", "", `a compiler setup script to run first, e.g. "C:\path\to\VCVARS32.BAT" (Windows)`)
		envArgs   = flag.String("env-args", "", "arguments for --env-script, e.g. x86 for vcvarsall.bat")
		skip      = flag.Bool("skip-checks", false, "build even if the pre-flight checks fail (for setups the checks do not understand)")
		backend   = flag.String("backend", "auto", "where the MinGW build runs: auto, docker, podman, wsl or wsl:<distro>")
		image     = flag.String("image", defaultImage, "container image for the MinGW build (default: built from the shipped recipe)")
		checkImg  = flag.Bool("check-image", false, "build and verify the MinGW build environment, then exit")
		native    = flag.Bool("native", true, "use a VC6 / Visual Studio compiler installed on this PC when there is one; with --native=false (or when none is found) builds run in container images")
		mingwCC   = flag.String("mingw-compiler", "clang", "compiler of the MinGW-w64 build: clang (the modern build) or gcc")
		accept    = flag.Bool("accept-licenses", false, "accept the licences of the compilers the VC6 / Visual Studio container images download (needed to build those images)")
		data      = flag.String("data", "", "your game's folder, to embed its binkw32.dll and mss32.dll in VC6/Visual Studio builds (optional)")
		allow     multiFlag
		projects  multiFlag
	)
	flag.Var(&projects, "project", "source folder of another game project, as id=path, e.g. generalsx=/src/GeneralsX (repeatable; -src sets ggc)")
	flag.Var(&allow, "allow-origin", "also answer a copy of the page hosted at this origin, e.g. https://you.github.io (repeatable)")
	flag.Parse()

	cfgProjects := map[string]string{}
	cfgPath := filepath.Join(exeDir(), "agent-config.json")
	if raw, err := os.ReadFile(cfgPath); err == nil {
		c := parseConfig(raw)
		if *src == "" {
			*src = c.Src
		}
		if *data == "" {
			*data = c.Data
		}
		cfgProjects = c.Projects
	}
	abs := ""
	if *src != "" {
		var err error
		if abs, err = filepath.Abs(*src); err != nil {
			pause("bad path: " + err.Error())
			os.Exit(1)
		}
	}
	if cfgProjects == nil {
		cfgProjects = map[string]string{}
	}
	for _, p := range projects {
		id, dir, ok := strings.Cut(p, "=")
		id, dir = strings.TrimSpace(id), strings.Trim(strings.TrimSpace(dir), `"`)
		if !ok || !reProject.MatchString(id) || dir == "" {
			pause("bad -project " + p + ": use id=path, e.g. generalsx=/src/GeneralsX")
			os.Exit(1)
		}
		cfgProjects[id] = dir
	}
	tok := *token
	if tok == "" {
		tok = os.Getenv("PORTAL_AGENT_TOKEN") // set when the agent restarts itself
	}
	if tok == "" {
		tok = newToken()
	}
	if os.Getenv("PORTAL_AGENT_NO_BROWSER") != "" {
		*noBrowser = true
	}
	if !reRemote.MatchString(*remote) {
		pause("bad --remote name")
		os.Exit(1)
	}
	a := NewAgent(abs, *out, tok, allow, *dryRun, nil)
	a.Remote, a.SkipChecks, a.BackendPref, a.Image = *remote, *skip, *backend, *image
	a.ConfigPath, a.AcceptLicenses, a.Native = cfgPath, *accept, *native
	for id, dir := range cfgProjects {
		if d, err := filepath.Abs(dir); err == nil && reProject.MatchString(id) {
			a.setSrc(id, d)
		}
	}
	if *mingwCC != "clang" && *mingwCC != "gcc" {
		pause("--mingw-compiler must be clang or gcc")
		os.Exit(1)
	}
	a.Mingw = *mingwCC
	if *data != "" {
		if d, err := filepath.Abs(*data); err == nil {
			a.DataDir = d
		}
	}
	if *envScript != "" {
		env, err := loadEnvScript(*envScript, *envArgs)
		if err != nil {
			pause(err.Error())
			os.Exit(1)
		}
		a.env = env
		fmt.Println("Loaded the compiler environment from", *envScript)
	}
	if *checkImg {
		os.Exit(a.runCheckImage())
	}
	if abs != "" {
		if _, err := a.presets(); err != nil {
			pause(err.Error())
			os.Exit(1)
		}
	}
	a.saveConfig()

	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", *port))
	if err != nil {
		pause(fmt.Sprintf("cannot listen on port %d (already running?): %v", *port, err))
		os.Exit(1)
	}
	link := fmt.Sprintf("http://127.0.0.1:%d/#agent-token=%s", *port, tok)
	if abs != "" {
		fmt.Println("Build agent running for", abs)
	} else {
		fmt.Println("Build agent running. It downloads each game's source from GitHub the first time you build it (cached in", a.cacheRoot()+").")
	}
	for id, d := range a.projectDirs() {
		fmt.Printf("  project %s: %s\n", id, d)
	}
	fmt.Println("Open:", link)
	if len(allow) > 0 {
		fmt.Println("Also answering pages from:", strings.Join(allow, ", "), " token:", tok)
	}
	if *dryRun {
		fmt.Println("(dry run: nothing will be built)")
	}
	a.printStartupReport()
	fmt.Println("Builds and logs are saved in", *out, "- close this window to stop.")
	if !*noBrowser {
		openBrowser(link)
	}
	srv := &http.Server{Handler: a.Handler(), ReadHeaderTimeout: 10 * time.Second}
	a.Exit = func(restart bool) { a.quit(srv, restart) }
	pause(srv.Serve(ln).Error())
}

// unixOrZero is t as Unix seconds, or 0 for the zero time (whose Unix value is a huge negative number).
func unixOrZero(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.Unix()
}

// handleSession gives the page the agent itself serves its token, so it can connect without a link or a copy-paste.
// Only a same-origin request from that page gets it: a request from another site (Sec-Fetch-Site: cross-site, or an
// Origin that is not this agent) is refused, so the token is not readable from a web page the user happens to visit.
func (a *Agent) handleSession(w http.ResponseWriter, r *http.Request) {
	own := "http://" + r.Host
	site := r.Header.Get("Sec-Fetch-Site")
	if r.Method != http.MethodGet || (site != "" && site != "same-origin") || (r.Header.Get("Origin") != "" && r.Header.Get("Origin") != own) {
		writeJSON(w, 403, map[string]string{"error": "only the page served by this agent can ask for its token"})
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, 200, map[string]string{"agent": "portal-build-agent", "token": a.Token})
}

// ---------------------------------------------------------------------------------------------- plain executables

// ExeInfo is a main game executable of a build, downloadable on its own.
type ExeInfo struct {
	Name string `json:"name"`
	Size int64  `json:"size"`
}

var reExeName = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)

// mainExes picks the game executables out of a build's files: generalsv and generalszh (any case, .exe or none);
// when neither is there, the largest executable called generals*. Tools (WorldBuilder, W3DView...) never match.
func mainExes(files []string) []string {
	var picked []string
	var best string
	var bestSize int64
	for _, p := range files {
		base := filepath.Base(p)
		ext := strings.ToLower(filepath.Ext(base))
		if ext != "" && ext != ".exe" {
			continue
		}
		stem := strings.ToLower(strings.TrimSuffix(base, filepath.Ext(base)))
		if !reExeName.MatchString(base) {
			continue
		}
		if stem == "generalsv" || stem == "generalszh" {
			picked = append(picked, p)
			continue
		}
		if strings.HasPrefix(stem, "generals") {
			if st, err := os.Stat(p); err == nil && st.Size() > bestSize {
				best, bestSize = p, st.Size()
			}
		}
	}
	if len(picked) == 0 && best != "" {
		picked = []string{best}
	}
	return picked
}

// removeOldExes deletes the plain copies a previous build of this id left, as listed in its manifest.
func removeOldExes(dir string) {
	raw, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if err != nil {
		return
	}
	var old Result
	if json.Unmarshal(raw, &old) != nil {
		return
	}
	for _, e := range old.Exes {
		if reExeName.MatchString(e.Name) {
			_ = os.Remove(filepath.Join(dir, e.Name))
		}
	}
}

// publishExes copies the main executables next to the zip and lists them.
func publishExes(dir string, files []string) ([]ExeInfo, error) {
	out := []ExeInfo{}
	for _, p := range mainExes(files) {
		name := filepath.Base(p)
		if name == "manifest.json" || name == "build.log" {
			continue
		}
		in, err := os.Open(p)
		if err != nil {
			return nil, err
		}
		dst, err := os.OpenFile(filepath.Join(dir, name), os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o755)
		if err != nil {
			in.Close()
			return nil, err
		}
		n, err := io.Copy(dst, in)
		in.Close()
		if cerr := dst.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			return nil, err
		}
		out = append(out, ExeInfo{Name: name, Size: n})
	}
	return out, nil
}

// serveExe serves one listed executable of a build, as it is.
func (a *Agent) serveExe(w http.ResponseWriter, r *http.Request, id, name string) {
	notFound := func() { writeJSON(w, 404, map[string]string{"error": "no such executable in that build"}) }
	if !reExeName.MatchString(name) || strings.Contains(name, "..") {
		notFound()
		return
	}
	raw, err := os.ReadFile(filepath.Join(a.Out, id, "manifest.json"))
	if err != nil {
		notFound()
		return
	}
	var res Result
	if json.Unmarshal(raw, &res) != nil {
		notFound()
		return
	}
	for _, e := range res.Exes {
		if e.Name == name {
			f, err := os.Open(filepath.Join(a.Out, id, name))
			if err != nil {
				notFound()
				return
			}
			defer f.Close()
			w.Header().Set("Content-Type", "application/octet-stream")
			w.Header().Set("Content-Disposition", `attachment; filename="`+name+`"`)
			http.ServeContent(w, r, name, time.Time{}, f)
			return
		}
	}
	notFound()
}
