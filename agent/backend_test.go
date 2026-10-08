package main

import (
	"archive/zip"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf16"
)

// A fake container engine. State lives in $FAKE_STATE: files "down", "windows", "image" switch its behaviour,
// "runs" records every `run`, "dockerfile" receives what `build -` was given, "probe" is what the tool probe prints.
const fakeEngine = `
S="$FAKE_STATE"
case "$1" in
 info) [ -f "$S/down" ] && exit 1; if [ -f "$S/windows" ]; then echo windows; else echo linux; fi ;;
 image) [ -f "$S/image" ] && exit 0; exit 1 ;;
 build) /bin/cat > "$S/dockerfile"; /bin/touch "$S/image"; echo "built image" ;;
 pull) /bin/touch "$S/image"; echo pulled ;;
 run)
  echo "$@" >> "$S/runs"
  src=""; preset=""; build=""; prev=""
  for a in "$@"; do
    case "$a" in type=bind,source=*) src="${a#type=bind,source=}"; src="${src%,target=/src}";; esac
    [ "$prev" = "--preset" ] && preset="$a"
    [ "$a" = "--build" ] && build=1
    prev="$a"
  done
  case " $* " in *" sh -c "*) /bin/cat "$S/probe" 2>/dev/null; exit 0;; esac
  [ -n "$build" ] && { /bin/mkdir -p "$src/build/$preset/GeneralsMD"; printf MZ > "$src/build/$preset/GeneralsMD/g.exe"; }
  exit 0 ;;
esac`

type cenv struct {
	*env
	state string
}

func containerEnv(t *testing.T, tools map[string]string) *cenv {
	t.Helper()
	e := newEnv(t)
	state := t.TempDir()
	e.a.GOOS = "linux"
	e.a.env = []string{"PATH=" + fakeBin(t, tools), "FAKE_STATE=" + state}
	_ = os.WriteFile(filepath.Join(e.src, "CMakePresets.json"), []byte(`{"version":6,"configurePresets":[{"name":"vc6"},{"name":"mingw-w64-i686"},{"name":"mingw-w64-i686-debug"}]}`), 0o644)
	e.a.Run = nil // the real runner, so the container is actually driven
	return &cenv{e, state}
}

func (c *cenv) touch(name string) { _ = os.WriteFile(filepath.Join(c.state, name), []byte("x"), 0o644) }
func (c *cenv) read(name string) string {
	b, _ := os.ReadFile(filepath.Join(c.state, name))
	return string(b)
}

const mingwJob = `{"id":"zh-mingw-release","preset":"mingw-w64-i686","game":"GeneralsMD","args":["-DRTS_BUILD_GENERALS=OFF","-DRTS_FLAGS=/W3;/DUSE_BUFFERED_IO=0"]}`

func TestNoContainerEngineGivesGuidance(t *testing.T) {
	c := containerEnv(t, nil)
	r := c.a.Doctor(context.Background(), "mingw-w64-i686")
	sum := r.Summary()
	if r.Ready || !strings.Contains(sum, "Docker Desktop") || !strings.Contains(sum, "Podman") || !strings.Contains(sum, "wsl --install") {
		t.Fatalf("%s", sum)
	}
}

func TestDockerInstalledButNotRunning(t *testing.T) {
	c := containerEnv(t, map[string]string{"docker": fakeEngine})
	c.touch("down")
	b := c.a.probeEngine("docker")
	if b.Ready || !strings.Contains(b.Problem, "not running") || !strings.Contains(b.Fix, "Start Docker Desktop") {
		t.Fatalf("%+v", b)
	}
}

func TestDockerInWindowsContainerMode(t *testing.T) {
	c := containerEnv(t, map[string]string{"docker": fakeEngine})
	c.touch("windows")
	b := c.a.probeEngine("docker")
	if b.Ready || !strings.Contains(b.Fix, "Switch to Linux containers") {
		t.Fatalf("%+v", b)
	}
}

func TestPodmanIsUsedWhenDockerIsStopped(t *testing.T) {
	dockerState, podmanState := t.TempDir(), t.TempDir()
	_ = os.WriteFile(filepath.Join(dockerState, "down"), []byte("x"), 0o644) // docker is installed but stopped
	c := containerEnv(t, map[string]string{
		"docker": `FAKE_STATE="` + dockerState + `"` + "\n" + fakeEngine,
		"podman": `FAKE_STATE="` + podmanState + `"` + "\n" + fakeEngine,
	})
	bs := c.a.Backends()
	got := pickBackend(bs, "auto")
	if got == nil || got.Kind != "podman" {
		t.Fatalf("%+v", bs)
	}
	if pickBackend(bs, "docker") != nil {
		t.Error("--backend docker must not fall back to podman")
	}
}

