package main

import (
	"archive/zip"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-c", "user.name=t", "-c", "user.email=t@t", "-c", "init.defaultBranch=main", "-c", "protocol.file.allow=always"}, args...)...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// history builds a "remote" with three commits (VERSION=v1,v2,v3) and pull request heads 10, 20, 30 pointing at them,
// and a clone of it that plays the part of the user's checkout (on the newest commit, with an untouched working tree).
func history(t *testing.T) (checkout string, shas []string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	root := t.TempDir()
	work, bare, checkout := filepath.Join(root, "work"), filepath.Join(root, "remote.git"), filepath.Join(root, "checkout")
	_ = os.MkdirAll(work, 0o755)
	git(t, work, "init", "-q")
	for i, v := range []string{"v1", "v2", "v3"} {
		_ = os.WriteFile(filepath.Join(work, "CMakePresets.json"), []byte(presetsJSON), 0o644)
		_ = os.WriteFile(filepath.Join(work, "VERSION"), []byte(v), 0o644)
		git(t, work, "add", "-A")
		git(t, work, "commit", "-q", "-m", v)
		shas = append(shas, git(t, work, "rev-parse", "HEAD"))
		_ = i
	}
	git(t, root, "clone", "-q", "--bare", work, bare)
	for pr, sha := range map[string]string{"10": shas[0], "20": shas[1], "30": shas[2]} {
		git(t, bare, "update-ref", "refs/pull/"+pr+"/head", sha)
	}
	git(t, root, "clone", "-q", bare, checkout)
	return checkout, shas
}

// versionRunner fakes cmake: the "binary" it produces contains the VERSION file of whatever tree it was run in.
// dirLog is a goroutine-safe list of the directories the fake cmake built in.
type dirLog struct {
	mu sync.Mutex
	l  []string
}

func (d *dirLog) add(s string) { d.mu.Lock(); d.l = append(d.l, s); d.mu.Unlock() }
func (d *dirLog) all() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string(nil), d.l...)
}

