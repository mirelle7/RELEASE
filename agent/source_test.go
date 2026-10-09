package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// autoEnv: an agent with NO source folder whose "ggc" project downloads from a local bare repository.
func autoEnv(t *testing.T) (e *env, origin string, shas []string, dirs *dirLog) {
	checkout, shas := history(t)
	e = newEnv(t)
	e.a.setSrc("ggc", "")
	origin = git(t, checkout, "remote", "get-url", "origin")
	e.a.RepoURLs = map[string]string{"ggc": origin, "generalsx": origin}
	dirs = &dirLog{}
	e.a.Run = versionRunner(dirs, nil)
	return
}

func pushCommit(t *testing.T, origin, version string) string {
	other := filepath.Join(t.TempDir(), "other")
	git(t, filepath.Dir(other), "clone", "-q", origin, other)
	_ = os.WriteFile(filepath.Join(other, "VERSION"), []byte(version), 0o644)
	git(t, other, "commit", "-q", "-am", version)
	git(t, other, "push", "-q", "origin", "HEAD:main")
	return git(t, other, "rev-parse", "HEAD")
}

func TestAutoDownloadsBuildsInCacheAndRebuildsIncrementally(t *testing.T) {
	e, origin, shas, dirs := autoEnv(t)
	if r := e.post(`{"id":"a1","preset":"vc6","game":"GeneralsMD","args":[],"ref":"latest"}`); r.code != 202 {
		t.Fatalf("%d %s", r.code, r.body)
	}
	res := e.waitResult("a1")
	if res["ok"] != true || res["sha"] != shas[2] || !strings.HasPrefix(res["label"].(string), "latest main") {
		t.Fatalf("%v", res)
	}
	cache := e.a.cacheDir("ggc")
	if d := dirs.all(); len(d) != 1 || d[0] != cache {
		t.Fatalf("built in %v, want %s", d, cache)
	}
	if got := exeContent(t, e.out, "a1"); got != "v3" {
		t.Fatalf("built %q", got)
	}
	log, _ := os.ReadFile(filepath.Join(e.out, "a1", "build.log"))
	if !strings.Contains(string(log), "Downloading the source from") || !strings.Contains(string(log), "Checking out "+shas[2][:7]) {
		t.Fatalf("log lacks the download lines:\n%s", log)
	}
	if v := git(t, cache, "config", "core.longpaths"); v != "true" {
		t.Fatalf("longpaths %q", v)
	}
	// a second build of another commit: build/ survives, stray files do not
	marker := filepath.Join(cache, "build", "vc6", "marker")
	_ = os.WriteFile(marker, []byte("x"), 0o644)
	_ = os.WriteFile(filepath.Join(cache, "junk.txt"), []byte("x"), 0o644)
	e.post(`{"id":"a2","preset":"vc6","game":"GeneralsMD","args":[],"ref":"` + shas[0] + `"}`)
	res = e.waitResult("a2")
	if res["ok"] != true || res["sha"] != shas[0] {
		t.Fatalf("%v", res)
	}
	if got := exeContent(t, e.out, "a2"); got != "v1" {
		t.Fatalf("built %q", got)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatal("build/ was not kept:", err)
	}
	if _, err := os.Stat(filepath.Join(cache, "junk.txt")); err == nil {
		t.Fatal("untracked file survived")
	}
	log, _ = os.ReadFile(filepath.Join(e.out, "a2", "build.log"))
	if strings.Contains(string(log), "first time only") {
		t.Fatal("cloned again")
	}
	// short sha, and an unknown one
	e.post(`{"id":"a3","preset":"vc6","game":"GeneralsMD","args":[],"ref":"` + shas[1][:9] + `"}`)
	if res = e.waitResult("a3"); res["ok"] != true || res["sha"] != shas[1] {
		t.Fatalf("%v", res)
	}
	e.post(`{"id":"a4","preset":"vc6","game":"GeneralsMD","args":[],"ref":"0123456789abcdef"}`)
	if res = e.waitResult("a4"); res["ok"] == true || res["error"] == "" {
		t.Fatalf("%v", res)
	}
	// "latest" notices a commit pushed meanwhile; an empty ref means the same
	v4 := pushCommit(t, origin, "v4")
	e.post(`{"id":"a5","preset":"vc6","game":"GeneralsMD","args":[]}`)
	if res = e.waitResult("a5"); res["ok"] != true || res["sha"] != v4 || exeContent(t, e.out, "a5") != "v4" {
		t.Fatalf("%v", res)
	}
	// a preset the revision does not have fails after the checkout, with a clear reason
	e.post(`{"id":"a6","preset":"mingw-w64-i686","game":"GeneralsMD","args":[],"ref":"latest"}`)
	if res = e.waitResult("a6"); res["ok"] == true || !strings.Contains(res["error"].(string), "no preset") {
		t.Fatalf("%v", res)
	}
}

