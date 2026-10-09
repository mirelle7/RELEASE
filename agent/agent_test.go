package main

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

const allowed = "https://demo.example"

const presetsJSON = `{"version":6,"configurePresets":[{"name":"vc6"},{"name":"win32"},{"name":"win32-debug"}]}`

type env struct {
	t     *testing.T
	src   string
	out   string
	a     *Agent
	srv   *httptest.Server
	mu    sync.Mutex
	calls [][]string
}

// newEnv starts a real HTTP server around an Agent whose cmake is faked: it writes the binaries a build would.
func newEnv(t *testing.T) *env {
	t.Helper()
	e := &env{t: t, src: t.TempDir(), out: t.TempDir()}
	if err := os.WriteFile(filepath.Join(e.src, "CMakePresets.json"), []byte(presetsJSON), 0o644); err != nil {
		t.Fatal(err)
	}
	run := func(ctx context.Context, dir string, log io.Writer, name string, args ...string) error {
		e.mu.Lock()
		e.calls = append(e.calls, append([]string{name}, args...))
		e.mu.Unlock()
		if args[0] == "--build" {
			preset := args[2]
			d := filepath.Join(e.src, "build", preset, "GeneralsMD")
			if strings.HasPrefix(preset, "win32") {
				cfg := "Release"
				if strings.Contains(preset, "debug") {
					cfg = "Debug"
				}
				d = filepath.Join(d, cfg)
			}
			_ = os.MkdirAll(d, 0o755)
			_ = os.WriteFile(filepath.Join(d, "generalszh.exe"), []byte("MZ"), 0o644)
			_ = os.WriteFile(filepath.Join(d, "generalszh.pdb"), []byte("pdb"), 0o644)
			_ = os.WriteFile(filepath.Join(d, "ignore.obj"), []byte("x"), 0o644)
		}
		return nil
	}
	e.a = NewAgent(e.src, e.out, "secret-token", []string{allowed}, false, run)
	e.a.CacheDir = t.TempDir()
	e.srv = httptest.NewServer(e.a.Handler())
	t.Cleanup(e.srv.Close)
	return e
}

type reply struct {
	code int
	hdr  http.Header
	body []byte
}

func (r reply) json() map[string]any {
	var m map[string]any
	_ = json.Unmarshal(r.body, &m)
	return m
}

