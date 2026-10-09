package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const projPresets = `{"version":6,"cmakeMinimumRequired":{"major":3,"minor":28,"patch":0},"configurePresets":[{"name":"unix","generator":"Ninja"},{"name":"macos"},{"name":"vc6"}]}`

func projectFolder(t *testing.T, presets string) string {
	t.Helper()
	d := t.TempDir()
	if err := os.WriteFile(filepath.Join(d, "CMakePresets.json"), []byte(presets), 0o644); err != nil {
		t.Fatal(err)
	}
	return d
}

// dirRunner fakes cmake: the build writes two game executables and a tool into <dir>/build/<preset>/GeneralsMD,
// each containing the folder it was built in.
func dirRunner(dirs *dirLog) Runner {
	return func(ctx context.Context, dir string, log io.Writer, name string, args ...string) error {
		if args[0] != "--build" {
			return nil
		}
		dirs.add(dir)
		d := filepath.Join(dir, "build", args[2], "GeneralsMD")
		_ = os.MkdirAll(d, 0o755)
		for _, n := range []string{"generalszh.exe", "generalsv.exe", "WorldBuilder.exe", "generalszh.pdb"} {
			_ = os.WriteFile(filepath.Join(d, n), []byte("bin:"+dir+":"+n), 0o644)
		}
		return nil
	}
}

func TestFamilyOfNativePresets(t *testing.T) {
	for p, want := range map[string]string{"unix": "linux", "unix-release": "linux", "linux-x64": "linux", "macos": "macos", "macos-arm64": "macos",
		"vc6": "vc6", "win32": "msvc", "mingw-w64-i686": "mingw", "other": "other"} {
		if got := familyOf(p); got != want {
			t.Errorf("familyOf(%q) = %q, want %q", p, got, want)
		}
	}
}

func TestSrcForAndOldConfigStillLoads(t *testing.T) {
	a := &Agent{Src: "/ggc", Srcs: map[string]string{}}
	a.setSrc("generalsx", "/gx")
	if a.srcFor("") != "/ggc" || a.srcFor("ggc") != "/ggc" || a.srcFor("generalsx") != "/gx" || a.srcFor("nope") != "" {
		t.Fatal("srcFor")
	}
	a.setSrc("generalsx", "")
	if a.srcFor("generalsx") != "" {
		t.Fatal("clearing a project")
	}
	old := parseConfig([]byte(`{"Src":"C:\\g","Data":"C:\\d"}`))
	if old.Src != `C:\g` || old.Data != `C:\d` || len(old.Projects) != 0 {
		t.Fatalf("%+v", old)
	}
	// saving writes Projects and keeps ggc in Src
	a.ConfigPath = filepath.Join(t.TempDir(), "agent-config.json")
	a.setSrc("bobtista", "/bt")
	a.saveConfig()
	raw, _ := os.ReadFile(a.ConfigPath)
	c := parseConfig(raw)
	if c.Src != "/ggc" || c.Projects["bobtista"] != "/bt" || len(c.Projects) != 1 {
		t.Fatalf("%s", raw)
	}
}