func TestBackendPreference(t *testing.T) {
	bs := []Backend{{Kind: "docker", Ready: true}, {Kind: "podman", Ready: true}, {Kind: "wsl", Distro: "Ubuntu", Ready: true}}
	for pref, want := range map[string]string{"auto": "docker", "": "docker", "podman": "podman", "wsl": "wsl", "wsl:Ubuntu": "wsl"} {
		if b := pickBackend(bs, pref); b == nil || b.Kind != want {
			t.Errorf("%q -> %+v", pref, b)
		}
	}
	if pickBackend(bs, "wsl:Debian") != nil || pickBackend([]Backend{{Kind: "docker"}}, "auto") != nil {
		t.Error("picked something that is not available")
	}
}

func TestMingwBuildRunsInAContainerAndIsPackaged(t *testing.T) {
	c := containerEnv(t, map[string]string{"docker": fakeEngine})
	if r := c.post(mingwJob); r.code != 202 {
		t.Fatalf("%d %s", r.code, r.body)
	}
	res := c.waitResult("zh-mingw-release")
	if res["ok"] != true {
		t.Fatalf("%v", res)
	}
	// the shipped recipe was used to build the image the first time
	df := c.read("dockerfile")
	for _, want := range []string{"FROM ubuntu", "gcc-mingw-w64-i686", "wine64-tools", "widl", "cmake"} {
		if !strings.Contains(df, want) {
			t.Errorf("Dockerfile is missing %q", want)
		}
	}
	runs := strings.Split(strings.TrimSpace(c.read("runs")), "\n")
	if len(runs) != 2 {
		t.Fatalf("expected configure + build, got %v", runs)
	}
	cfg := runs[0]
	for _, want := range []string{"--rm", "--mount type=bind,source=" + c.src + ",target=/src", "-w /src", defaultImage, "cmake --preset mingw-w64-i686", "-DRTS_BUILD_GENERALS=OFF", "-DRTS_FLAGS=/W3;/DUSE_BUFFERED_IO=0", "--user"} {
		if !strings.Contains(cfg, want) {
			t.Errorf("configure command lacks %q:\n%s", want, cfg)
		}
	}
	if !strings.Contains(runs[1], "cmake --build --preset mingw-w64-i686") {
		t.Errorf("%s", runs[1])
	}
	zr, err := zip.OpenReader(filepath.Join(c.out, "zh-mingw-release", "zh-mingw-release.zip"))
	if err != nil || zr.File[0].Name != "g.exe" {
		t.Fatalf("zip: %v", err)
	}
}

func TestImageIsOnlyBuiltOnce(t *testing.T) {
	c := containerEnv(t, map[string]string{"docker": fakeEngine})
	c.touch("image") // already built
	c.post(mingwJob)
	if res := c.waitResult("zh-mingw-release"); res["ok"] != true {
		t.Fatalf("%v", res)
	}
	if c.read("dockerfile") != "" {
		t.Fatal("rebuilt an image that already exists")
	}
}

func TestCustomImageIsPulledNotBuilt(t *testing.T) {
	c := containerEnv(t, map[string]string{"docker": fakeEngine})
	c.a.Image = "example.org/me/toolchain:1"
	c.post(mingwJob)
	c.waitResult("zh-mingw-release")
	if !strings.Contains(c.read("runs"), "example.org/me/toolchain:1 cmake") || c.read("dockerfile") != "" {
		t.Fatalf("runs: %s", c.read("runs"))
	}
}

func TestMingwBuildWithNoEngineExplainsInTheResult(t *testing.T) {
	c := containerEnv(t, nil)
	c.post(mingwJob)
	res := c.waitResult("zh-mingw-release")
	if res["ok"] == true || !strings.Contains(res["error"].(string), "Docker Desktop") {
		t.Fatalf("%v", res)
	}
}

func TestToolchainCheckReportsMissingTools(t *testing.T) {
	c := containerEnv(t, map[string]string{"docker": fakeEngine})
	c.touch("image")
	_ = os.WriteFile(filepath.Join(c.state, "probe"), []byte("MISSING:widl\ncmake version 3.28.3\n"), 0o644)
	missing, ver, err := c.a.CheckToolchain(context.Background(), Backend{Kind: "docker", Path: which(c.a.env, "docker")})
	if err != nil || len(missing) != 1 || missing[0] != "widl" || !strings.Contains(ver, "3.28.3") {
		t.Fatalf("%v %q %v", missing, ver, err)
	}
	_ = os.WriteFile(filepath.Join(c.state, "probe"), []byte("cmake version 3.28.3\n"), 0o644)
	if missing, _, _ := c.a.CheckToolchain(context.Background(), Backend{Kind: "docker", Path: which(c.a.env, "docker")}); len(missing) != 0 {
		t.Fatalf("%v", missing)
	}
}