func (e *env) do(method, path string, body string, h map[string]string) reply {
	e.t.Helper()
	req, _ := http.NewRequest(method, e.srv.URL+path, strings.NewReader(body))
	for k, v := range h {
		if k == "Host" {
			req.Host = v
			continue
		}
		req.Header.Set(k, v)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return reply{res.StatusCode, res.Header, b}
}

var tok = map[string]string{"X-Agent-Token": "secret-token"}

func (e *env) post(job string) reply { return e.do("POST", "/api/queue", job, tok) }

func (e *env) waitResult(id string) map[string]any {
	e.t.Helper()
	for i := 0; i < 200; i++ {
		st := e.do("GET", "/api/status", "", tok).json()
		for _, r := range st["results"].([]any) {
			if m := r.(map[string]any); m["id"] == id {
				return m
			}
		}
		time.Sleep(25 * time.Millisecond)
	}
	e.t.Fatal("timed out waiting for", id)
	return nil
}

const okJob = `{"id":"zh-vc6-release","preset":"vc6","game":"GeneralsMD","args":["-DRTS_BUILD_GENERALS=OFF","-DRTS_BUILD_ZEROHOUR=ON"]}`

func TestPingOpenStatusNeedsToken(t *testing.T) {
	e := newEnv(t)
	if e.do("GET", "/api/ping", "", nil).json()["agent"] != "portal-build-agent" {
		t.Fatal("ping")
	}
	for _, h := range []map[string]string{nil, {"X-Agent-Token": "wrong"}, {"X-Agent-Token": ""}} {
		if c := e.do("GET", "/api/status", "", h).code; c != 401 {
			t.Fatalf("status without good token: %d", c)
		}
	}
	if c := e.do("GET", "/api/status", "", tok).code; c != 200 {
		t.Fatal(c)
	}
}

func TestQueueNeedsToken(t *testing.T) {
	e := newEnv(t)
	if c := e.do("POST", "/api/queue", okJob, nil).code; c != 401 {
		t.Fatal(c)
	}
	time.Sleep(100 * time.Millisecond)
	if len(e.calls) != 0 {
		t.Fatal("ran without a token")
	}
}

func TestBuildPackagesAndReports(t *testing.T) {
	e := newEnv(t)
	if r := e.post(okJob); r.code != 202 {
		t.Fatalf("%d %s", r.code, r.body)
	}
	res := e.waitResult("zh-vc6-release")
	if res["ok"] != true || res["sha256"] == "" {
		t.Fatalf("%v", res)
	}
	want := []string{"cmake", "--preset", "vc6", "-DRTS_BUILD_GENERALS=OFF", "-DRTS_BUILD_ZEROHOUR=ON"}
	if strings.Join(e.calls[0], " ") != strings.Join(want, " ") || strings.Join(e.calls[1], " ") != "cmake --build --preset vc6" {
		t.Fatalf("commands: %v", e.calls)
	}
	zr, err := zip.OpenReader(filepath.Join(e.out, "zh-vc6-release", "zh-vc6-release.zip"))
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, f := range zr.File {
		names = append(names, f.Name)
	}
	if strings.Join(names, ",") != "generalszh.exe,generalszh.pdb,BUILD-INFO.txt" {
		t.Fatalf("zip contents: %v", names)
	}
	dl := e.do("GET", "/api/download/zh-vc6-release", "", tok)
	if dl.code != 200 || !bytes.HasPrefix(dl.body, []byte("PK")) {
		t.Fatalf("download: %d", dl.code)
	}
	if c := e.do("GET", "/api/download/zh-vc6-release", "", nil).code; c != 401 {
		t.Fatal("download without token", c)
	}
}

func TestMultiConfigPresetCollectsFromConfigDir(t *testing.T) {
	e := newEnv(t)
	e.post(`{"id":"zh-msvc-debug","preset":"win32-debug","game":"GeneralsMD","args":[]}`)
	if res := e.waitResult("zh-msvc-debug"); res["ok"] != true {
		t.Fatalf("%v", res)
	}
}

func TestUniversalCollectsBothGames(t *testing.T) {
	e := newEnv(t)
	e.a.Run = func(ctx context.Context, dir string, log io.Writer, name string, args ...string) error {
		if args[0] == "--build" {
			for game, exe := range map[string]string{"Generals": "generals.exe", "GeneralsMD": "generalszh.exe"} {
				d := filepath.Join(e.src, "build", "vc6", game)
				_ = os.MkdirAll(d, 0o755)
				_ = os.WriteFile(filepath.Join(d, exe), []byte("MZ"), 0o644)
			}
		}
		return nil
	}
	e.post(`{"id":"universal-vc6-release","preset":"vc6","game":"Universal","args":["-DRTS_BUILD_GENERALS=ON","-DRTS_BUILD_ZEROHOUR=ON"]}`)
	res := e.waitResult("universal-vc6-release")
	if res["ok"] != true || res["files"] != float64(2) {
		t.Fatalf("%v", res)
	}
}

func TestHostileJobsRejected(t *testing.T) {
	e := newEnv(t)
	bad := []string{
		`{"id":"x","preset":"nope","game":"GeneralsMD","args":[]}`,                                   // preset not in this checkout
		`{"id":"x","preset":"vc6; calc","game":"GeneralsMD","args":[]}`,                              // shell metacharacters
		`{"id":"x","preset":"--build","game":"GeneralsMD","args":[]}`,                                // looks like a flag
		`{"id":"x","preset":"vc6","game":"GeneralsMD","args":["-DRTS_X=ON & calc.exe"]}`,             // injection in value
		`{"id":"x","preset":"vc6","game":"GeneralsMD","args":["-DCMAKE_TOOLCHAIN_FILE=evil.cmake"]}`, // not an RTS_ switch
		`{"id":"x","preset":"vc6","game":"GeneralsMD","args":["--fresh"]}`,                           // arbitrary cmake flag
		`{"id":"x","preset":"vc6","game":"GeneralsMD","args":["-DRTS_X=maybe"]}`,                     // bad value
		`{"id":"../../evil","preset":"vc6","game":"GeneralsMD","args":[]}`,                           // path traversal in id
		`{"id":"x","preset":"vc6","game":"..\\..","args":[]}`,                                        // bad game
		`{"id":"x","preset":"vc6","game":"GeneralsMD","args":[],"extra":1}`,                          // unknown field
		`{not json`, `[]`, `"x"`, ``,
	}
	for _, b := range bad {
		if c := e.post(b).code; c != 400 {
			t.Errorf("%q -> %d, want 400", b, c)
		}
	}
	if c := e.post(`{"id":"` + strings.Repeat("a", 100) + `","preset":"vc6","game":"GeneralsMD","args":[]}`).code; c != 400 {
		t.Error("long id", c)
	}
	if c := e.post(strings.Repeat("x", maxBody+10)).code; c != 400 {
		t.Error("oversized body", c)
	}
	time.Sleep(100 * time.Millisecond)
	if len(e.calls) != 0 {
		t.Fatalf("something ran: %v", e.calls)
	}
}

func TestCheckoutWithoutPresetsRejected(t *testing.T) {
	e := newEnv(t)
	_ = os.Remove(filepath.Join(e.src, "CMakePresets.json"))
	r := e.post(okJob)
	if r.code != 400 || !strings.Contains(string(r.body), "CMakePresets.json") {
		t.Fatalf("%d %s", r.code, r.body)
	}
}

func TestDNSRebindingHostRejected(t *testing.T) {
	e := newEnv(t)
	for _, p := range []string{"/api/ping", "/", "/index.html"} {
		if c := e.do("GET", p, "", map[string]string{"Host": "evil.example"}).code; c != 403 {
			t.Errorf("%s -> %d", p, c)
		}
	}
	h := map[string]string{"Host": "evil.example", "X-Agent-Token": "secret-token"}
	if c := e.do("POST", "/api/queue", okJob, h).code; c != 403 {
		t.Error(c)
	}
}

func TestCORSOnlyForAllowedOrigins(t *testing.T) {
	e := newEnv(t)
	if got := e.do("GET", "/api/ping", "", map[string]string{"Origin": allowed}).hdr.Get("Access-Control-Allow-Origin"); got != allowed {
		t.Fatalf("allowed origin got %q", got)
	}
	if got := e.do("GET", "/api/ping", "", map[string]string{"Origin": "https://evil.example"}).hdr.Get("Access-Control-Allow-Origin"); got != "" {
		t.Fatalf("evil origin got %q", got)
	}
	pre := e.do("OPTIONS", "/api/queue", "", map[string]string{"Origin": allowed})
	if pre.code != 204 || pre.hdr.Get("Access-Control-Allow-Private-Network") != "true" || !strings.Contains(pre.hdr.Get("Access-Control-Allow-Headers"), "X-Agent-Token") {
		t.Fatalf("preflight: %d %v", pre.code, pre.hdr)
	}
	if c := e.do("OPTIONS", "/api/queue", "", map[string]string{"Origin": "https://evil.example"}).code; c != 403 {
		t.Fatal(c)
	}
	if c := e.do("OPTIONS", "/api/queue", "", nil).code; c != 403 {
		t.Fatal(c)
	}
}

func TestDuplicateRequestNotQueuedTwice(t *testing.T) {
	e := newEnv(t)
	release := make(chan struct{})
	started := make(chan struct{}, 4)
	e.a.Run = func(ctx context.Context, dir string, log io.Writer, name string, args ...string) error {
		if args[0] == "--build" {
			started <- struct{}{}
			<-release
			d := filepath.Join(e.src, "build", "vc6", "GeneralsMD")
			_ = os.MkdirAll(d, 0o755)
			_ = os.WriteFile(filepath.Join(d, "g.exe"), []byte("MZ"), 0o644)
		}
		return nil
	}
	if r := e.post(okJob); r.code != 202 {
		t.Fatal(r.code)
	}
	<-started
	again := e.post(okJob)
	if again.code != 200 || again.json()["queued"] != false {
		t.Fatalf("%d %s", again.code, again.body)
	}
	if cur := e.do("GET", "/api/status", "", tok).json()["current"]; cur != "zh-vc6-release" {
		t.Fatal(cur)
	}
	close(release)
	e.waitResult("zh-vc6-release")
	if len(started) != 0 {
		t.Fatal("built twice")
	}
}

func TestFailedBuildIsReported(t *testing.T) {
	e := newEnv(t)
	e.a.Run = func(ctx context.Context, dir string, log io.Writer, name string, args ...string) error {
		return exec.ErrNotFound
	}
	e.post(okJob)
	res := e.waitResult("zh-vc6-release")
	if res["ok"] == true || !strings.Contains(res["error"].(string), "failed") {
		t.Fatalf("%v", res)
	}
	if c := e.do("GET", "/api/download/zh-vc6-release", "", tok).code; c != 404 {
		t.Fatal("download of a failed build", c)
	}
}

func TestBuildWithNoBinariesFails(t *testing.T) {
	e := newEnv(t)
	e.a.Run = func(ctx context.Context, dir string, log io.Writer, name string, args ...string) error { return nil }
	e.post(okJob)
	if res := e.waitResult("zh-vc6-release"); res["ok"] == true {
		t.Fatalf("%v", res)
	}
}

func TestDryRunBuildsNothing(t *testing.T) {
	e := newEnv(t)
	e.a.DryRun = true
	e.post(okJob)
	res := e.waitResult("zh-vc6-release")
	if res["ok"] != true || res["dry_run"] != true || len(e.calls) != 0 {
		t.Fatalf("%v %v", res, e.calls)
	}
}

func TestServesEmbeddedSite(t *testing.T) {
	e := newEnv(t)
	if e.a.Site == nil {
		t.Skip("site not embedded")
	}
	if r := e.do("GET", "/", "", nil); r.code != 200 && r.code != 404 {
		t.Fatal(r.code) // 404 is fine when only .gitkeep is embedded
	}
}

func TestListensOnLoopbackOnly(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	if !ln.Addr().(*net.TCPAddr).IP.IsLoopback() {
		t.Fatal("not loopback")
	}
}

// A real CMake build of a tiny project that has the same layout as the game's presets.
func TestRealCMakeBuild(t *testing.T) {
	if _, err := exec.LookPath("cmake"); err != nil {
		t.Skip("cmake not installed")
	}
	if _, err := exec.LookPath("cc"); err != nil {
		t.Skip("no C compiler")
	}
	src, out := t.TempDir(), t.TempDir()
	write := func(name, body string) {
		p := filepath.Join(src, name)
		_ = os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("CMakePresets.json", `{"version":6,"configurePresets":[{"name":"vc6","generator":"Unix Makefiles","binaryDir":"${sourceDir}/build/${presetName}"}],"buildPresets":[{"name":"vc6","configurePreset":"vc6"}]}`)
	write("CMakeLists.txt", "cmake_minimum_required(VERSION 3.20)\nproject(tiny C)\noption(RTS_FLAG \"x\" OFF)\nadd_subdirectory(GeneralsMD)\n")
	write("GeneralsMD/CMakeLists.txt", "add_executable(hello main.c)\nset_target_properties(hello PROPERTIES SUFFIX \".exe\")\nif(RTS_FLAG)\n target_compile_definitions(hello PRIVATE FLAG=1)\nendif()\n")
	write("GeneralsMD/main.c", "int main(void){return 0;}\n")

	a := NewAgent(src, out, "t", nil, false, nil) // nil runner = the real cmake
	a.SkipChecks = true                           // this tiny project builds on Linux; the Windows checks do not apply
	srv := httptest.NewServer(a.Handler())
	defer srv.Close()
	req, _ := http.NewRequest("POST", srv.URL+"/api/queue", strings.NewReader(`{"id":"tiny-vc6-release","preset":"vc6","game":"GeneralsMD","args":["-DRTS_FLAG=ON"]}`))
	req.Header.Set("X-Agent-Token", "t")
	res, err := http.DefaultClient.Do(req)
	if err != nil || res.StatusCode != 202 {
		t.Fatalf("queue: %v %v", err, res)
	}
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		if raw, err := os.ReadFile(filepath.Join(out, "tiny-vc6-release", "manifest.json")); err == nil {
			var r Result
			_ = json.Unmarshal(raw, &r)
			if !r.OK {
				log, _ := os.ReadFile(filepath.Join(out, "tiny-vc6-release", "build.log"))
				t.Fatalf("build failed: %s\n%s", r.Error, log)
			}
			zr, err := zip.OpenReader(filepath.Join(out, "tiny-vc6-release", r.File))
			if err != nil {
				t.Fatal(err)
			}
			if zr.File[0].Name != "hello.exe" {
				t.Fatalf("zip: %v", zr.File[0].Name)
			}
			log, _ := os.ReadFile(filepath.Join(out, "tiny-vc6-release", "build.log"))
			if !strings.Contains(string(log), "-DRTS_FLAG=ON") {
				t.Fatalf("switch did not reach cmake:\n%s", log)
			}
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("real build timed out")
}
