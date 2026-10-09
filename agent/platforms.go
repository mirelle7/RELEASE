package main

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
)

// Platform is one way of building the game that the site offers (a compiler) and whether this PC can do it right now.
type Platform struct {
	ID      string  `json:"id"` // the compiler id used by the site (matrix.yaml)
	Name    string  `json:"name"`
	Preset  string  `json:"preset"` // the Release preset the check ran for
	Ready   bool    `json:"ready"`
	Backend string  `json:"backend"` // where it would run: native, docker, podman, wsl
	Image   string  `json:"image,omitempty"`
	Problem string  `json:"problem,omitempty"` // first thing that is missing, with how to fix it
	Report  *Report `json:"report"`
}

var platformList = []struct{ id, name, preset, toolchain string }{
	{"vc6", "Visual C++ 6 SP6", "vc6", ""},
	{"msvc", "Modern MSVC (VS2022)", "win32", ""},
	{"mingw-clang", "MinGW-w64 + Clang (container)", "mingw-w64-i686", "clang"},
	{"mingw-gcc", "MinGW-w64 + GCC (container)", "mingw-w64-i686", "gcc"},
	{"linux", "Linux (native)", "unix", ""},
	{"macos", "macOS (Apple silicon, native)", "macos", ""},
}

// platformPreset is the preset to check a platform with: for the native ones, the project's own first preset of that family.
func (a *Agent) platformPreset(project, id, def string) string {
	if id != "linux" && id != "macos" {
		return def
	}
	if dir := a.presetSrc(project); dir != "" {
		if names, err := readPresetNames(dir); err == nil {
			for _, n := range names {
				if familyOf(n) == id {
					return n
				}
			}
		}
	}
	return def
}

// Platforms checks every platform at the same time (each check may talk to Docker, Podman or WSL, which is slow).
func (a *Agent) Platforms(ctx context.Context) []Platform { return a.PlatformsFor(ctx, "") }

// PlatformsFor checks the platforms against one project's source folder.
func (a *Agent) PlatformsFor(ctx context.Context, project string) []Platform {
	out := make([]Platform, len(platformList))
	var wg sync.WaitGroup
	for i, p := range platformList {
		wg.Add(1)
		go func(i int, id, name, preset, tc string) {
			defer wg.Done()
			preset = a.platformPreset(project, id, preset)
			rep := a.DoctorCachedProj(ctx, preset, tc, project)
			pl := Platform{ID: id, Name: name, Preset: preset, Ready: rep.Ready, Backend: rep.Backend, Image: rep.Image, Report: &rep}
			for _, c := range rep.Checks {
				if c.Status == "fail" {
					pl.Problem = c.Name + ": " + c.Detail
					if c.Fix != "" {
						pl.Problem += " " + c.Fix
					}
					break
				}
			}
			out[i] = pl
		}(i, p.id, p.name, p.preset, p.toolchain)
	}
	wg.Wait()
	return out
}

func (a *Agent) handlePlatforms(w http.ResponseWriter, r *http.Request) {
	project := r.URL.Query().Get("project")
	if project != "" && !reProject.MatchString(project) {
		writeJSON(w, 400, map[string]string{"error": "bad project id"})
		return
	}
	writeJSON(w, 200, map[string]any{"platforms": a.PlatformsFor(r.Context(), project)})
}

// handleLog returns a piece of a build's log for the live task view: ?id=<build>&from=<byte offset>.
// Without "from" (or with a negative one) it returns the last 16 KB. "next" is where to continue from.
func (a *Agent) handleLog(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("id")
	if !reID.MatchString(id) {
		writeJSON(w, 400, map[string]string{"error": "bad id"})
		return
	}
	f, err := os.Open(filepath.Join(a.Out, id, "build.log"))
	if err != nil {
		writeJSON(w, 404, map[string]string{"error": "no log for that build"})
		return
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	const tail = 16 << 10
	from, perr := strconv.ParseInt(r.URL.Query().Get("from"), 10, 64)
	if perr != nil || from < 0 || from > st.Size() || st.Size()-from > 256<<10 {
		from = max(0, st.Size()-tail)
	}
	buf := make([]byte, st.Size()-from)
	n, _ := f.ReadAt(buf, from)
	a.mu.Lock()
	running := a.current == id
	a.mu.Unlock()
	writeJSON(w, 200, map[string]any{"text": strings.ToValidUTF8(string(buf[:n]), "?"), "from": from, "next": from + int64(n), "running": running})
}

// prepareBuildDir keeps one build folder from being shared by two MinGW compilers: CMake refuses (or silently mixes)
// a cache made by another compiler, so when the compiler changes the folder is cleared and stamped.
func prepareBuildDir(src, preset, toolchain string) error {
	dir := filepath.Join(src, "build", preset)
	stamp := filepath.Join(dir, ".portal-toolchain")
	if old, err := os.ReadFile(stamp); err == nil && strings.TrimSpace(string(old)) != toolchain {
		if err := os.RemoveAll(dir); err != nil {
			return err
		}
	}
	if _, err := os.Stat(dir); os.IsNotExist(err) {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	return os.WriteFile(stamp, []byte(toolchain+"\n"), 0o644)
}