func TestAutoRangeBuildsFromTheDownloadedHistory(t *testing.T) {
	e, _, _, dirs := autoEnv(t)
	e.post(`{"id":"r1","preset":"vc6","game":"GeneralsMD","args":[],"range":{"unit":"pr","from":10,"to":30,"step":10}}`)
	waitGroup(t, e, "r1", 3)
	for id, want := range map[string]string{"r1-pr10": "v1", "r1-pr20": "v2", "r1-pr30": "v3"} {
		if got := exeContent(t, e.out, id); got != want {
			t.Errorf("%s built %q want %q", id, got, want)
		}
	}
	e.post(`{"id":"r2","preset":"vc6","game":"GeneralsMD","args":[],"range":{"unit":"commit","from":1,"to":3,"step":2}}`)
	waitGroup(t, e, "r2", 2)
	if exeContent(t, e.out, "r2-c1") != "v1" || exeContent(t, e.out, "r2-c3") != "v3" {
		t.Error("commit range wrong")
	}
	for _, d := range dirs.all() {
		if !strings.Contains(d, "worktrees") {
			t.Errorf("built in %s", d)
		}
	}
}

func TestAutoDoctorSaysWhereTheSourceComesFrom(t *testing.T) {
	e, _, _, _ := autoEnv(t)
	find := func(rep Report, id string) *Check {
		for i := range rep.Checks {
			if rep.Checks[i].ID == id {
				return &rep.Checks[i]
			}
		}
		return nil
	}
	rep := e.a.DoctorProj(context.Background(), "vc6", "", "")
	if c := find(rep, "source"); c == nil || c.Status != "info" || !strings.Contains(c.Detail, "will be downloaded") {
		t.Fatalf("%+v", rep.Checks)
	}
	e.post(`{"id":"d1","preset":"vc6","game":"GeneralsMD","args":[]}`)
	e.waitResult("d1")
	rep = e.a.DoctorProj(context.Background(), "vc6", "", "")
	if c := find(rep, "source"); c == nil || !strings.Contains(c.Detail, "cached "+e.a.cacheHead("ggc")) || !strings.Contains(c.Detail, "MB") {
		t.Fatalf("%+v", rep.Checks)
	}
	// without git the build cannot start, and the doctor says so
	e.a.env = []string{"PATH=" + t.TempDir()}
	e.a.Git = nil
	rep = e.a.DoctorProj(context.Background(), "vc6", "", "")
	if c := find(rep, "git"); c == nil || c.Status != "fail" || rep.Ready {
		t.Fatalf("%+v", rep.Checks)
	}
	// and a platform listing does not fail on a project with no clone yet
	if pl := e.a.PlatformsFor(context.Background(), "generalsx"); len(pl) == 0 {
		t.Fatal("no platforms")
	}
}

