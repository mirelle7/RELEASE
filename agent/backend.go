package main

import (
	"bytes"
	"context"
	_ "embed"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"time"
	"unicode/utf16"
)

// Where builds run. Visual C++ 6 and Visual Studio are Windows programs that cannot be shipped, so those builds run
// natively on a Windows PC that has them. The MinGW-w64 preset needs only open-source tools, so its whole build
// environment is shipped as a container image (Docker or Podman) or used directly inside a WSL distribution.

//go:embed toolchain/Dockerfile
var toolchainDockerfile string

const defaultImage = "generals-portal-mingw:2"

// tools the MinGW preset needs inside the container or distro
var mingwTools = []string{"i686-w64-mingw32-gcc", "i686-w64-mingw32-g++", "i686-w64-mingw32-windres", "cmake", "ninja", "git", "widl"}

const toolProbe = `for t in i686-w64-mingw32-gcc i686-w64-mingw32-g++ i686-w64-mingw32-windres cmake ninja git widl; do command -v $t >/dev/null 2>&1 || echo MISSING:$t; done; cmake --version 2>/dev/null | head -1`

type Backend struct {
	Kind    string `json:"kind"` // docker, podman or wsl
	Name    string `json:"name"`
	Path    string `json:"-"`
	Distro  string `json:"distro,omitempty"`
	Windows bool   `json:"windows,omitempty"` // Docker is in Windows-container mode: usable for the VC6/Visual Studio images only
	Ready   bool   `json:"ready"`
	Problem string `json:"problem,omitempty"`
	Fix     string `json:"fix,omitempty"`
}

func (a *Agent) probeEngine(kind string) Backend {
	title := map[string]string{"docker": "Docker", "podman": "Podman"}[kind]
	b := Backend{Kind: kind, Name: title, Path: which(a.env, kind)}
	if b.Path == "" {
		b.Problem = title + " is not installed."
		b.Fix = map[string]string{
			"docker": "Install Docker Desktop (https://www.docker.com/products/docker-desktop/) or Docker Engine.",
			"podman": "Install Podman (https://podman.io/docs/installation).",
		}[kind]
		return b
	}
	format := map[string]string{"docker": "{{.OSType}}", "podman": "{{.Version.OsArch}}"}[kind]
	out, ok := a.capture(12*time.Second, kind, "info", "--format", format)
	if !ok {
		b.Problem = title + " is installed but not running."
		b.Fix = map[string]string{
			"docker": "Start Docker Desktop (or the docker service) and wait until it says it is running.",
			"podman": "Start Podman's machine: run `podman machine start` (on Linux, `systemctl --user start podman.socket`).",
		}[kind]
		return b
	}
	if kind == "docker" && strings.Contains(strings.ToLower(out), "windows") {
		b.Windows = true
		b.Problem = "Docker is set to Windows containers, but the MinGW build environment is a Linux image."
		b.Fix = "Right-click the Docker Desktop tray icon and choose \"Switch to Linux containers\"."
		return b
	}
	b.Ready = true
	return b
}

// decodeWSLList reads `wsl -l -q`, which prints UTF-16 on Windows.
func decodeWSLList(raw []byte) []string {
	text := string(raw)
	if bytes.IndexByte(raw, 0) >= 0 {
		u := make([]uint16, 0, len(raw)/2)
		for i := 0; i+1 < len(raw); i += 2 {
			u = append(u, uint16(raw[i])|uint16(raw[i+1])<<8)
		}
		text = string(utf16.Decode(u))
	}
	var out []string
	for _, l := range strings.Split(strings.ReplaceAll(text, "\r", ""), "\n") {
		l = strings.TrimSpace(strings.TrimPrefix(l, "\ufeff"))
		if l != "" && !strings.HasPrefix(l, "docker-desktop") {
			out = append(out, l)
		}
	}
	return out
}

