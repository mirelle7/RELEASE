package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Check is one line of the "can this PC build this?" report.
type Check struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Status string `json:"status"` // ok, warn (builds anyway), fail (blocks the build), info
	Detail string `json:"detail,omitempty"`
	Fix    string `json:"fix,omitempty"`
}

type Report struct {
	Preset  string  `json:"preset"`
	Family  string  `json:"family"`  // vc6, msvc or mingw
	Backend string  `json:"backend"` // where the build will run: native, docker, podman, wsl
	Checks  []Check `json:"checks"`
	Ready   bool    `json:"ready"`
	Skipped bool    `json:"skipped,omitempty"` // checks were turned off with --skip-checks
}

func (r Report) Summary() string {
	var lines []string
	for _, c := range r.Checks {
		if c.Status == "fail" {
			s := c.Name + ": " + c.Detail
			if c.Fix != "" {
				s += " " + c.Fix
			}
			lines = append(lines, s)
		}
	}
	return "this PC is not ready for this build. " + strings.Join(lines, " | ")
}

func familyOf(preset string) string {
	switch {
	case strings.HasPrefix(preset, "vc6"):
		return "vc6"
	case strings.HasPrefix(preset, "win32"):
		return "msvc"
	case strings.HasPrefix(preset, "mingw"):
		return "mingw"
	}
	return "other"
}

var (
	reCMakeVer = regexp.MustCompile(`cmake version (\d+)\.(\d+)(?:\.(\d+))?`)
	reCLBanner = regexp.MustCompile(`Version (\d+)\.(\d+)\.(\d+)(?: for (\S+))?`)
)

// capture runs a program from env's PATH and returns its combined output (errors are not fatal: cl exits non-zero).
func (a *Agent) capture(timeout time.Duration, name string, args ...string) (string, bool) {
	p := which(a.env, name)
	if p == "" {
		return "", false
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, p, args...)
	cmd.Env = a.env
	var buf bytes.Buffer
	cmd.Stdout, cmd.Stderr = &buf, &buf
	err := cmd.Run()
	return buf.String(), err == nil
}

// presetInfo reads the generator a preset uses (following "inherits") and the CMake version the project asks for.
func (a *Agent) presetInfo(src, preset string) (generator string, min [3]int) {
	raw, err := os.ReadFile(filepath.Join(src, "CMakePresets.json"))
	if err != nil {
		return "", min
	}
	var doc struct {
		Min       struct{ Major, Minor, Patch int } `json:"cmakeMinimumRequired"`
		Configure []struct {
			Name      string          `json:"name"`
			Generator string          `json:"generator"`
			Inherits  json.RawMessage `json:"inherits"`
		} `json:"configurePresets"`
	}
	if json.Unmarshal(raw, &doc) != nil {
		return "", min
	}
	min = [3]int{doc.Min.Major, doc.Min.Minor, doc.Min.Patch}
	byName := map[string]int{}
	for i, p := range doc.Configure {
		byName[p.Name] = i
	}
	var find func(name string, depth int) string
	find = func(name string, depth int) string {
		i, ok := byName[name]
		if !ok || depth > 8 {
			return ""
		}
		p := doc.Configure[i]
		if p.Generator != "" {
			return p.Generator
		}
		var parents []string
		if json.Unmarshal(p.Inherits, &parents) != nil {
			var one string
			if json.Unmarshal(p.Inherits, &one) == nil && one != "" {
				parents = []string{one}
			}
		}
		for _, par := range parents {
			if g := find(par, depth+1); g != "" {
				return g
			}
		}
		return ""
	}
	return find(preset, 0), min
}

func versionAtLeast(have, want [3]int) bool {
	for i := range have {
		if have[i] != want[i] {
			return have[i] > want[i]
		}
	}
	return true
}

func wingetHint(a *Agent, id string) string {
	if which(a.env, "winget") != "" {
		return fmt.Sprintf("Install it with: winget install --id %s -e, then open a new prompt.", id)
	}
	return "Install it from its website, then open a new prompt."
}