func TestProjectsEndpoint(t *testing.T) {
	e := newEnv(t)
	r := e.do("GET", "/api/projects", "", nil)
	if r.code != 401 {
		t.Fatal(r.code)
	}
	list := func() map[string]map[string]any {
		out := map[string]map[string]any{}
		for _, p := range e.do("GET", "/api/projects", "", tok).json()["projects"].([]any) {
			m := p.(map[string]any)
			out[m["id"].(string)] = m
		}
		return out
	}
	l := list()
	for _, id := range []string{"ggc", "generalsx", "bobtista", "generalsonline"} {
		if l[id] == nil {
			t.Fatalf("missing %s: %v", id, l)
		}
	}
	if l["ggc"]["exists"] != true || l["ggc"]["has_presets"] != true || l["generalsx"]["dir"] != "" || l["generalsx"]["exists"] != false {
		t.Fatalf("%v", l)
	}
	good := projectFolder(t, projPresets)
	for _, bad := range []string{
		`{"id":"Bad Id","dir":"` + "/x" + `"}`,
		`{"id":"generalsx","dir":"relative/path"}`,
		fmt.Sprintf(`{"id":"generalsx","dir":%q}`, filepath.Join(good, "missing")),
		fmt.Sprintf(`{"id":"generalsx","dir":%q}`, t.TempDir()), // exists, no presets
		`not json`,
	} {
		if c := e.do("POST", "/api/projects", bad, tok).code; c != 400 {
			t.Errorf("%s -> %d", bad, c)
		}
	}
	e.a.ConfigPath = filepath.Join(t.TempDir(), "cfg.json")
	if c := e.do("POST", "/api/projects", fmt.Sprintf(`{"id":"generalsx","dir":%q}`, good), tok).code; c != 200 {
		t.Fatal(c)
	}
	gx := list()["generalsx"]
	if gx["exists"] != true || gx["has_presets"] != true || len(gx["presets"].([]any)) != 3 {
		t.Fatalf("%v", gx)
	}
	if raw, _ := os.ReadFile(e.a.ConfigPath); parseConfig(raw).Projects["generalsx"] != good {
		t.Fatalf("not persisted: %s", raw)
	}
	e.do("POST", "/api/projects", `{"id":"generalsx","dir":""}`, tok)
	if list()["generalsx"]["dir"] != "" {
		t.Fatal("not cleared")
	}
}

func TestJobBuildsInItsProjectsFolderAndServesThePlainExe(t *testing.T) {
	e := newEnv(t)
	dirs := &dirLog{}
	e.a.Run = dirRunner(dirs)
	gx := projectFolder(t, projPresets)
	e.a.setSrc("generalsx", gx)

	if c := e.post(`{"id":"gx-vc6","project":"mystery","preset":"vc6","game":"GeneralsMD","args":[]}`).code; c != 400 {
		t.Fatalf("unconfigured project accepted: %d", c)
	}
	if r := e.post(`{"id":"gx-vc6","project":"Bad!","preset":"vc6","game":"GeneralsMD","args":[]}`); r.code != 400 {
		t.Fatal(r.code)
	}
	if r := e.post(`{"id":"gx-vc6","project":"generalsx","preset":"win32","game":"GeneralsMD","args":[]}`); r.code != 400 || !strings.Contains(string(r.body), "generalsx") {
		t.Fatalf("preset is checked in the project's own presets: %d %s", r.code, r.body)
	}
	if c := e.post(`{"id":"generalsx-zh-vc6","project":"generalsx","preset":"vc6","game":"GeneralsMD","args":[]}`).code; c != 202 {
		t.Fatal(c)
	}
	res := e.waitResult("generalsx-zh-vc6")
	if res["ok"] != true || res["project"] != "generalsx" {
		t.Fatalf("%v", res)
	}
	if got := dirs.all(); len(got) != 1 || got[0] != gx {
		t.Fatalf("built in %v, want %s", got, gx)
	}
	exes := res["exes"].([]any)
	if len(exes) != 2 {
		t.Fatalf("exes %v", exes)
	}
	for _, x := range exes {
		if n := x.(map[string]any)["name"].(string); strings.EqualFold(n, "WorldBuilder.exe") || x.(map[string]any)["size"].(float64) == 0 {
			t.Fatalf("bad exe %v", x)
		}
	}
	if _, err := os.Stat(filepath.Join(e.out, "generalsx-zh-vc6", "generalszh.exe")); err != nil {
		t.Fatal("no plain copy next to the zip")
	}
	ticket := func() string { return e.do("POST", "/api/ticket", "", tok).json()["ticket"].(string) }
	d := e.do("GET", "/api/download/generalsx-zh-vc6?exe=generalszh.exe&ticket="+ticket(), "", nil)
	if d.code != 200 || !strings.HasPrefix(string(d.body), "bin:"+gx) || d.hdr.Get("Content-Disposition") != `attachment; filename="generalszh.exe"` || d.hdr.Get("Content-Type") != "application/octet-stream" {
		t.Fatalf("%d %v %q", d.code, d.hdr, d.body)
	}
	if c := e.do("GET", "/api/download/generalsx-zh-vc6?exe=WorldBuilder.exe", "", tok).code; c != 404 {
		t.Errorf("unlisted exe: %d", c)
	}
	for _, bad := range []string{"../x", "..%2Fmanifest.json", "a/b", "manifest.json"} {
		if c := e.do("GET", "/api/download/generalsx-zh-vc6?exe="+bad, "", tok).code; c != 404 {
			t.Errorf("%q: %d", bad, c)
		}
	}
	if c := e.do("GET", "/api/download/generalsx-zh-vc6?exe=generalszh.exe", "", nil).code; c != 401 {
		t.Errorf("no token: %d", c)
	}
	if d := e.do("GET", "/api/download/generalsx-zh-vc6", "", tok); d.code != 200 {
		t.Errorf("zip: %d", d.code)
	}
}