func TestProjectsModeSwitching(t *testing.T) {
	e, _, _, _ := autoEnv(t)
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
		if l[id] == nil || l[id]["mode"] != "auto" || l[id]["cached"] != false || l[id]["dir"] != "" {
			t.Fatalf("%s: %v", id, l[id])
		}
	}
	if l["generalsonline"]["repo"] != "https://github.com/GeneralsOnlineDevelopmentTeam/GameClient" || l["bobtista"]["repo"] == "" {
		t.Fatalf("%v", l["generalsonline"])
	}
	good := projectFolder(t, projPresets)
	e.do("POST", "/api/projects", `{"id":"ggc","dir":`+quoteJSON(good)+`}`, tok)
	if l = list(); l["ggc"]["mode"] != "local" || l["ggc"]["dir"] != good || l["ggc"]["has_presets"] != true {
		t.Fatalf("%v", l["ggc"])
	}
	if e.a.isAuto("") {
		t.Fatal("still auto")
	}
	e.do("POST", "/api/projects", `{"id":"ggc","dir":""}`, tok)
	if l = list(); l["ggc"]["mode"] != "auto" || l["ggc"]["dir"] != "" || !e.a.isAuto("") {
		t.Fatalf("%v", l["ggc"])
	}
	// download, then see the cache in the list and clear it
	e.post(`{"id":"m1","preset":"vc6","game":"GeneralsMD","args":[]}`)
	e.waitResult("m1")
	l = list()
	if l["ggc"]["cached"] != true || l["ggc"]["head"] == "" || l["ggc"]["has_presets"] != true || l["ggc"]["cache_dir"] != e.a.cacheDir("ggc") {
		t.Fatalf("%v", l["ggc"])
	}
	if r := e.do("POST", "/api/projects/clear-cache", `{"id":"ggc"}`, tok); r.code != 200 {
		t.Fatalf("%d %s", r.code, r.body)
	}
	if _, err := os.Stat(e.a.cacheDir("ggc")); err == nil {
		t.Fatal("cache not deleted")
	}
	if list()["ggc"]["cached"] != false {
		t.Fatal("still cached")
	}
	if c := e.do("POST", "/api/projects/clear-cache", `{"id":"ggc"}`, nil).code; c != 401 {
		t.Fatal(c)
	}
	// an unknown project still needs a folder
	if c := e.post(`{"id":"u1","project":"mystery","preset":"vc6","game":"GeneralsMD","args":[]}`).code; c != 400 {
		t.Fatal(c)
	}
}