// Doctor checks whether this PC can build the preset, and says exactly what is missing and how to fix it.
func (a *Agent) Doctor(ctx context.Context, preset string) Report {
	fam := familyOf(preset)
	rep := Report{Preset: preset, Family: fam, Backend: "native", Skipped: a.SkipChecks}
	add := func(c Check) { rep.Checks = append(rep.Checks, c) }

	if fam == "mingw" {
		a.doctorContainer(ctx, &rep)
	} else {
		a.doctorNative(&rep, fam)
	}
	rep.Ready = a.SkipChecks
	if !rep.Ready {
		rep.Ready = true
		for _, c := range rep.Checks {
			if c.Status == "fail" {
				rep.Ready = false
			}
		}
	}
	_ = add
	return rep
}

type docEntry struct {
	at  time.Time
	rep Report
}

// DoctorCached avoids re-probing Docker/WSL for every step of a long range build.
func (a *Agent) DoctorCached(ctx context.Context, preset string) Report {
	a.docMu.Lock()
	e, ok := a.docCache[preset]
	a.docMu.Unlock()
	if ok && time.Since(e.at) < 30*time.Second {
		return e.rep
	}
	rep := a.Doctor(ctx, preset)
	a.docMu.Lock()
	a.docCache[preset] = docEntry{time.Now(), rep}
	a.docMu.Unlock()
	return rep
}

func printReport(rep Report) {
	mark := map[string]string{"ok": "[ok]  ", "warn": "[warn]", "fail": "[FAIL]", "info": "[ -- ]"}
	for _, c := range rep.Checks {
		line := fmt.Sprintf("  %s %s", mark[c.Status], c.Name)
		if c.Detail != "" {
			line += ": " + c.Detail
		}
		fmt.Println(line)
		if c.Fix != "" && c.Status != "ok" {
			fmt.Println("         ->", c.Fix)
		}
	}
}

func (a *Agent) printStartupReport() {
	known, err := a.presets()
	if err != nil {
		return
	}
	fmt.Println("\nChecking this PC:")
	for _, p := range []string{"vc6", "win32", "mingw-w64-i686"} {
		if !known[p] {
			continue
		}
		rep := a.Doctor(context.Background(), p)
		verdict := "ready"
		if !rep.Ready {
			verdict = "NOT ready"
		}
		fmt.Printf("\n %s builds (%s): %s\n", map[string]string{"vc6": "Visual C++ 6", "msvc": "Modern Visual Studio", "mingw": "MinGW-w64"}[rep.Family], p, verdict)
		printReport(rep)
	}
	fmt.Println()
}

// runCheckImage builds the MinGW build environment and verifies that every tool is in it.
func (a *Agent) runCheckImage() int {
	bs := a.Backends()
	for _, b := range bs {
		fmt.Printf("%-18s ready=%v %s %s\n", b.Name, b.Ready, b.Problem, b.Fix)
	}
	b := pickBackend(bs, a.BackendPref)
	if b == nil {
		fmt.Println("No container engine or WSL distribution is ready, so there is nothing to check.")
		return 1
	}
	fmt.Println("Using", b.Name)
	if err := a.ensureImage(context.Background(), *b, os.Stdout); err != nil {
		fmt.Println("FAILED:", err)
		return 1
	}
	missing, ver, err := a.CheckToolchain(context.Background(), *b)
	if err != nil {
		fmt.Println("FAILED:", err)
		return 1
	}
	if len(missing) > 0 {
		fmt.Println("FAILED: the build environment is missing:", strings.Join(missing, ", "))
		return 1
	}
	fmt.Println("OK: all MinGW-w64 build tools are present.", ver)
	return 0
}