func TestMissingProjectFolderFailsTheJobWithAnExplanation(t *testing.T) {
	e := newEnv(t)
	e.a.Run = dirRunner(&dirLog{})
	gx := projectFolder(t, projPresets)
	e.a.setSrc("generalsx", gx)
	// the folder disappears after the job was accepted: the validation is bypassed by queuing directly
	_ = os.Remove(filepath.Join(gx, "CMakePresets.json"))
	e.a.jobs <- queued{Job: Job{ID: "gone", Project: "generalsx", Preset: "vc6", Game: "GeneralsMD"}}
	res := e.waitResult("gone")
	msg, _ := res["error"].(string)
	if res["ok"] == true || !strings.Contains(msg, `"generalsx"`) || !strings.Contains(msg, "-project generalsx=") {
		t.Fatalf("%v", res)
	}
}

func TestDoctorAndPlatformsUseTheProjectsPresets(t *testing.T) {
	e := doctorEnv(t, goodTools())
	// ggc has no unix preset; the project does, with a higher CMake minimum than the installed 3.30.1
	px := projectFolder(t, `{"version":6,"cmakeMinimumRequired":{"major":3,"minor":40,"patch":0},"configurePresets":[{"name":"unix-release","generator":"Ninja"}]}`)
	e.a.setSrc("generalsx", px)
	e.a.GOOS = "linux"
	tools := goodTools()
	tools["g++"] = "exit 0"
	e.a.env = []string{"PATH=" + fakeBin(t, tools)}
	r := e.a.DoctorProj(context.Background(), "unix-release", "", "generalsx")
	if r.Family != "linux" || r.Ready || status(r, "cmake") != "fail" || !strings.Contains(fmt.Sprint(r.Checks), "3.40.0") {
		t.Fatalf("%+v", r)
	}
	if r := e.a.DoctorProj(context.Background(), "unix", "", ""); !r.Ready || status(r, "compiler") != "ok" { // ggc minimum is 3.28
		t.Fatalf("%+v", r)
	}
	var pl Platform
	for _, p := range e.a.PlatformsFor(context.Background(), "generalsx") {
		if p.ID == "linux" {
			pl = p
		}
	}
	if pl.Preset != "unix-release" || pl.Name != "Linux (native)" || pl.Ready {
		t.Fatalf("%+v", pl)
	}
	rr := e.do("GET", "/api/platforms?project=generalsx", "", tok)
	if rr.code != 200 || !strings.Contains(string(rr.body), "unix-release") {
		t.Fatalf("%d %s", rr.code, rr.body)
	}
	if rr := e.do("GET", "/api/doctor?preset=unix-release&project=generalsx", "", tok).json(); rr["family"] != "linux" || rr["ready"] != false {
		t.Fatalf("%v", rr)
	}
	if c := e.do("GET", "/api/doctor?preset=unix&project=BAD!", "", tok).code; c != 400 {
		t.Fatal(c)
	}
}