func versionRunner(dirs *dirLog, gate chan struct{}) Runner {
	return func(ctx context.Context, dir string, log io.Writer, name string, args ...string) error {
		if args[0] != "--build" {
			return nil
		}
		dirs.add(dir)
		if gate != nil {
			select {
			case <-gate:
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		v, err := os.ReadFile(filepath.Join(dir, "VERSION"))
		if err != nil {
			return err
		}
		d := filepath.Join(dir, "build", args[2], "GeneralsMD")
		_ = os.MkdirAll(d, 0o755)
		return os.WriteFile(filepath.Join(d, "g.exe"), v, 0o644)
	}
}

func exeContent(t *testing.T, out, id string) string {
	t.Helper()
	zr, err := zip.OpenReader(filepath.Join(out, id, id+".zip"))
	if err != nil {
		t.Fatalf("%s: %v", id, err)
	}
	defer zr.Close()
	for _, f := range zr.File {
		if f.Name == "g.exe" {
			rc, _ := f.Open()
			b, _ := io.ReadAll(rc)
			rc.Close()
			return string(b)
		}
	}
	t.Fatalf("%s: no g.exe in zip", id)
	return ""
}

func rangeEnv(t *testing.T, gate chan struct{}) (*env, string, []string, *dirLog) {
	checkout, shas := history(t)
	e := newEnv(t)
	e.a.Src = checkout
	e.a.Run = nil
	dirs := &dirLog{}
	e.a.Run = versionRunner(dirs, gate)
	return e, checkout, shas, dirs
}

func rangeJob(unit string, from, to, step int) string {
	return strings.NewReplacer("UNIT", unit, "FROM", itoa(from), "TO", itoa(to), "STEP", itoa(step)).Replace(
		`{"id":"r-vc6","preset":"vc6","game":"GeneralsMD","args":["-DRTS_BUILD_GENERALS=OFF"],"range":{"unit":"UNIT","from":FROM,"to":TO,"step":STEP}}`)
}

func itoa(n int) string { return strconv.Itoa(n) }

func waitGroup(t *testing.T, e *env, group string, n int) []map[string]any {
	t.Helper()
	for i := 0; i < 400; i++ {
		st := e.do("GET", "/api/status", "", tok).json()
		var got []map[string]any
		for _, r := range st["results"].([]any) {
			if m := r.(map[string]any); m["group"] == group {
				got = append(got, m)
			}
		}
		if len(got) >= n && st["busy"] == false {
			return got
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatal("range did not finish")
	return nil
}

func TestPRRangeBuildsEachStepFromThatPullRequest(t *testing.T) {
	e, checkout, shas, dirs := rangeEnv(t, nil)
	headBefore := git(t, checkout, "rev-parse", "HEAD")
	if r := e.post(`{"id":"r-vc6","preset":"vc6","game":"GeneralsMD","args":[],"range":{"unit":"pr","from":10,"to":30,"step":10}}`); r.code != 202 {
		t.Fatalf("%d %s", r.code, r.body)
	}
	res := waitGroup(t, e, "r-vc6", 3)
	for id, want := range map[string]string{"r-vc6-pr10": "v1", "r-vc6-pr20": "v2", "r-vc6-pr30": "v3"} {
		if got := exeContent(t, e.out, id); got != want {
			t.Errorf("%s built from %q, want %q", id, got, want)
		}
	}
	for _, r := range res {
		if r["ok"] != true || r["sha"] == "" || !strings.HasPrefix(r["label"].(string), "PR #") {
			t.Errorf("%v", r)
		}
	}
	// every build ran in the temporary worktree, never in the user's checkout
	for _, d := range dirs.all() {
		if d == checkout || !strings.Contains(d, "worktrees") {
			t.Errorf("built in %s", d)
		}
	}
	// the user's checkout is exactly as it was
	if got := git(t, checkout, "rev-parse", "HEAD"); got != headBefore || got != shas[2] {
		t.Errorf("HEAD moved: %s", got)
	}
	if st := git(t, checkout, "status", "--porcelain"); st != "" {
		t.Errorf("working tree changed:\n%s", st)
	}
	if wl := git(t, checkout, "worktree", "list"); strings.Count(wl, "\n") != 0 {
		t.Errorf("worktree left behind:\n%s", wl)
	}
	if _, err := os.Stat(filepath.Join(e.out, "worktrees", "r-vc6")); err == nil {
		t.Error("worktree folder left behind")
	}
}

func TestIntervalSkipsPoints(t *testing.T) {
	e, _, _, _ := rangeEnv(t, nil)
	e.post(`{"id":"r-vc6","preset":"vc6","game":"GeneralsMD","args":[],"range":{"unit":"pr","from":10,"to":30,"step":20}}`) // 10 and 30
	res := waitGroup(t, e, "r-vc6", 2)
	if len(res) != 2 || exeContent(t, e.out, "r-vc6-pr10") != "v1" || exeContent(t, e.out, "r-vc6-pr30") != "v3" {
		t.Fatalf("%v", res)
	}
	if _, err := os.Stat(filepath.Join(e.out, "r-vc6-pr20")); err == nil {
		t.Fatal("built a point the interval skips")
	}
}

func TestMissingPRFailsThatStepAndContinues(t *testing.T) {
	e, _, _, _ := rangeEnv(t, nil)
	e.post(`{"id":"r-vc6","preset":"vc6","game":"GeneralsMD","args":[],"range":{"unit":"pr","from":10,"to":40,"step":10}}`)
	res := waitGroup(t, e, "r-vc6", 4)
	byID := map[string]map[string]any{}
	for _, r := range res {
		byID[r["id"].(string)] = r
	}
	if byID["r-vc6-pr40"]["ok"] == true || !strings.Contains(byID["r-vc6-pr40"]["error"].(string), "PR #40") {
		t.Fatalf("%v", byID["r-vc6-pr40"])
	}
	if byID["r-vc6-pr30"]["ok"] != true {
		t.Fatalf("a later failure must not affect earlier steps: %v", byID["r-vc6-pr30"])
	}
}

func TestCommitRangeUsesHistoryOrder(t *testing.T) {
	e, _, shas, _ := rangeEnv(t, nil)
	e.post(`{"id":"r-vc6","preset":"vc6","game":"GeneralsMD","args":[],"range":{"unit":"commit","from":1,"to":5,"step":2}}`) // 1, 3, 5
	res := waitGroup(t, e, "r-vc6", 3)
	by := map[string]map[string]any{}
	for _, r := range res {
		by[r["id"].(string)] = r
	}
	if exeContent(t, e.out, "r-vc6-c1") != "v1" || exeContent(t, e.out, "r-vc6-c3") != "v3" || by["r-vc6-c1"]["sha"] != shas[0] {
		t.Fatalf("%v", res)
	}
	if by["r-vc6-c5"]["ok"] == true || !strings.Contains(by["r-vc6-c5"]["error"].(string), "only 3 commits") {
		t.Fatalf("%v", by["r-vc6-c5"])
	}
}

func TestOldRevisionWithoutThePresetFailsCleanly(t *testing.T) {
	checkout, _ := history(t)
	git(t, checkout, "rm", "-q", "CMakePresets.json")
	git(t, checkout, "commit", "-q", "-m", "drop presets")
	// the preset is checked against the user's checkout first, so use dry validation off: restore it for validation
	e := newEnv(t)
	e.a.Src = checkout
	_ = os.WriteFile(filepath.Join(checkout, "CMakePresets.json"), []byte(presetsJSON), 0o644) // untracked, only for Validate
	e.a.Run = versionRunner(&dirLog{}, nil)
	e.post(`{"id":"r-vc6","preset":"vc6","game":"GeneralsMD","args":[],"range":{"unit":"commit","from":4,"to":4,"step":1}}`) // commit 4 is the one without presets
	res := waitGroup(t, e, "r-vc6", 1)
	if res[0]["ok"] == true || !strings.Contains(res[0]["error"].(string), "CMakePresets.json") {
		t.Fatalf("%v", res[0])
	}
}

func TestRangeValidation(t *testing.T) {
	e := newEnv(t)
	bad := []string{
		`"unit":"branch","from":1,"to":5,"step":1`, `"unit":"pr","from":0,"to":5,"step":1`, `"unit":"pr","from":9,"to":5,"step":1`,
		`"unit":"pr","from":1,"to":5,"step":0`, `"unit":"pr","from":1,"to":5,"step":-1`, `"unit":"pr","from":1,"to":1000,"step":1`,
		`"unit":"pr","from":1,"to":999999999,"step":1`, `"unit":"pr","from":1,"to":5`,
	}
	for _, r := range bad {
		body := `{"id":"r-vc6","preset":"vc6","game":"GeneralsMD","args":[],"range":{` + r + `}}`
		if c := e.post(body).code; c != 400 {
			t.Errorf("%s -> %d", r, c)
		}
	}
	if c := e.post(`{"id":"r-vc6","preset":"vc6","game":"GeneralsMD","args":[],"range":{"unit":"pr","from":1,"to":100,"step":1}}`).code; c != 202 {
		t.Errorf("exactly %d steps should be accepted, got %d", maxRangeSteps, c)
	}
}

func TestCancelStopsTheRangeAndClearsTheQueue(t *testing.T) {
	gate := make(chan struct{})
	e, _, _, dirs := rangeEnv(t, gate)
	e.post(`{"id":"r-vc6","preset":"vc6","game":"GeneralsMD","args":[],"range":{"unit":"pr","from":10,"to":30,"step":10}}`)
	e.post(`{"id":"other","preset":"vc6","game":"GeneralsMD","args":[]}`)
	for i := 0; i < 200 && len(dirs.all()) == 0; i++ {
		time.Sleep(10 * time.Millisecond)
	}
	if st := e.do("GET", "/api/status", "", tok).json(); st["progress"] == "" || len(st["queue"].([]any)) != 1 {
		t.Fatalf("status while running: %v", st)
	}
	if c := e.do("POST", "/api/cancel", "", tok).code; c != 200 {
		t.Fatal(c)
	}
	if c := e.do("POST", "/api/cancel", "", nil).code; c != 401 {
		t.Fatal("cancel without token", c)
	}
	for i := 0; i < 200; i++ {
		if st := e.do("GET", "/api/status", "", tok).json(); st["busy"] == false {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	if n := len(dirs.all()); n != 1 {
		t.Fatalf("builds started after cancel: %d", n)
	}
	if _, err := os.Stat(filepath.Join(e.out, "other", "manifest.json")); err == nil {
		t.Fatal("a queued job ran after cancel")
	}
	st := e.do("GET", "/api/status", "", tok).json()
	if len(st["queue"].([]any)) != 0 {
		t.Fatal("queue not cleared")
	}
	// the agent still works afterwards
	close(gate)
	if r := e.post(okJob); r.code != 202 {
		t.Fatal(r.code)
	}
}

func TestRTSFlagsArgumentIsValidatedStrictly(t *testing.T) {
	e := newEnv(t)
	ok := `{"id":"x","preset":"vc6","game":"GeneralsMD","args":["-DRTS_FLAGS=/W3;/DPRESERVE_TUNNEL_HEAL_STACKING=0;/DUSE_BUFFERED_IO=1"]}`
	if c := e.post(ok).code; c != 202 {
		t.Fatalf("valid flags rejected: %d", c)
	}
	bad := []string{
		`"-DRTS_FLAGS=/W3;/DX_Y=0;/DEVIL=1&calc"`, `"-DRTS_FLAGS=/W3;/DABC=2"`, `"-DRTS_FLAGS=/W3;/Ofast"`, `"-DRTS_FLAGS=/W3;/DABC=0;/DABC2=1;/I../.."`,
		`"-DRTS_FLAGS=/W4;/DABC=0"`, `"-DRTS_FLAGS=/DABC=0"`, `"-DRTS_FLAGS=/W3"`, `"-DRTS_FLAGS=/W3;/Dabc=0"`,
		`"-DRTS_FLAGS=/W3;/DABC=0","-DRTS_FLAGS=/W3;/DDEF=1"`, // two of them
		`"-DCMAKE_CXX_FLAGS=/DABC=0"`,
	}
	for _, a := range bad {
		body := `{"id":"y","preset":"vc6","game":"GeneralsMD","args":[` + a + `]}`
		if c := e.post(body).code; c != 400 {
			t.Errorf("%s -> %d, want 400", a, c)
		}
	}
}

var _ = httptest.NewServer
var _ = http.StatusOK
