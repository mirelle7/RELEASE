package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const embedJob = `{"id":"zh-vc6-embed","preset":"vc6","game":"GeneralsMD","embed":true,"args":["-DRTS_BUILD_ZEROHOUR=ON"]}`

func writeGameDLLs(t *testing.T, dir string, names ...string) {
	t.Helper()
	for _, n := range names {
		if err := os.WriteFile(filepath.Join(dir, n), []byte("MZ fake "+n), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func TestFindGameDLLsIgnoresCaseAndReportsWhatIsMissing(t *testing.T) {
	dir := t.TempDir()
	writeGameDLLs(t, dir, "BINKW32.DLL")
	paths, st := findGameDLLs(dir)
	if st.Ready || len(st.Found) != 1 || st.Found[0] != "binkw32.dll" || len(st.Missing) != 1 || st.Missing[0] != "mss32.dll" {
		t.Fatalf("%+v", st)
	}
	if filepath.Base(paths["binkw32.dll"]) != "BINKW32.DLL" {
		t.Fatalf("%v", paths)
	}
	writeGameDLLs(t, dir, "Mss32.dll")
	if _, st = findGameDLLs(dir); !st.Ready {
		t.Fatalf("%+v", st)
	}
}

func TestFindGameDLLsWithNoFolderOrAnUnreadableOne(t *testing.T) {
	for _, dir := range []string{"", filepath.Join(t.TempDir(), "nope")} {
		_, st := findGameDLLs(dir)
		if st.Ready || st.Exists || len(st.Missing) != 2 {
			t.Fatalf("%q: %+v", dir, st)
		}
		if msg := missingDLLsMessage(st); msg == "" {
			t.Fatal("no explanation")
		}
	}
}

func TestEmbedAddsTheProjectIncludeAndGeneratesFiles(t *testing.T) {
	e := newEnv(t)
	game := t.TempDir()
	writeGameDLLs(t, game, "binkw32.dll", "mss32.dll")
	e.a.DataDir = game

	if r := e.post(embedJob); r.code != 202 {
		t.Fatalf("%d %s", r.code, r.body)
	}
	res := e.waitResult("zh-vc6-embed")
	if res["ok"] != true || res["note"] != nil {
		t.Fatalf("%v", res)
	}
	e.mu.Lock()
	configure := strings.Join(e.calls[0], " ")
	e.mu.Unlock()
	inc := filepath.ToSlash(filepath.Join(e.out, "zh-vc6-embed", "embed", "embed.cmake"))
	if !strings.Contains(configure, "-DCMAKE_PROJECT_INCLUDE="+inc) {
		t.Fatalf("configure did not get the include: %s", configure)
	}
	dir := filepath.Join(e.out, "zh-vc6-embed", "embed")
	for _, f := range []string{"embed.cmake", "embedded_dlls.rc", "embedded_dlls_hook.cpp", "binkw32.dll", "mss32.dll"} {
		if _, err := os.Stat(filepath.Join(dir, f)); err != nil {
			t.Fatal(err)
		}
	}
	rc, _ := os.ReadFile(filepath.Join(dir, "embedded_dlls.rc"))
	if !strings.Contains(string(rc), "7001 RCDATA") || !strings.Contains(string(rc), "7002 RCDATA") {
		t.Fatalf("rc: %s", rc)
	}
	cm, _ := os.ReadFile(filepath.Join(dir, "embed.cmake"))
	for _, want := range []string{"/DELAYLOAD:binkw32.dll", "/DELAYLOAD:mss32.dll", "delayimp.lib", "g_generals", "z_generals"} {
		if !strings.Contains(string(cm), want) {
			t.Fatalf("embed.cmake lacks %s", want)
		}
	}
	hook, _ := os.ReadFile(filepath.Join(dir, "embedded_dlls_hook.cpp"))
	if strings.Contains(string(hook), "@") {
		t.Fatalf("unreplaced placeholder in the hook:\n%s", hook)
	}
	if !strings.Contains(string(hook), "__pfnDliNotifyHook") {
		t.Fatal("no delay-load hook")
	}
}

func TestEmbedWithoutTheDLLsStillBuildsAndSaysSo(t *testing.T) {
	for name, setup := range map[string]func(*env){
		"no folder set":  func(e *env) {},
		"missing a file": func(e *env) { g := t.TempDir(); writeGameDLLs(t, g, "binkw32.dll"); e.a.DataDir = g },
	} {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t)
			setup(e)
			if r := e.post(embedJob); r.code != 202 {
				t.Fatalf("%d %s", r.code, r.body)
			}
			res := e.waitResult("zh-vc6-embed")
			note, _ := res["note"].(string)
			if res["ok"] != true || !strings.Contains(note, "not embedded") {
				t.Fatalf("%v", res)
			}
			e.mu.Lock()
			defer e.mu.Unlock()
			if strings.Contains(strings.Join(e.calls[0], " "), "CMAKE_PROJECT_INCLUDE") {
				t.Fatal("tried to embed without the DLLs")
			}
		})
	}
}

func TestNoEmbedMeansNoChange(t *testing.T) {
	e := newEnv(t)
	game := t.TempDir()
	writeGameDLLs(t, game, "binkw32.dll", "mss32.dll")
	e.a.DataDir = game
	if r := e.post(okJob); r.code != 202 {
		t.Fatalf("%d %s", r.code, r.body)
	}
	res := e.waitResult("zh-vc6-release")
	e.mu.Lock()
	defer e.mu.Unlock()
	if res["note"] != nil || strings.Contains(strings.Join(e.calls[0], " "), "CMAKE_PROJECT_INCLUDE") {
		t.Fatalf("%v %v", res, e.calls[0])
	}
}

func TestMingwCannotEmbed(t *testing.T) {
	if canEmbed("mingw") || !canEmbed("vc6") || !canEmbed("msvc") {
		t.Fatal("wrong families")
	}
}

func TestDataEndpoint(t *testing.T) {
	e := newEnv(t)
	cfg := filepath.Join(t.TempDir(), "agent-config.json")
	e.a.ConfigPath = cfg
	if c := e.do("GET", "/api/data", "", nil).code; c != 401 {
		t.Fatalf("needs a token: %d", c)
	}
	st := e.do("GET", "/api/data", "", tok).json()
	if st["ready"] != false || st["dir"] != "" {
		t.Fatalf("%v", st)
	}

	game := t.TempDir()
	writeGameDLLs(t, game, "binkw32.dll", "mss32.dll")
	body := `{"dir":` + jsonString(game) + `}`
	st = e.do("POST", "/api/data", body, tok).json()
	if st["ready"] != true {
		t.Fatalf("%v", st)
	}
	if raw, _ := os.ReadFile(cfg); !strings.Contains(string(raw), `"Data"`) {
		t.Fatalf("not saved: %s", raw)
	}

	if r := e.do("POST", "/api/data", `{"dir":`+jsonString(filepath.Join(game, "nope"))+`}`, tok); r.code != 400 {
		t.Fatalf("a folder that does not exist: %d", r.code)
	}
	if r := e.do("POST", "/api/data", `{"dir":"x","extra":1}`, tok); r.code != 400 {
		t.Fatalf("unknown field: %d", r.code)
	}
	st = e.do("POST", "/api/data", `{"dir":""}`, tok).json()
	if st["dir"] != "" || st["ready"] != false {
		t.Fatalf("clearing: %v", st)
	}
	if c := e.do("DELETE", "/api/data", "", tok).code; c != 405 {
		t.Fatal(c)
	}
}

func jsonString(s string) string {
	return `"` + strings.ReplaceAll(s, `\`, `\\`) + `"`
}