func (a *Agent) doctorNative(rep *Report, fam string) {
	add := func(c Check) { rep.Checks = append(rep.Checks, c) }
	setup := map[string]string{
		"vc6":  `Run the agent from a prompt where Visual C++ 6 is set up (run its VCVARS32.BAT first), or start the agent with --env-script "C:\path\to\VCVARS32.BAT".`,
		"msvc": `Run the agent from the "x86 Native Tools Command Prompt for VS" in the Start menu, or start it with --env-script "C:\...\VC\Auxiliary\Build\vcvarsall.bat" --env-args x86.`,
	}[fam]

	if a.GOOS != "windows" {
		add(Check{ID: "os", Name: "Windows", Status: "fail",
			Detail: "These builds need Windows and Microsoft's compiler, and this computer is not running Windows.",
			Fix:    "Use a Windows PC, or pick the MinGW-w64 compiler, which builds in a container (Docker, Podman or WSL). You can still choose switches here and copy the cmake command."})
		return
	}
	add(Check{ID: "os", Name: "Windows", Status: "ok"})

	// CMake, at least the version the project asks for
	gen, min := a.presetInfo(a.Src, rep.Preset)
	if min == [3]int{} {
		min = [3]int{3, 28, 0}
	}
	if out, ok := a.capture(15*time.Second, "cmake", "--version"); !ok && which(a.env, "cmake") == "" {
		add(Check{ID: "cmake", Name: "CMake", Status: "fail", Detail: "CMake was not found.", Fix: wingetHint(a, "Kitware.CMake")})
	} else if m := reCMakeVer.FindStringSubmatch(out); m == nil {
		add(Check{ID: "cmake", Name: "CMake", Status: "warn", Detail: "Found CMake but could not read its version."})
	} else {
		have := [3]int{atoi(m[1]), atoi(m[2]), atoi(m[3])}
		if !versionAtLeast(have, min) {
			add(Check{ID: "cmake", Name: "CMake", Status: "fail", Detail: fmt.Sprintf("CMake %d.%d.%d is too old; this project needs %d.%d.%d or newer.", have[0], have[1], have[2], min[0], min[1], min[2]), Fix: wingetHint(a, "Kitware.CMake")})
		} else {
			add(Check{ID: "cmake", Name: "CMake", Status: "ok", Detail: fmt.Sprintf("%d.%d.%d", have[0], have[1], have[2])})
		}
	}

	if strings.HasPrefix(gen, "Ninja") {
		if which(a.env, "ninja") == "" {
			add(Check{ID: "ninja", Name: "Ninja", Status: "fail", Detail: "This preset uses the Ninja generator, and Ninja was not found.", Fix: wingetHint(a, "Ninja-build.Ninja")})
		} else {
			add(Check{ID: "ninja", Name: "Ninja", Status: "ok"})
		}
	}

	if which(a.env, "git") == "" {
		add(Check{ID: "git", Name: "Git", Status: "fail", Detail: "Git was not found. CMake uses it to download the project's dependencies, and range builds need it.", Fix: wingetHint(a, "Git.Git")})
	} else {
		add(Check{ID: "git", Name: "Git", Status: "ok", Detail: "The first build also needs internet access to download dependencies."})
	}

	// The compiler CMake will actually pick: environment overrides first, then cl.exe on PATH
	for _, k := range []string{"CC", "CXX", "CMAKE_C_COMPILER", "CMAKE_CXX_COMPILER"} {
		if v := envGet(a.env, k); v != "" {
			base := strings.TrimSuffix(strings.ToLower(filepath.Base(strings.ReplaceAll(v, `\`, "/"))), ".exe")
			if base != "cl" {
				add(Check{ID: "compiler-env", Name: "Compiler override", Status: "fail",
					Detail: fmt.Sprintf("%s is set to %q, so CMake would use that instead of Microsoft's cl.exe.", k, v),
					Fix:    fmt.Sprintf("Remove it for this session (in cmd: set %s=) and start the agent again.", k)})
			}
		}
	}
	banner, _ := a.capture(15*time.Second, "cl")
	m := reCLBanner.FindStringSubmatch(banner)
	switch {
	case which(a.env, "cl") == "":
		add(Check{ID: "compiler", Name: map[string]string{"vc6": "Visual C++ 6", "msvc": "Visual Studio C++"}[fam], Status: "fail", Detail: "cl.exe (Microsoft's compiler) was not found on PATH.", Fix: setup})
	case m == nil:
		add(Check{ID: "compiler", Name: "Compiler", Status: "warn", Detail: "Found cl.exe but could not read its version."})
	default:
		maj, minr, build := atoi(m[1]), atoi(m[2]), atoi(m[3])
		arch := m[4]
		ver := fmt.Sprintf("%d.%02d.%d", maj, minr, build)
		switch fam {
		case "vc6":
			if maj != 12 {
				add(Check{ID: "compiler", Name: "Visual C++ 6", Status: "fail", Detail: fmt.Sprintf("This build needs Visual C++ 6 (12.00), but the compiler on PATH is %s.", ver), Fix: setup})
			} else if build != 8804 {
				add(Check{ID: "compiler", Name: "Visual C++ 6", Status: "warn", Detail: ver + " builds, but only 12.00.8804 (Service Pack 6) is CRC-compatible with retail."})
			} else {
				add(Check{ID: "compiler", Name: "Visual C++ 6", Status: "ok", Detail: ver + " (SP6)"})
			}
		case "msvc":
			if maj < 19 {
				add(Check{ID: "compiler", Name: "Visual Studio C++", Status: "fail", Detail: fmt.Sprintf("This build needs a modern Visual Studio compiler (19.x), but the compiler on PATH is %s.", ver), Fix: setup})
			} else if arch != "" && arch != "x86" {
				add(Check{ID: "compiler", Name: "Visual Studio C++", Status: "fail", Detail: fmt.Sprintf("The game is built as 32-bit, but this compiler targets %s.", arch), Fix: setup})
			} else {
				add(Check{ID: "compiler", Name: "Visual Studio C++", Status: "ok", Detail: ver + " " + arch})
			}
		}
	}

	if which(a.env, "midl") == "" {
		add(Check{ID: "midl", Name: "MIDL (midl.exe)", Status: "fail", Detail: "midl.exe was not found. The project needs it to build EABrowserDispatch.", Fix: "It comes with the compiler tools and is on PATH once the compiler is set up. " + setup})
	} else {
		add(Check{ID: "midl", Name: "MIDL (midl.exe)", Status: "ok"})
	}
}

func atoi(s string) int { n, _ := strconv.Atoi(s); return n }

// explainFailure turns a CMake/build log into plain-English reasons, so a failure says why instead of dumping output.
func explainFailure(fam, log string) []string {
	var out []string
	add := func(s string) {
		for _, o := range out {
			if o == s {
				return
			}
		}
		out = append(out, s)
	}
	if m := regexp.MustCompile(`The C(?:XX)? compiler identification is (\w+)`).FindStringSubmatch(log); m != nil && (fam == "vc6" || fam == "msvc") && m[1] != "MSVC" {
		add(fmt.Sprintf("CMake used %s instead of Microsoft's compiler, so cl.exe is not set up for this session. Start the agent from a developer prompt, or with --env-script.", m[1]))
	}
	switch {
	case strings.Contains(log, "requires an IDL compiler"):
		add("midl.exe was not found. It comes with the compiler tools; set the compiler up first (developer prompt or --env-script).")
	}
	if regexp.MustCompile(`No CMAKE_(C|CXX)_COMPILER could be found|is not a full path to an existing compiler`).MatchString(log) {
		add("CMake could not find a compiler. Set the compiler up first (developer prompt or --env-script).")
	}
	if regexp.MustCompile(`unable to find a build program corresponding to "Ninja"|CMAKE_MAKE_PROGRAM is not set`).MatchString(log) {
		add("Ninja was not found. Install it (winget install --id Ninja-build.Ninja -e) and open a new prompt.")
	}
	if regexp.MustCompile(`(?i)could not find git|git: command not found|Could NOT find Git`).MatchString(log) {
		add("Git was not found, and CMake needs it to download dependencies.")
	}
	if regexp.MustCompile(`(?i)could not resolve host|unable to access 'https?://|failed to connect|connection timed out|network is unreachable`).MatchString(log) {
		add("A download failed. The first build needs internet access to fetch dependencies; check the connection or any proxy and try again.")
	}
	return out
}

// readTail returns the last ~256 KB of a file.
func readTail(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	st, _ := f.Stat()
	const max = 256 * 1024
	if st != nil && st.Size() > max {
		_, _ = f.Seek(-max, 2)
	}
	b := new(bytes.Buffer)
	_, _ = b.ReadFrom(f)
	return b.String()
}