func TestWrapDockerOnWindowsHostDoesNotSetAUser(t *testing.T) {
	a := &Agent{GOOS: "windows"}
	_, argv, _ := a.wrap(Backend{Kind: "docker", Path: "docker"}, `C:\src\game`, "cmake", []string{"--version"})
	j := strings.Join(argv, " ")
	if strings.Contains(j, "--user") || !strings.Contains(j, `source=C:\src\game,target=/src`) {
		t.Fatal(j)
	}
	a.GOOS = "linux"
	_, argv, _ = a.wrap(Backend{Kind: "podman", Path: "podman"}, "/home/me/game", "cmake", nil)
	if j := strings.Join(argv, " "); !strings.Contains(j, "--user") || !strings.Contains(j, "label=disable") {
		t.Fatal(j)
	}
}

func utf16le(s string) []byte {
	var out []byte
	for _, u := range utf16.Encode([]rune("\ufeff" + s)) {
		out = append(out, byte(u), byte(u>>8))
	}
	return out
}

func TestDecodeWSLListHandlesUTF16(t *testing.T) {
	got := decodeWSLList(utf16le("Ubuntu-22.04\r\ndocker-desktop\r\nDebian\r\n"))
	if strings.Join(got, ",") != "Ubuntu-22.04,Debian" {
		t.Fatalf("%v", got)
	}
	if got := decodeWSLList([]byte("Ubuntu\n")); len(got) != 1 {
		t.Fatalf("%v", got)
	}
}

func TestWSLPath(t *testing.T) {
	for in, want := range map[string]string{`C:\Users\me\game`: "/mnt/c/Users/me/game", "D:/x/y": "/mnt/d/x/y", "/already/linux": "/already/linux"} {
		if got := wslPath(in); got != want {
			t.Errorf("%s -> %s", in, got)
		}
	}
}

// fake wsl.exe: `-l -q` lists distributions, `-d D -- sh -c` is the tool probe, `-d D --cd P -- cmake` runs a build.
const fakeWSL = `
S="$FAKE_STATE"
if [ "$1" = "-l" ]; then /bin/cat "$S/distros"; exit 0; fi
if [ "$1" = "-d" ]; then
  case " $* " in *" sh -c "*) /bin/cat "$S/probe" 2>/dev/null; exit 0;; esac
  cd=""; preset=""; build=""; prev=""
  for a in "$@"; do
    [ "$prev" = "--cd" ] && cd="$a"
    [ "$prev" = "--preset" ] && preset="$a"
    [ "$a" = "--build" ] && build=1
    prev="$a"
  done
  echo "$@" >> "$S/runs"
  [ -n "$build" ] && { /bin/mkdir -p "$cd/build/$preset/GeneralsMD"; printf MZ > "$cd/build/$preset/GeneralsMD/g.exe"; }
  exit 0
fi
exit 1`

func wslEnv(t *testing.T, probe string) *cenv {
	c := containerEnv(t, map[string]string{"wsl": fakeWSL})
	c.a.GOOS = "windows"
	_ = os.WriteFile(filepath.Join(c.state, "distros"), utf16le("Ubuntu\r\ndocker-desktop\r\n"), 0o644)
	_ = os.WriteFile(filepath.Join(c.state, "probe"), []byte(probe), 0o644)
	return c
}

func TestWSLDistroWithTheToolsIsReady(t *testing.T) {
	c := wslEnv(t, "cmake version 3.28.3\n")
	bs := c.a.Backends()
	var w *Backend
	for i := range bs {
		if bs[i].Kind == "wsl" {
			w = &bs[i]
		}
	}
	if w == nil || !w.Ready || w.Distro != "Ubuntu" {
		t.Fatalf("%+v", bs)
	}
}

func TestWSLDistroMissingToolsSaysWhichAndHowToInstall(t *testing.T) {
	c := wslEnv(t, "MISSING:widl\nMISSING:ninja\ncmake version 3.25.1\n")
	var w Backend
	for _, b := range c.a.Backends() {
		if b.Kind == "wsl" {
			w = b
		}
	}
	if w.Ready || !strings.Contains(w.Problem, "widl") || !strings.Contains(w.Problem, "ninja") || !strings.Contains(w.Problem, "cmake 3.28") || !strings.Contains(w.Fix, "apt-get install") {
		t.Fatalf("%+v", w)
	}
}

func TestMingwBuildInsideWSL(t *testing.T) {
	c := wslEnv(t, "cmake version 3.28.3\n")
	c.post(mingwJob)
	res := c.waitResult("zh-mingw-release")
	if res["ok"] != true {
		t.Fatalf("%v", res)
	}
	runs := c.read("runs")
	if !strings.Contains(runs, "-d Ubuntu --cd "+c.src+" -- cmake --preset mingw-w64-i686") {
		t.Fatalf("%s", runs)
	}
}

func TestWSLBackendNeedsNoImage(t *testing.T) {
	c := wslEnv(t, "cmake version 3.28.3\n")
	r := c.a.Doctor(context.Background(), "mingw-w64-i686")
	if !r.Ready || r.Backend != "wsl:Ubuntu" || status(r, "image") != "missing" {
		t.Fatalf("%+v", r)
	}
}