func TestNativePlatformsSayWhatOSTheyNeed(t *testing.T) {
	e := doctorEnv(t, goodTools())
	e.a.GOOS = "windows"
	for preset, want := range map[string]string{"unix": "Needs Linux", "macos": "Needs a Mac"} {
		r := e.a.Doctor(context.Background(), preset)
		if r.Ready || !strings.Contains(r.Summary(), want) {
			t.Errorf("%s: %s", preset, r.Summary())
		}
	}
	e.a.GOOS = "linux"
	if r := e.a.Doctor(context.Background(), "vc6"); r.Ready || !strings.Contains(r.Summary(), "Needs Windows") {
		t.Errorf("vc6 on linux: %s", r.Summary())
	}
	e.a.GOOS = "darwin"
	tools := goodTools()
	tools["clang"] = "exit 0"
	tools["xcode-select"] = "echo /Library/Developer/CommandLineTools"
	e.a.env = []string{"PATH=" + fakeBin(t, tools)}
	if r := e.a.Doctor(context.Background(), "macos"); !r.Ready {
		t.Errorf("mac: %s", r.Summary())
	}
	tools["xcode-select"] = "exit 2"
	e.a.env = []string{"PATH=" + fakeBin(t, tools)}
	if r := e.a.Doctor(context.Background(), "macos"); r.Ready {
		t.Error("mac without the command line tools must not be ready")
	}
}

func TestCollectFindsUnixOutputs(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("needs unix file modes")
	}
	src := t.TempDir()
	d := filepath.Join(src, "build", "unix", "GeneralsMD")
	_ = os.MkdirAll(d, 0o755)
	_ = os.WriteFile(filepath.Join(d, "generalszh"), []byte("elf"), 0o755)
	_ = os.WriteFile(filepath.Join(d, "libfoo.so"), []byte("so"), 0o644)
	_ = os.WriteFile(filepath.Join(d, "libbar.dylib"), []byte("dy"), 0o644)
	_ = os.WriteFile(filepath.Join(d, "Makefile"), []byte("m"), 0o644)
	_ = os.WriteFile(filepath.Join(d, "cmake_install.cmake"), []byte("m"), 0o755)
	got := collect(src, "unix", "GeneralsMD")
	names := []string{}
	for _, g := range got {
		names = append(names, filepath.Base(g))
	}
	if strings.Join(names, ",") != "generalszh,libbar.dylib,libfoo.so" {
		t.Fatalf("%v", names)
	}
	if ex := mainExes(got); len(ex) != 1 || filepath.Base(ex[0]) != "generalszh" {
		t.Fatalf("%v", ex)
	}
}

func TestRefValidation(t *testing.T) {
	e := newEnv(t)
	for ref, want := range map[string]int{"latest": 202, "abcdef1": 202, "ABCDEF1": 400, "abc": 400, "main": 400, "abcdef1; rm -rf": 400, "../x": 400} {
		id := fmt.Sprintf("ref-%d", len(ref)) + strings.Map(func(r rune) rune {
			if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
				return r
			}
			return -1
		}, ref)
		j := fmt.Sprintf(`{"id":%q,"preset":"vc6","game":"GeneralsMD","args":[],"ref":%q}`, id, ref)
		if c := e.post(j).code; c != want {
			t.Errorf("ref %q: %d, want %d", ref, c, want)
		}
	}
}

// refEnv is a checkout with a local bare origin; pushV4 adds a new commit to the origin that the checkout does not have yet.
func refEnv(t *testing.T) (e *env, shas []string, dirs *dirLog, pushV4 func() string) {
	checkout, shas := history(t)
	e = newEnv(t)
	e.a.setSrc("generalsx", checkout)
	dirs = &dirLog{}
	e.a.Run = versionRunner(dirs, nil)
	origin := git(t, checkout, "remote", "get-url", "origin")
	pushV4 = func() string {
		other := filepath.Join(t.TempDir(), "other")
		git(t, filepath.Dir(other), "clone", "-q", origin, other)
		_ = os.WriteFile(filepath.Join(other, "VERSION"), []byte("v4"), 0o644)
		git(t, other, "commit", "-q", "-am", "v4")
		git(t, other, "push", "-q", "origin", "HEAD:main")
		return git(t, other, "rev-parse", "HEAD")
	}
	return
}

