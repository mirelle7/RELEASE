package main

import (
	"context"
	"encoding/json"
	"io"
	"errors"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPlatformsListsEveryCompilerWithReadiness(t *testing.T) {
	e := doctorEnv(t, goodTools())
	e.a.SkipChecks = true // every platform reports ready; the point is the list, not the probes
	ps := e.a.Platforms(context.Background())
	want := []string{"vc6", "msvc", "mingw-clang", "mingw-gcc", "linux", "macos"}
	if len(ps) != len(want) {
		t.Fatalf("%+v", ps)
	}
	for i, p := range ps {
		if p.ID != want[i] || !p.Ready || p.Report == nil || p.Report.Preset != p.Preset {
			t.Errorf("%d: %+v", i, p)
		}
	}
}

func TestPlatformsExplainWhatIsMissing(t *testing.T) {
	tools := goodTools()
	delete(tools, "cl")
	e := doctorEnv(t, tools)
	e.a.Native = true
	for _, p := range e.a.Platforms(context.Background()) {
		if p.ID == "msvc" && !p.Ready && p.Problem == "" {
			t.Errorf("a platform that is not ready must say why: %+v", p)
		}
	}
}

func TestLogEndpointReturnsTheTailAndLetsTheReaderContinue(t *testing.T) {
	e := doctorEnv(t, goodTools())
	dir := filepath.Join(e.a.Out, "zh-vc6-release")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "build.log"), []byte("one\ntwo\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	get := func(q string) map[string]any {
		w := httptest.NewRecorder()
		e.a.handleLog(w, httptest.NewRequest("GET", "/api/log?"+q, nil))
		var m map[string]any
		_ = json.Unmarshal(w.Body.Bytes(), &m)
		m["code"] = float64(w.Code)
		return m
	}
	if m := get("id=zh-vc6-release&from=-1"); m["text"] != "one\ntwo\n" || m["next"] != float64(8) {
		t.Fatalf("%v", m)
	}
	if m := get("id=zh-vc6-release&from=4"); m["text"] != "two\n" {
		t.Fatalf("%v", m)
	}
	if m := get("id=../etc"); m["code"] != float64(400) {
		t.Fatalf("path tricks must be refused: %v", m)
	}
	if m := get("id=nope"); m["code"] != float64(404) {
		t.Fatalf("%v", m)
	}
}

func TestToolchainPicksTheImageAndRejectsUnknownCompilers(t *testing.T) {
	e := doctorEnv(t, goodTools())
	if spec, _ := e.a.mingwImageFor("gcc"); spec.Name != defaultImage {
		t.Errorf("gcc must use the gcc image, got %q", spec.Name)
	}
	if spec, _ := e.a.mingwImageFor("clang"); spec.Name != clangImage.Name {
		t.Errorf("clang must use the clang image, got %q", spec.Name)
	}
	if err := e.a.Validate(Job{ID: "x", Preset: "mingw-w64-i686", Game: "Generals", Toolchain: "tcc"}); err == nil {
		t.Error("an unknown toolchain must be refused")
	}
}

