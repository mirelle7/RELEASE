package main

import (
	"context"
	_ "embed"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// The Visual C++ 6 and Visual Studio builds link the game against Microsoft's static d3dx8.lib (from
// TheSuperHackers/min-dx8-sdk, which CMake downloads), so the retail exe needs no D3DX DLL. MinGW-w64 has only the
// debug *import* library (libd3dx8d.a), so a MinGW exe would demand d3dx8d.dll, which ships with the DirectX SDK and is
// not on a player's PC. To avoid that without touching the game's source, the agent puts that same static d3dx8.lib
// (plus a tiny object with the few Microsoft runtime helpers it needs) where the linker finds it before libd3dx8d.a.

//go:embed toolchain/gpa_msvc_compat.cpp
var dx8ShimSource []byte

// dx8OverrideDir is the folder (inside the project, next to the per-preset build folders) searched first by the MinGW link.
const dx8OverrideDir = "build/gpa-override"

// prepareDX8 builds the override library once the preset has been configured (configuring is what downloads min-dx8-sdk).
// It never fails a build: if anything goes wrong the override is removed and the link falls back to libd3dx8d.a.
func (a *Agent) prepareDX8(ctx context.Context, run Runner, src, preset string, log io.Writer) {
	lib := filepath.Join(src, "build", preset, "_deps", "dx8-src", "d3dx8.lib")
	if _, err := os.Stat(lib); err != nil {
		fmt.Fprintln(log, "\n-- min-dx8-sdk's static d3dx8.lib not found: the exe will need d3dx8d.dll next to it")
		return
	}
	dir := filepath.Join(src, filepath.FromSlash(dx8OverrideDir))
	fail := func(why error) {
		fmt.Fprintf(log, "\n-- could not set up the static D3DX8 library (%v): the exe will need d3dx8d.dll next to it\n", why)
		_ = os.RemoveAll(dir)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		fail(err)
		return
	}
	if err := os.WriteFile(filepath.Join(dir, "gpa_msvc_compat.cpp"), dx8ShimSource, 0o644); err != nil {
		fail(err)
		return
	}
	script := fmt.Sprintf(`set -e
cd %s
rm -f libd3dx8d.a liblibci.a gpa_msvc_compat.o
cp ../%s/_deps/dx8-src/d3dx8.lib libd3dx8d.a
i686-w64-mingw32-g++ -O2 -c gpa_msvc_compat.cpp -o gpa_msvc_compat.o
ar r libd3dx8d.a gpa_msvc_compat.o
ar rc liblibci.a`, dx8OverrideDir, preset)
	fmt.Fprintln(log, "\n-- linking Microsoft's static D3DX8 (from min-dx8-sdk), as the Visual C++ and Visual Studio builds do")
	if err := run(ctx, src, log, "sh", "-c", script); err != nil {
		fail(err)
	}
}