func TestBuildASpecificCommitInAWorktree(t *testing.T) {
	e, shas, dirs, _ := refEnv(t)
	checkout := e.a.srcFor("generalsx")
	before := git(t, checkout, "status", "--porcelain")
	j := fmt.Sprintf(`{"id":"gx-c%s","project":"generalsx","preset":"vc6","game":"GeneralsMD","args":[],"ref":%q}`, shas[0][:7], shas[0][:7])
	if c := e.post(j).code; c != 202 {
		t.Fatal(c)
	}
	res := e.waitResult("gx-c" + shas[0][:7])
	if res["ok"] != true || res["sha"] != shas[0] || res["label"] != shas[0][:7] {
		t.Fatalf("%v", res)
	}
	if got := exeContent(t, e.out, "gx-c"+shas[0][:7]); got != "v1" {
		t.Fatalf("built %q, want v1", got)
	}
	if d := dirs.all(); len(d) != 1 || d[0] == checkout || !strings.Contains(d[0], "worktrees") {
		t.Fatalf("built in %v", d)
	}
	if _, err := os.Stat(filepath.Join(e.out, "worktrees", "gx-c"+shas[0][:7])); err == nil {
		t.Error("worktree not removed")
	}
	if git(t, checkout, "status", "--porcelain") != before || git(t, checkout, "rev-parse", "HEAD") != shas[2] {
		t.Error("the user's checkout was touched")
	}
	if wl := git(t, checkout, "worktree", "list"); strings.Count(wl, "\n") != 0 {
		t.Errorf("worktrees left behind: %s", wl)
	}
}

func TestBuildLatestFetchesTheDefaultBranch(t *testing.T) {
	e, shas, _, pushV4 := refEnv(t)
	v4 := pushV4()
	if c := e.post(`{"id":"gx-latest","project":"generalsx","preset":"vc6","game":"GeneralsMD","args":[],"ref":"latest"}`).code; c != 202 {
		t.Fatal(c)
	}
	res := e.waitResult("gx-latest")
	if res["ok"] != true || res["sha"] != v4 || res["label"] != "latest main" {
		t.Fatalf("%v", res)
	}
	if exeContent(t, e.out, "gx-latest") != "v4" {
		t.Fatal("did not build the new commit")
	}
	if git(t, e.a.srcFor("generalsx"), "rev-parse", "HEAD") != shas[2] {
		t.Error("checkout moved")
	}
	// a commit that exists only on the origin is fetched on demand
	v5 := pushV4Again(t, e.a.srcFor("generalsx"))
	j := fmt.Sprintf(`{"id":"gx-c5","project":"generalsx","preset":"vc6","game":"GeneralsMD","args":[],"ref":%q}`, v5[:7])
	e.post(j)
	if res := e.waitResult("gx-c5"); res["ok"] != true || res["sha"] != v5 || exeContent(t, e.out, "gx-c5") != "v5" {
		t.Fatalf("%v", res)
	}
}

func pushV4Again(t *testing.T, checkout string) string {
	origin := git(t, checkout, "remote", "get-url", "origin")
	other := filepath.Join(t.TempDir(), "other5")
	git(t, filepath.Dir(other), "clone", "-q", origin, other)
	_ = os.WriteFile(filepath.Join(other, "VERSION"), []byte("v5"), 0o644)
	git(t, other, "commit", "-q", "-am", "v5")
	git(t, other, "push", "-q", "origin", "HEAD:main")
	return git(t, other, "rev-parse", "HEAD")
}

func TestUnknownCommitFailsClearly(t *testing.T) {
	e, _, dirs, _ := refEnv(t)
	e.post(`{"id":"gx-bad","project":"generalsx","preset":"vc6","game":"GeneralsMD","args":[],"ref":"0123456789abcdef"}`)
	res := e.waitResult("gx-bad")
	msg, _ := res["error"].(string)
	if res["ok"] == true || !strings.Contains(msg, "does not exist") || len(dirs.all()) != 0 {
		t.Fatalf("%v", res)
	}
}