func quoteJSON(s string) string { return `"` + strings.ReplaceAll(s, `\`, `\\`) + `"` }

func TestClearCacheRefusedWhileItsBuildRuns(t *testing.T) {
	e, _, _, dirs := autoEnv(t)
	gate := make(chan struct{})
	e.a.Run = versionRunner(dirs, gate)
	e.post(`{"id":"c1","preset":"vc6","game":"GeneralsMD","args":[]}`)
	for i := 0; i < 400 && len(dirs.all()) == 0; i++ {
		time.Sleep(25 * time.Millisecond)
	}
	if c := e.do("POST", "/api/projects/clear-cache", `{"id":"ggc"}`, tok).code; c != 409 {
		t.Fatalf("clear while building: %d", c)
	}
	close(gate)
	e.waitResult("c1")
}

func TestCommitsWithoutAFolder(t *testing.T) {
	e, origin, shas, _ := autoEnv(t)
	get := func(q string) map[string]any { return e.do("GET", "/api/commits?project=ggc"+q, "", tok).json() }
	// the origin is not on GitHub, so the list comes from downloading the clone
	r := get("")
	cs, _ := r["commits"].([]any)
	if r["branch"] != "main" || r["head"] != shas[2] || len(cs) != 3 || !e.a.cloned("ggc") {
		t.Fatalf("%v", r)
	}
	v4 := pushCommit(t, origin, "v4")
	if r = get("&fetch=1&limit=2"); r["fetched"] != true || r["head"] != v4 || len(r["commits"].([]any)) != 2 {
		t.Fatalf("%v", r)
	}
	// an unreachable repository is a 200 with an error
	e.a.RepoURLs["generalsx"] = filepath.Join(t.TempDir(), "nowhere.git")
	rr := e.do("GET", "/api/commits?project=generalsx", "", tok)
	if m := rr.json(); rr.code != 200 || m["error"] == nil || len(m["commits"].([]any)) != 0 {
		t.Fatalf("%d %s", rr.code, rr.body)
	}
}

func TestCommitsFromTheGitHubAPIWhenNothingIsDownloaded(t *testing.T) {
	var asked []string
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked = append(asked, r.URL.RequestURI())
		switch {
		case r.URL.Path == "/repos/o/r":
			fmt.Fprint(w, `{"default_branch":"bobtista/topic/trunk"}`)
		case r.URL.Path == "/repos/o/r/commits":
			fmt.Fprint(w, `[{"sha":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","commit":{"message":"first line\n\nbody","author":{"name":"Ann","date":"2026-01-02T03:04:05Z"}}},
			 {"sha":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb","commit":{"message":"older","author":{"name":"Bob","date":"2026-01-01T00:00:00Z"}}}]`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer api.Close()
	e := newEnv(t)
	e.a.setSrc("ggc", "")
	e.a.APIBase = api.URL
	e.a.RepoURLs = map[string]string{"ggc": "https://github.com/o/r.git"}
	r := e.do("GET", "/api/commits?project=ggc&limit=2", "", tok).json()
	cs, _ := r["commits"].([]any)
	if r["branch"] != "bobtista/topic/trunk" || r["head"] != strings.Repeat("a", 40) || r["fetched"] != true || len(cs) != 2 {
		t.Fatalf("%v", r)
	}
	if c := cs[0].(map[string]any); c["subject"] != "first line" || c["author"] != "Ann" || c["short"] != "aaaaaaa" || c["date"] != "2026-01-02T03:04:05Z" {
		t.Fatalf("%v", c)
	}
	if e.a.cloned("ggc") {
		t.Fatal("API answer should not download anything")
	}
	if !strings.Contains(strings.Join(asked, " "), "sha=bobtista/topic/trunk&per_page=2") {
		t.Fatalf("%v", asked)
	}
}

func TestDefaultBranchIsAskedNotAssumed(t *testing.T) {
	checkout, _ := history(t)
	origin := git(t, checkout, "remote", "get-url", "origin")
	other := filepath.Join(t.TempDir(), "o")
	git(t, filepath.Dir(other), "clone", "-q", origin, other)
	git(t, other, "push", "-q", "origin", "HEAD:refs/heads/bobtista/topic/trunk")
	git(t, origin, "symbolic-ref", "HEAD", "refs/heads/bobtista/topic/trunk")
	a := NewAgent("", t.TempDir(), "t", nil, true, nil)
	a.CacheDir = t.TempDir()
	a.RepoURLs = map[string]string{"ggc": origin}
	g, err := a.gitFunc()
	if err != nil {
		t.Skip(err)
	}
	dir, err := a.ensureClone(context.Background(), g, "ggc", &strings.Builder{})
	if err != nil {
		t.Fatal(err)
	}
	if b, err := a.defaultBranch(context.Background(), g, dir); err != nil || b != "bobtista/topic/trunk" {
		t.Fatalf("%q %v", b, err)
	}
}

func TestCancelAbortsTheDownload(t *testing.T) {
	e, _, _, _ := autoEnv(t)
	started := make(chan struct{})
	e.a.Git = func(ctx context.Context, dir string, args ...string) (string, error) {
		if args[0] == "clone" {
			close(started)
			<-ctx.Done()
			return "", ctx.Err()
		}
		return "", nil
	}
	e.post(`{"id":"x1","preset":"vc6","game":"GeneralsMD","args":[]}`)
	<-started
	if st := e.do("GET", "/api/status", "", tok).json(); !strings.Contains(fmt.Sprint(st["progress"]), "Downloading the source from") {
		t.Fatalf("progress: %v", st["progress"])
	}
	t0 := time.Now()
	e.do("POST", "/api/cancel", "", tok)
	res := e.waitResult("x1")
	if res["ok"] == true || res["error"] != "cancelled" || time.Since(t0) > 3*time.Second {
		t.Fatalf("%v after %v", res, time.Since(t0))
	}
	if e.a.cloned("ggc") {
		t.Fatal("partial clone left behind")
	}
}
