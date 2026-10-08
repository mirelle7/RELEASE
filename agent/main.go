// Build agent: a single program that serves the portal page and builds the configurations you pick,
// on this PC, with your own compiler. No runtime to install.
//
//	build-agent.exe                      first run asks for your GeneralsGameCode folder, then opens the page
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
	reFlags  = regexp.MustCompile(`^-DRTS_FLAGS=/W3(;/D[A-Z][A-Z0-9_]{2,80}=[01])+$`)
	reRemote = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,40}$`)
	games    = map[string]bool{"Generals": true, "GeneralsMD": true, "Universal": true}
)

// Job is what the page sends: an already-resolved build.
type Job struct {
	ID     string     `json:"id"`
	Preset string     `json:"preset"`
	Game   string     `json:"game"`            // CI folder name: Generals, GeneralsMD or Universal
	Args   []string   `json:"args"`            // -DRTS_...=ON|OFF|DEFAULT, and -DRTS_FLAGS=/W3;/DNAME=0|1;...
	Range  *RangeSpec `json:"range,omitempty"` // build this configuration at many points in history
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
	ID     string `json:"id"`
	OK     bool   `json:"ok"`
	DryRun bool   `json:"dry_run,omitempty"`
	Error  string `json:"error,omitempty"`
	Group  string `json:"group,omitempty"` // the range this step belongs to
	Label  string `json:"label,omitempty"` // e.g. "PR #510"
	Sha    string `json:"sha,omitempty"`
	At     string `json:"at"`
	AtUnix int64  `json:"at_unix"`
	Files  int    `json:"files,omitempty"`
	Size   int64  `json:"size,omitempty"`
	SHA256 string `json:"sha256,omitempty"`
	File   string `json:"file,omitempty"`
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
	Src, Out, Token string
	Allow           map[string]bool
	DryRun          bool
	Run             Runner
	Site            fs.FS

	Remote string  // git remote that range builds fetch pull requests from
	Git    GitFunc // nil = the real git; replaced in tests

	mu        sync.Mutex
	current   string
	progress  string
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
	a := &Agent{Src: src, Out: out, Token: token, Allow: map[string]bool{}, DryRun: dryRun, Run: run, Remote: "origin", jobs: make(chan queued, 64)}
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

func (a *Agent) presets() (map[string]bool, error) {
	raw, err := os.ReadFile(filepath.Join(a.Src, "CMakePresets.json"))
	if err != nil {
		return nil, fmt.Errorf("no CMakePresets.json in %s: is that your GeneralsGameCode folder?", a.Src)
	}
	var doc struct {
		Configure []struct {
			Name string `json:"name"`
		} `json:"configurePresets"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("CMakePresets.json is not valid JSON: %v", err)
	}
	out := map[string]bool{}
	for _, p := range doc.Configure {
		out[p.Name] = true
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
	known, err := a.presets()
	if err != nil {
		return err
	}
	if !known[j.Preset] {
		return fmt.Errorf("preset %q does not exist in your CMakePresets.json", j.Preset)
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
		a.current, a.progress, a.cancel = j.ID, "", cancel
		a.mu.Unlock()

		if j.Range != nil {
			a.runRange(ctx, j)
		} else {
			a.record(a.build(ctx, a.Src, j, j.ID, nil))
		}

		cancel()
		a.mu.Lock()
		a.current, a.progress, a.cancel = "", "", nil
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
	a.current, a.progress = id, progress
	a.mu.Unlock()
}

// record writes the result next to the build and reports it.
func (a *Agent) record(res Result) {
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
type stepMeta struct{ Group, Label, Sha string }

func (a *Agent) fail(id string, meta *stepMeta, msg string) Result {
	at, unix := now()
	r := Result{ID: id, At: at, AtUnix: unix, Error: msg}
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
	logf, err := os.Create(filepath.Join(dir, "build.log"))
	if err != nil {
		return a.fail(id, meta, err.Error())
	}
	defer logf.Close()
	_ = os.Remove(filepath.Join(dir, id+".zip"))

	steps := [][]string{append([]string{"--preset", j.Preset}, j.Args...), {"--build", "--preset", j.Preset}}
	if a.DryRun {
		for _, s := range steps {
			fmt.Fprintf(logf, "would run: cmake %s\n", strings.Join(s, " "))
		}
		r := a.fail(id, meta, "")
		r.OK, r.DryRun = true, true
		return r
	}
	if _, err := exec.LookPath("cmake"); err != nil && a.Run == nil {
		return a.fail(id, meta, "cmake was not found on PATH. Start the agent from a developer shell with the compiler and CMake set up.")
	}
	ctx, cancel := context.WithTimeout(ctx, buildBudget)
	defer cancel()
	run := a.Run
	if run == nil {
		run = execRunner
	}
	for _, s := range steps {
		if err := run(ctx, src, logf, "cmake", s...); err != nil {
			if errors.Is(ctx.Err(), context.Canceled) {
				return a.fail(id, meta, "cancelled")
			}
			return a.fail(id, meta, fmt.Sprintf("cmake %s failed: %v (see %s)", s[0], err, filepath.Join(dir, "build.log")))
		}
	}

	files := collect(src, j.Preset, j.Game)
	if len(files) == 0 {
		return a.fail(id, meta, "the build finished but produced no .exe/.dll/.pdb under build/"+j.Preset)
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
	r := Result{ID: id, OK: true, At: at, AtUnix: unix, Files: len(files), Size: size, SHA256: sum, File: id + ".zip"}
	if meta != nil {
		r.Group, r.Label, r.Sha = meta.Group, meta.Label, meta.Sha
	}
	return r
}

// collect gathers the binaries the way the CI workflow does.
func collect(src, preset, game string) []string {
	base := filepath.Join(src, "build", preset)
	folders := []string{"Core", game}
	if game == "Universal" { // both games in one package
		folders = []string{"Core", "Generals", "GeneralsMD"}
	}
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
			if !e.IsDir() && (ext == ".exe" || ext == ".dll" || ext == ".pdb") {
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
		st := map[string]any{"busy": a.current != "", "current": a.current, "progress": a.progress, "queue": append([]string{}, a.pending...), "dry_run": a.DryRun}
		a.mu.Unlock()
		st["results"] = a.results()
		writeJSON(w, 200, st)
	}))
	mux.HandleFunc("/api/queue", a.auth(a.handleQueue))
	mux.HandleFunc("/api/cancel", a.auth(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeJSON(w, 405, map[string]string{"error": "POST only"})
			return
		}
		a.Cancel()
		writeJSON(w, 200, map[string]bool{"cancelled": true})
	}))
	mux.HandleFunc("/api/download/", a.auth(a.handleDownload))
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