func TestCommitsEndpoint(t *testing.T) {
	e, shas, _, pushV4 := refEnv(t)
	get := func(q string) map[string]any { return e.do("GET", "/api/commits?project=generalsx"+q, "", tok).json() }
	r := get("")
	cs := r["commits"].([]any)
	if r["branch"] != "main" || r["head"] != shas[2] || r["fetched"] != false || len(cs) != 3 {
		t.Fatalf("%v", r)
	}
	first := cs[0].(map[string]any)
	if first["sha"] != shas[2] || first["short"] != shas[2][:7] || first["subject"] != "v3" || first["author"] != "t" || first["date"] == "" {
		t.Fatalf("%v", first)
	}
	if n := len(get("&limit=2")["commits"].([]any)); n != 2 {
		t.Fatalf("limit 2 gave %d", n)
	}
	v4 := pushV4()
	if r := get(""); r["head"] != shas[2] {
		t.Fatal("seen a commit that was not fetched")
	}
	r = get("&fetch=1")
	if r["fetched"] != true || r["head"] != v4 || len(r["commits"].([]any)) != 4 {
		t.Fatalf("%v", r)
	}
	// not a git checkout, and an unset project: 200 with an error and no commits
	plain := projectFolder(t, projPresets)
	e.a.setSrc("bobtista", plain)
	for _, p := range []string{"bobtista", "mystery"} {
		r := e.do("GET", "/api/commits?project="+p, "", tok)
		m := r.json()
		if r.code != 200 || m["error"] == nil || len(m["commits"].([]any)) != 0 {
			t.Errorf("%s: %d %s", p, r.code, r.body)
		}
	}
	if c := e.do("GET", "/api/commits?project=BAD!", "", tok).code; c != 400 {
		t.Error(c)
	}
	// fetch failure is not an error
	git(t, e.a.srcFor("generalsx"), "remote", "set-url", "origin", filepath.Join(t.TempDir(), "nowhere.git"))
	if r := get("&fetch=1"); r["fetched"] != false || r["error"] != nil || len(r["commits"].([]any)) == 0 {
		t.Fatalf("%v", r)
	}
}

func TestDependencyDownloadIsRetriedButCompileErrorsAreNot(t *testing.T) {
	old := retryDelays
	retryDelays = []time.Duration{time.Millisecond, time.Millisecond}
	defer func() { retryDelays = old }()

	e := newEnv(t)
	var calls atomic.Int32
	e.a.Run = func(ctx context.Context, dir string, log io.Writer, name string, args ...string) error {
		if args[0] == "--build" {
			d := filepath.Join(dir, "build", args[2], "GeneralsMD")
			_ = os.MkdirAll(d, 0o755)
			return os.WriteFile(filepath.Join(d, "generalszh.exe"), []byte("MZ"), 0o644)
		}
		if calls.Add(1) == 1 {
			fmt.Fprintln(log, "fatal: could not read Username for 'https://github.com': terminal prompts disabled")
			return fmt.Errorf("exit status 1")
		}
		return nil
	}
	e.post(okJob)
	res := e.waitResult("zh-vc6-release")
	if res["ok"] != true || calls.Load() != 2 {
		t.Fatalf("%v calls=%d", res, calls.Load())
	}
	logb, _ := os.ReadFile(filepath.Join(e.out, "zh-vc6-release", "build.log"))
	if !strings.Contains(string(logb), "the dependency download failed (network); retrying (2 of 3)") {
		t.Fatalf("%s", logb)
	}

	// always failing network: three attempts, message mentions it
	calls.Store(0)
	e.a.Run = func(ctx context.Context, dir string, log io.Writer, name string, args ...string) error {
		calls.Add(1)
		fmt.Fprintln(log, "fatal: unable to access: Could not resolve host: github.com")
		return fmt.Errorf("exit status 1")
	}
	e.post(strings.Replace(okJob, "zh-vc6-release", "net-fail", 1))
	res = e.waitResult("net-fail")
	if res["ok"] == true || calls.Load() != 3 || !strings.Contains(res["error"].(string), "failed 3 times") {
		t.Fatalf("%v calls=%d", res, calls.Load())
	}

	// compile error: no retry
	calls.Store(0)
	e.a.Run = func(ctx context.Context, dir string, log io.Writer, name string, args ...string) error {
		calls.Add(1)
		fmt.Fprintln(log, "foo.cpp:3: error: expected ';'")
		return fmt.Errorf("exit status 1")
	}
	e.post(strings.Replace(okJob, "zh-vc6-release", "cc-fail", 1))
	res = e.waitResult("cc-fail")
	if res["ok"] == true || calls.Load() != 1 || strings.Contains(res["error"].(string), "failed 3 times") {
		t.Fatalf("%v calls=%d", res, calls.Load())
	}
}