var reDistro = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._ -]{0,63}$`)

func (a *Agent) probeWSL() []Backend {
	path := which(a.env, "wsl")
	if path == "" {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	defer cancel()
	list := exec.CommandContext(ctx, path, "-l", "-q")
	list.Env = a.env
	raw, err := list.Output()
	if err != nil {
		return []Backend{{Kind: "wsl", Name: "WSL", Problem: "WSL is installed but has no Linux distribution.", Fix: "Open a prompt and run `wsl --install`, restart, then try again."}}
	}
	var out []Backend
	for _, d := range decodeWSLList(raw) {
		if !reDistro.MatchString(d) {
			continue
		}
		b := Backend{Kind: "wsl", Name: "WSL: " + d, Path: path, Distro: d}
		o, ok := a.capture(25*time.Second, "wsl", "-d", d, "--", "sh", "-c", toolProbe)
		if !ok {
			b.Problem = "Could not run commands in this distribution."
			b.Fix = "Start it once from the Start menu, then try again."
			out = append(out, b)
			continue
		}
		missing := []string{}
		for _, l := range strings.Split(o, "\n") {
			if strings.HasPrefix(l, "MISSING:") {
				missing = append(missing, strings.TrimSpace(strings.TrimPrefix(l, "MISSING:")))
			}
		}
		if m := reCMakeVer.FindStringSubmatch(o); m != nil && !versionAtLeast([3]int{atoi(m[1]), atoi(m[2]), atoi(m[3])}, [3]int{3, 28, 0}) {
			missing = append(missing, "cmake 3.28 or newer")
		}
		if len(missing) > 0 {
			b.Problem = "This distribution is missing: " + strings.Join(missing, ", ") + "."
			b.Fix = "Open it and run: sudo apt-get update && sudo apt-get install -y gcc-mingw-w64-i686 g++-mingw-w64-i686 binutils-mingw-w64-i686 cmake ninja-build git wine64-tools libwine-dev"
		} else {
			b.Ready = true
		}
		out = append(out, b)
	}
	if len(out) == 0 {
		out = []Backend{{Kind: "wsl", Name: "WSL", Problem: "WSL has no Linux distribution installed.", Fix: "Run `wsl --install` in a prompt, restart, then try again."}}
	}
	return out
}

// Backends probes Docker, Podman and (on Windows) WSL.
func (a *Agent) Backends() []Backend {
	out := []Backend{a.probeEngine("docker"), a.probeEngine("podman")}
	if a.GOOS == "windows" {
		out = append(out, a.probeWSL()...)
	}
	return out
}

// pick chooses where a MinGW build runs: the first ready backend, or the one the user asked for with --backend.
func pickBackend(bs []Backend, pref string) *Backend {
	for i := range bs {
		b := &bs[i]
		if !b.Ready {
			continue
		}
		switch {
		case pref == "" || pref == "auto", pref == b.Kind, pref == "wsl:"+b.Distro:
			return b
		}
	}
	return nil
}

var reWinPath = regexp.MustCompile(`^([A-Za-z]):[\\/](.*)$`)

// wslPath converts C:\a\b to /mnt/c/a/b.
func wslPath(p string) string {
	if m := reWinPath.FindStringSubmatch(p); m != nil {
		return "/mnt/" + strings.ToLower(m[1]) + "/" + strings.ReplaceAll(m[2], `\`, "/")
	}
	return strings.ReplaceAll(p, `\`, "/")
}

// mingwLinkFlags make a MinGW-w64 build carry its own thread and C++ runtime, so the exe does not need
// libwinpthread-1.dll (or libstdc++-6.dll, libgcc_s_dw2-1.dll) next to it. CMake reads LDFLAGS when it first configures a build folder.
// --allow-multiple-definition: Microsoft's d3dx8.lib and MinGW's libdxguid.a both define the same DirectX GUID constants
// (identical values); GNU ld refuses the duplicates, lld does not.
func mingwLinkFlags(root string) string {
	return "-static -Wl,--allow-multiple-definition -L" + root + "/" + dx8OverrideDir
}

// mingwStamp is written into a build folder so that one configured with other flags or another compiler is started afresh.
const mingwStamp = "+static3"

// isMingwImage is true for the MinGW-w64 build environment images (not the VC6 / Visual Studio ones).
func isMingwImage(img string) bool {
	return strings.Contains(img, "generals-portal-mingw") || strings.Contains(img, "generals-portal-clang")
}

// wrap turns "run this tool in dir" into the command that runs it inside the backend.
func (a *Agent) wrap(b Backend, dir, name string, args []string) (string, []string, error) {
	return a.wrapImage(b, a.image(), false, dir, name, args)
}

// wrapImage is wrap for a specific image. windows means a Windows container image (Docker in Windows-container mode).
func (a *Agent) wrapImage(b Backend, img string, windows bool, dir, name string, args []string) (string, []string, error) {
	switch b.Kind {
	case "docker", "podman":
		if windows {
			// Hyper-V isolation, so the image does not have to match this PC's exact Windows build.
			argv := []string{"run", "--rm", "--isolation=hyperv", "--mount", "type=bind,source=" + dir + `,target=C:\src`, "-w", `C:\src`, img, name}
			return b.Path, append(argv, args...), nil
		}
		argv := []string{"run", "--rm", "--init", "--mount", "type=bind,source=" + dir + ",target=/src", "-w", "/src", "-e", "HOME=/tmp"}
		if a.GOOS != "windows" && a.GOOS != "darwin" { // keep files on the host owned by the user
			argv = append(argv, "--user", fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid()))
		}
		if b.Kind == "podman" && a.GOOS == "linux" {
			argv = append(argv, "--security-opt", "label=disable")
		}
		if isMingwImage(img) {
			argv = append(argv, "-e", "LDFLAGS="+mingwLinkFlags("/src"))
		}
		argv = append(argv, img, name)
		return b.Path, append(argv, args...), nil
	case "wsl": // WSL only ever runs the MinGW-w64 build
		return b.Path, append([]string{"-d", b.Distro, "--cd", wslPath(dir), "--", "env", "LDFLAGS=" + mingwLinkFlags(wslPath(dir)), name}, args...), nil
	}
	return "", nil, fmt.Errorf("unknown backend %q", b.Kind)
}

func (a *Agent) containerRunner(b Backend) Runner {
	return a.containerRunnerImage(b, a.image(), false)
}

func (a *Agent) containerRunnerImage(b Backend, img string, windows bool) Runner {
	return func(ctx context.Context, dir string, log io.Writer, name string, args ...string) error {
		exe, argv, err := a.wrapImage(b, img, windows, dir, name, args)
		if err != nil {
			return err
		}
		fmt.Fprintf(log, "\n$ %s %s\n", exe, strings.Join(argv, " "))
		cmd := exec.CommandContext(ctx, exe, argv...)
		cmd.Env, cmd.Stdout, cmd.Stderr = a.env, log, log
		return cmd.Run()
	}
}

func (a *Agent) nativeRunner() Runner {
	return func(ctx context.Context, dir string, log io.Writer, name string, args ...string) error {
		p := which(a.env, name)
		if p == "" {
			return fmt.Errorf("%s was not found on PATH", name)
		}
		fmt.Fprintf(log, "\n$ %s %s\n", name, strings.Join(args, " "))
		cmd := exec.CommandContext(ctx, p, args...)
		cmd.Dir, cmd.Env, cmd.Stdout, cmd.Stderr = dir, a.env, log, log
		return cmd.Run()
	}
}

func (a *Agent) image() string {
	if a.Image == "" {
		return defaultImage
	}
	return a.Image
}

// imageReady reports whether the toolchain image exists locally.
func (a *Agent) imageReady(b Backend) bool {
	_, ok := a.capture(20*time.Second, b.Kind, "image", "inspect", a.image())
	return ok
}

// ensureImage builds the shipped toolchain image (or pulls a custom one) the first time it is needed.
func (a *Agent) ensureImage(ctx context.Context, b Backend, log io.Writer) error {
	if b.Kind == "wsl" || a.imageReady(b) {
		return nil
	}
	var cmd *exec.Cmd
	if a.image() == defaultImage {
		fmt.Fprintf(log, "\nBuilding the MinGW-w64 build environment (first time only; needs internet, takes several minutes)\n")
		cmd = exec.CommandContext(ctx, b.Path, "build", "-t", defaultImage, "-")
		cmd.Stdin = strings.NewReader(toolchainDockerfile)
	} else {
		fmt.Fprintf(log, "\nPulling %s\n", a.image())
		cmd = exec.CommandContext(ctx, b.Path, "pull", a.image())
	}
	cmd.Env, cmd.Stdout, cmd.Stderr = a.env, log, log
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("could not prepare the build environment image %q: %v", a.image(), err)
	}
	return nil
}

// CheckToolchain runs the tool probe inside the backend and returns what is missing (empty = ready).
func (a *Agent) CheckToolchain(ctx context.Context, b Backend) ([]string, string, error) {
	var out bytes.Buffer
	spec, _ := a.mingwImage()
	run := a.containerRunnerImage(b, spec.Name, false)
	if err := run(ctx, a.srcFor(""), &out, "sh", "-c", toolProbe); err != nil {
		return nil, "", fmt.Errorf("%v\n%s", err, out.String())
	}
	var missing []string
	ver := ""
	for _, l := range strings.Split(out.String(), "\n") {
		if strings.HasPrefix(l, "MISSING:") {
			missing = append(missing, strings.TrimSpace(strings.TrimPrefix(l, "MISSING:")))
		}
		if strings.HasPrefix(l, "cmake version") {
			ver = strings.TrimSpace(l)
		}
	}
	return missing, ver, nil
}

func (a *Agent) doctorContainer(ctx context.Context, rep *Report) {
	add := func(c Check) { rep.Checks = append(rep.Checks, c) }
	bs := a.Backends()
	for _, b := range bs {
		st := "info"
		if b.Ready {
			st = "ok"
		}
		add(Check{ID: b.Kind, Name: b.Name, Status: st, Detail: b.Problem, Fix: b.Fix})
	}
	b := pickBackend(bs, a.BackendPref)
	if b == nil {
		add(Check{ID: "backend", Name: "A place to run the build", Status: "fail",
			Detail: "The MinGW-w64 build runs in a container or WSL, and none is ready on this computer.",
			Fix:    "Install and start Docker Desktop or Podman, or on Windows set up WSL (`wsl --install`). Details for each are above."})
		return
	}
	rep.Backend = b.Kind
	if b.Distro != "" {
		rep.Backend = "wsl:" + b.Distro
	}
	add(Check{ID: "backend", Name: "Build runs in", Status: "ok", Detail: b.Name})
	if b.Kind != "wsl" {
		if spec, _ := a.mingwImageFor(rep.Toolchain); a.imageExists(*b, spec.Name) {
			add(Check{ID: "image", Name: "Build environment image", Status: "ok", Detail: spec.Name})
		} else {
			add(Check{ID: "image", Name: "Build environment image", Status: "warn", Detail: "It is built automatically the first time (needs internet, takes several minutes). Run `build-agent --check-image` to do it now and verify it."})
		}
	}
}