func (a *Agent) handleDownload(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/api/download/")
	if !reID.MatchString(id) {
		writeJSON(w, 400, map[string]string{"error": "bad id"})
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
		src       = flag.String("src", "", "path to your GeneralsGameCode folder (asked for on first run)")
		port      = flag.Int("port", 8787, "port to listen on (127.0.0.1 only)")
		token     = flag.String("token", "", "fixed token (default: random each run)")
		out       = flag.String("out", filepath.Join(exeDir(), "agent-output"), "where finished builds and logs go")
		dryRun    = flag.Bool("dry-run", false, "accept jobs but build nothing")
		noBrowser = flag.Bool("no-browser", false, "do not open the page automatically")
		remote    = flag.String("remote", "origin", "git remote that range builds fetch pull requests from")
		allow     multiFlag
	)
	flag.Var(&allow, "allow-origin", "also answer a copy of the page hosted at this origin, e.g. https://you.github.io (repeatable)")
	flag.Parse()

	cfgPath := filepath.Join(exeDir(), "agent-config.json")
	if *src == "" {
		if raw, err := os.ReadFile(cfgPath); err == nil {
			var c struct{ Src string }
			_ = json.Unmarshal(raw, &c)
			*src = c.Src
		}
	}
	if *src == "" {
		fmt.Print("Path to your GeneralsGameCode folder: ")
		line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
		*src = strings.Trim(strings.TrimSpace(line), `"`)
	}
	abs, err := filepath.Abs(*src)
	if err != nil {
		pause("bad path: " + err.Error())
		os.Exit(1)
	}
	tok := *token
	if tok == "" {
		tok = newToken()
	}
	if !reRemote.MatchString(*remote) {
		pause("bad --remote name")
		os.Exit(1)
	}
	a := NewAgent(abs, *out, tok, allow, *dryRun, nil)
	a.Remote = *remote
	if _, err := a.presets(); err != nil {
		pause(err.Error())
		os.Exit(1)
	}
	raw, _ := json.Marshal(map[string]string{"Src": abs})
	_ = os.WriteFile(cfgPath, raw, 0o644)

	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", *port))
	if err != nil {
		pause(fmt.Sprintf("cannot listen on port %d (already running?): %v", *port, err))
		os.Exit(1)
	}
	link := fmt.Sprintf("http://127.0.0.1:%d/#agent-token=%s", *port, tok)
	fmt.Println("Build agent running for", abs)
	fmt.Println("Open:", link)
	if len(allow) > 0 {
		fmt.Println("Also answering pages from:", strings.Join(allow, ", "), " token:", tok)
	}
	if *dryRun {
		fmt.Println("(dry run: nothing will be built)")
	}
	fmt.Println("Builds and logs are saved in", *out, "- close this window to stop.")
	if !*noBrowser {
		openBrowser(link)
	}
	srv := &http.Server{Handler: a.Handler(), ReadHeaderTimeout: 10 * time.Second}
	pause(srv.Serve(ln).Error())
}
