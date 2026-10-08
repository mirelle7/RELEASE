package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeBin makes a directory of tiny executable scripts and returns it, for use as PATH.
func fakeBin(t *testing.T, tools map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, body := range tools {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

const (
	vc6CL     = `echo "Microsoft (R) 32-bit C/C++ Optimizing Compiler Version 12.00.8804 for 80x86" >&2`
	vc6Old    = `echo "Microsoft (R) 32-bit C/C++ Optimizing Compiler Version 12.00.8168 for 80x86" >&2`
	vs2022x86 = `echo "Microsoft (R) C/C++ Optimizing Compiler Version 19.38.33130 for x86" >&2`
	vs2022x64 = `echo "Microsoft (R) C/C++ Optimizing Compiler Version 19.38.33130 for x64" >&2`
)

func goodTools() map[string]string {
	return map[string]string{
		"cmake": `echo "cmake version 3.30.1"`, "ninja": `echo 1.11.1`, "git": `echo "git version 2.43.0"`, "midl": `exit 0`, "cl": vc6CL,
	}
}

// doctorEnv is a test agent that believes it runs on Windows with only the given tools installed.
func doctorEnv(t *testing.T, tools map[string]string, extraEnv ...string) *env {
	t.Helper()
	e := newEnv(t)
	e.a.GOOS = "windows"
	e.a.env = append([]string{"PATH=" + fakeBin(t, tools)}, extraEnv...)
	_ = os.WriteFile(filepath.Join(e.src, "CMakePresets.json"), []byte(`{"version":6,"cmakeMinimumRequired":{"major":3,"minor":28,"patch":0},
	 "configurePresets":[{"name":"base","generator":"Ninja"},{"name":"vc6","inherits":"base"},{"name":"win32","inherits":["base"]},{"name":"mingw-w64-i686","generator":"Ninja"}]}`), 0o644)
	return e
}

func status(r Report, id string) string {
	for _, c := range r.Checks {
		if c.ID == id {
			return c.Status
		}
	}
	return "missing"
}

func TestDoctorReadyWhenEverythingIsSetUp(t *testing.T) {
	e := doctorEnv(t, goodTools())
	r := e.a.Doctor(context.Background(), "vc6")
	if !r.Ready || r.Family != "vc6" || status(r, "ninja") != "ok" { // ninja is needed because vc6 inherits a Ninja generator
		t.Fatalf("%+v", r)
	}
}

func TestDoctorSaysWhatIsMissingAndHowToFixIt(t *testing.T) {
	e := doctorEnv(t, map[string]string{"winget": "exit 0"})
	r := e.a.Doctor(context.Background(), "vc6")
	if r.Ready {
		t.Fatal("empty PATH reported ready")
	}
	for _, id := range []string{"cmake", "ninja", "git", "compiler", "midl"} {
		if status(r, id) != "fail" {
			t.Errorf("%s: %s", id, status(r, id))
		}
	}
	for _, c := range r.Checks {
		if c.Status == "fail" && c.Fix == "" {
			t.Errorf("%s has no fix", c.ID)
		}
	}
	sum := r.Summary()
	if !strings.Contains(sum, "winget install --id Git.Git") || !strings.Contains(sum, "VCVARS32.BAT") {
		t.Fatalf("summary: %s", sum)
	}
}

func TestDoctorWithoutGitIsBlocking(t *testing.T) {
	tools := goodTools()
	delete(tools, "git")
	r := doctorEnv(t, tools).a.Doctor(context.Background(), "vc6")
	if r.Ready || status(r, "git") != "fail" {
		t.Fatalf("%+v", r)
	}
}

func TestDoctorChecksTheCompilerFamily(t *testing.T) {
	cases := []struct {
		name, preset, cl string
		ready            bool
		mention          string
	}{
		{"modern compiler for a VC6 build", "vc6", vs2022x86, false, "needs Visual C++ 6"},
		{"VC6 compiler for a modern build", "win32", vc6CL, false, "needs a modern"},
		{"64-bit compiler for the 32-bit game", "win32", vs2022x64, false, "32-bit"},
		{"right modern compiler", "win32", vs2022x86, true, ""},
		{"VC6 before SP6 still builds, with a warning", "vc6", vc6Old, true, ""},
	}
	for _, c := range cases {
		tools := goodTools()
		tools["cl"] = c.cl
		r := doctorEnv(t, tools).a.Doctor(context.Background(), c.preset)
		if r.Ready != c.ready || (c.mention != "" && !strings.Contains(r.Summary(), c.mention)) {
			t.Errorf("%s: ready=%v %s", c.name, r.Ready, r.Summary())
		}
	}
	tools := goodTools()
	tools["cl"] = vc6Old
	if r := doctorEnv(t, tools).a.Doctor(context.Background(), "vc6"); status(r, "compiler") != "warn" {
		t.Error("an older VC6 build should only warn")
	}
}

// The failure that prompted all this: Clang was picked because no Microsoft compiler was set up.
func TestDoctorCatchesAnotherCompilerBeingPicked(t *testing.T) {
	r := doctorEnv(t, goodTools(), `CC=C:\Program Files\LLVM\bin\clang.exe`).a.Doctor(context.Background(), "vc6")
	if r.Ready || status(r, "compiler-env") != "fail" || !strings.Contains(r.Summary(), "clang.exe") {
		t.Fatalf("%+v", r)
	}
	if r := doctorEnv(t, goodTools(), `CXX=C:\VC6\bin\cl.exe`).a.Doctor(context.Background(), "vc6"); !r.Ready {
		t.Fatalf("a cl.exe override is fine: %s", r.Summary())
	}
}

func TestDoctorCMakeTooOld(t *testing.T) {
	tools := goodTools()
	tools["cmake"] = `echo "cmake version 3.20.2"`
	r := doctorEnv(t, tools).a.Doctor(context.Background(), "vc6")
	if r.Ready || !strings.Contains(r.Summary(), "too old") || !strings.Contains(r.Summary(), "3.28") {
		t.Fatalf("%s", r.Summary())
	}
}

func TestDoctorNotWindows(t *testing.T) {
	e := doctorEnv(t, goodTools())
	e.a.GOOS = "linux"
	r := e.a.Doctor(context.Background(), "vc6")
	if r.Ready || status(r, "os") != "fail" || !strings.Contains(r.Summary(), "MinGW") {
		t.Fatalf("%s", r.Summary())
	}
	if len(r.Checks) != 1 {
		t.Errorf("only the OS check should be reported, got %d checks", len(r.Checks))
	}
}

func TestSkipChecksLetsAnythingThrough(t *testing.T) {
	e := doctorEnv(t, nil)
	e.a.SkipChecks = true
	if r := e.a.Doctor(context.Background(), "vc6"); !r.Ready || !r.Skipped || status(r, "git") != "fail" {
		t.Fatalf("%+v", r)
	}
}

func TestRealBuildIsRefusedBeforeCMakeRuns(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "cmake-calls")
	tools := goodTools()
	tools["cmake"] = `echo "$@" >> ` + marker + `; echo "cmake version 3.30.1"` // records every call
	delete(tools, "cl")                                                         // no compiler set up: the exact situation in the bug report
	e := doctorEnv(t, tools)
	e.a.Run = nil // the real runner
	e.post(`{"id":"zh-vc6-release","preset":"vc6","game":"GeneralsMD","args":[]}`)
	res := e.waitResult("zh-vc6-release")
	if res["ok"] == true || !strings.Contains(res["error"].(string), "cl.exe") || !strings.Contains(res["error"].(string), "VCVARS32.BAT") {
		t.Fatalf("%v", res)
	}
	calls, _ := os.ReadFile(marker)
	if strings.Contains(string(calls), "--preset") || strings.Contains(string(calls), "--build") {
		t.Fatalf("cmake was configured or built even though the PC is not ready:\n%s", calls)
	}
}

// The log from a real failing run: Clang instead of Microsoft's compiler, then no midl.exe.
const clangLog = `$ cmake --preset vc6 -DRTS_BUILD_GENERALS=ON -DRTS_BUILD_ZEROHOUR=ON
-- The C compiler identification is Clang 22.1.8 with GNU-like command-line
-- The CXX compiler identification is Clang 22.1.8 with GNU-like command-line
-- Check for working C compiler: C:/Program Files/LLVM/bin/clang.exe - skipped
-- CMAKE_CXX_COMPILER_ID: Clang
  Retail compatibility: Clang 22.1.8 is not CRC-compatible with retail.
Cloning into 'CompLibHeader'...
CMake Error at Core/Libraries/Source/EABrowserDispatch/CMakeLists.txt:19 (message):
  EABrowserDispatch requires an IDL compiler for Windows builds:
    - For MSVC: midl.exe should be in PATH
-- Configuring incomplete, errors occurred!`

func TestExplainsTheRealFailureInPlainWords(t *testing.T) {
	hints := strings.Join(explainFailure("vc6", clangLog), " ")
	for _, want := range []string{"Clang", "developer prompt", "midl.exe", "--env-script"} {
		if !strings.Contains(hints, want) {
			t.Errorf("missing %q in: %s", want, hints)
		}
	}
	if len(explainFailure("mingw", clangLog)) == 0 || strings.Contains(strings.Join(explainFailure("mingw", clangLog), " "), "Clang instead of Microsoft") {
		t.Error("the Clang hint is about Microsoft builds only")
	}
}

func TestExplainsGitNinjaAndNetworkProblems(t *testing.T) {
	cases := map[string]string{
		"CMake Error: CMake was unable to find a build program corresponding to \"Ninja\"": "Ninja",
		"error: could not find git for clone of lzhl-populate":                             "Git",
		"fatal: unable to access 'https://github.com/x/y.git/': Could not resolve host":    "internet",
		"No CMAKE_CXX_COMPILER could be found.":                                            "compiler",
	}
	for log, want := range cases {
		if h := strings.Join(explainFailure("vc6", log), " "); !strings.Contains(h, want) {
			t.Errorf("%q -> %s", log, h)
		}
	}
	if h := explainFailure("vc6", "everything is fine"); len(h) != 0 {
		t.Errorf("invented a hint: %v", h)
	}
}

func TestWhichAndEnvHelpers(t *testing.T) {
	dir := fakeBin(t, map[string]string{"tool": "exit 0"})
	_ = os.WriteFile(filepath.Join(dir, "notexec"), []byte("x"), 0o644)
	env := []string{"A=1", "PATH=" + dir, "A=2"}
	if which(env, "tool") == "" || which(env, "notexec") != "" || which(env, "nope") != "" || which(nil, "tool") != "" {
		t.Error("which")
	}
	if envGet(env, "A") != "2" {
		t.Error("the last value wins")
	}
	got := parseSetOutput("PATH=C:\\x\r\nFOO=bar=baz\r\n=C:=C:\\\r\nnoequals\r\n")
	if len(got) != 2 || got[1] != "FOO=bar=baz" {
		t.Errorf("%v", got)
	}
	if _, err := loadEnvScript("x.bat", ""); err == nil && os.PathSeparator != '\\' {
		t.Error("--env-script must refuse to run off Windows")
	}
}

func TestDoctorEndpoint(t *testing.T) {
	e := doctorEnv(t, goodTools())
	if c := e.do("GET", "/api/doctor?preset=vc6", "", nil).code; c != 401 {
		t.Fatal(c)
	}
	if c := e.do("GET", "/api/doctor?preset=vc6;calc", "", tok).code; c != 400 {
		t.Fatal(c)
	}
	r := e.do("GET", "/api/doctor?preset=vc6", "", tok)
	if r.code != 200 || r.json()["ready"] != true || r.json()["family"] != "vc6" {
		t.Fatalf("%d %s", r.code, r.body)
	}
}