func TestBuildFolderIsClearedWhenTheCompilerChanges(t *testing.T) {
	src := t.TempDir()
	if err := prepareBuildDir(src, "mingw-w64-i686", "clang"); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(src, "build", "mingw-w64-i686", "CMakeCache.txt")
	if err := os.WriteFile(marker, []byte("cache"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := prepareBuildDir(src, "mingw-w64-i686", "clang"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Error("the same compiler must keep its build folder (that is what makes repeat builds fast)")
	}
	if err := prepareBuildDir(src, "mingw-w64-i686", "gcc"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Error("a different compiler must start from a clean build folder")
	}
}

func TestSessionTokenIsOnlyGivenToThePageTheAgentServes(t *testing.T) {
	e := doctorEnv(t, goodTools())
	e.a.Token = "secret-token"
	ask := func(headers map[string]string) int {
		r := httptest.NewRequest("GET", "http://127.0.0.1:8787/api/session", nil)
		for k, v := range headers {
			r.Header.Set(k, v)
		}
		w := httptest.NewRecorder()
		e.a.handleSession(w, r)
		if w.Code == 200 && !strings.Contains(w.Body.String(), "secret-token") {
			t.Errorf("a 200 must carry the token: %s", w.Body.String())
		}
		if w.Code != 200 && strings.Contains(w.Body.String(), "secret-token") {
			t.Errorf("a refusal must not leak the token: %s", w.Body.String())
		}
		return w.Code
	}
	if c := ask(nil); c != 200 {
		t.Errorf("the page's own request (no Origin) must get it, got %d", c)
	}
	if c := ask(map[string]string{"Sec-Fetch-Site": "same-origin", "Origin": "http://127.0.0.1:8787"}); c != 200 {
		t.Errorf("same-origin must get it, got %d", c)
	}
	if c := ask(map[string]string{"Sec-Fetch-Site": "cross-site", "Origin": "https://evil.example"}); c != 403 {
		t.Errorf("another site must be refused, got %d", c)
	}
	if c := ask(map[string]string{"Origin": "https://you.github.io"}); c != 403 {
		t.Errorf("an allowed-origin page is still not this agent's own page, got %d", c)
	}
}

func TestDownloadTicketWorksOnceAndTheTokenStaysOutOfTheUrl(t *testing.T) {
	e := doctorEnv(t, goodTools())
	e.a.Token = "secret-token"
	dir := filepath.Join(e.a.Out, "zh-vc6-release")
	_ = os.MkdirAll(dir, 0o755)
	_ = os.WriteFile(filepath.Join(dir, "zh-vc6-release.zip"), []byte("PK-zip"), 0o644)

	w := httptest.NewRecorder()
	e.a.handleTicket(w, httptest.NewRequest("POST", "/api/ticket?id=zh-vc6-release", nil))
	var tk map[string]string
	_ = json.Unmarshal(w.Body.Bytes(), &tk)
	if w.Code != 200 || len(tk["ticket"]) < 16 || strings.Contains(w.Body.String(), "secret-token") {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	get := func(q string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		e.a.handleDownload(w, httptest.NewRequest("GET", "/api/download/zh-vc6-release"+q, nil))
		return w
	}
	if w := get("?ticket=" + tk["ticket"]); w.Code != 200 || w.Body.String() != "PK-zip" || !strings.Contains(w.Header().Get("Content-Disposition"), "attachment") {
		t.Fatalf("first use must download: %d %q", w.Code, w.Body.String())
	}
	if w := get("?ticket=" + tk["ticket"]); w.Code != 401 {
		t.Errorf("a ticket must work only once, got %d", w.Code)
	}
	if w := get(""); w.Code != 401 {
		t.Errorf("no token and no ticket must be refused, got %d", w.Code)
	}
	if w := get("?ticket=made-up"); w.Code != 401 {
		t.Errorf("an unknown ticket must be refused, got %d", w.Code)
	}
}

func TestMingwContainerBuildsAreLinkedStatically(t *testing.T) {
	a := doctorEnv(t, goodTools()).a
	b := Backend{Kind: "podman", Path: "podman", Ready: true}
	_, argv, err := a.wrapImage(b, "generals-portal-clang:1", false, t.TempDir(), "cmake", nil)
	if err != nil || !strings.Contains(strings.Join(argv, " "), "LDFLAGS=-static") {
		t.Errorf("a MinGW build must link its runtime in, so the exe needs no libwinpthread-1.dll: %v %v", argv, err)
	}
	_, argv, _ = a.wrapImage(b, "generals-portal-vc6-wine:3", false, t.TempDir(), "cmake", nil)
	if strings.Contains(strings.Join(argv, " "), "LDFLAGS") {
		t.Errorf("the VC6 image must not get MinGW linker flags: %v", argv)
	}
}

func TestStaticD3DX8IsLinkedWhenMinDx8SdkIsThere(t *testing.T) {
	a := doctorEnv(t, goodTools()).a
	src := t.TempDir()
	var log strings.Builder
	var scripts []string
	run := func(ctx context.Context, dir string, w io.Writer, name string, args ...string) error {
		scripts = append(scripts, name+" "+strings.Join(args, " "))
		return nil
	}
	// min-dx8-sdk not downloaded (configure failed or the project has none): nothing runs, the build goes on
	a.prepareDX8(context.Background(), run, src, "mingw-w64-i686", &log)
	if len(scripts) != 0 || !strings.Contains(log.String(), "d3dx8d.dll") {
		t.Fatalf("without d3dx8.lib there is nothing to set up: %v %q", scripts, log.String())
	}
	lib := filepath.Join(src, "build", "mingw-w64-i686", "_deps", "dx8-src", "d3dx8.lib")
	_ = os.MkdirAll(filepath.Dir(lib), 0o755)
	_ = os.WriteFile(lib, []byte("!<arch>\n"), 0o644)
	a.prepareDX8(context.Background(), run, src, "mingw-w64-i686", &log)
	if len(scripts) != 1 || !strings.Contains(scripts[0], "libd3dx8d.a") || !strings.Contains(scripts[0], "gpa_msvc_compat.o") {
		t.Fatalf("the static library must be built as libd3dx8d.a with the shim: %v", scripts)
	}
	shim, err := os.ReadFile(filepath.Join(src, "build", "gpa-override", "gpa_msvc_compat.cpp"))
	if err != nil || !strings.Contains(string(shim), "__alloca_probe") {
		t.Fatalf("the shim source must be written next to it: %v", err)
	}
	// a failing step never fails the build: the override is removed so the link falls back to the import library
	failing := func(ctx context.Context, dir string, w io.Writer, name string, args ...string) error { return errors.New("boom") }
	a.prepareDX8(context.Background(), failing, src, "mingw-w64-i686", &log)
	if _, err := os.Stat(filepath.Join(src, "build", "gpa-override")); err == nil {
		t.Error("a failed setup must remove the override folder")
	}
}
