package main

import (
	"regexp"
	"strings"
	"testing"
)

func TestClangImageSpec(t *testing.T) {
	if clangImageName != "generals-portal-clang:1" {
		t.Fatalf("name constant: %q", clangImageName)
	}
	if clangImage.Name != clangImageName {
		t.Fatalf("name: %q", clangImage.Name)
	}
	if clangImage.Windows {
		t.Fatal("the Clang image is a Linux image")
	}
	if strings.TrimSpace(clangImage.Dockerfile) == "" || clangImage.Dockerfile != dockerfileClang {
		t.Fatal("recipe is empty or not the embedded one")
	}
}

func TestClangRecipeHasWhatTheBuildNeeds(t *testing.T) {
	d := dockerfileClang
	for _, want := range []string{
		"safe.directory",              // git trusts the bind-mounted source folder
		"clang lld",                   // the compiler and linker packages
		"widl",                        // the IDL compiler EABrowserDispatch needs
		"wine64-tools", "libwine-dev", // widl and the Wine IDL headers
		"mingw-w64-i686-dev",            // the sysroot
		"ENV CC=i686-w64-mingw32-clang", // CC and CXX point at the wrappers
		"CXX=i686-w64-mingw32-clang++",
		"--target=i686-w64-mingw32",
		"--gcc-install-dir", // pins the posix-threads libstdc++
		"-fuse-ld=lld",
		"i686-w64-mingw32-gcc", // the toolchain file's hard-coded names resolve to Clang
		"i686-w64-mingw32-g++",
		"/usr/local/bin/toolprobe", "MISSING:",
		"BUILT AND VERIFIED",
	} {
		if !strings.Contains(d, want) {
			t.Errorf("recipe lacks %q", want)
		}
	}
}

func TestClangRecipeHasNoPlaceholders(t *testing.T) {
	if m := regexp.MustCompile(`@[A-Z_]+@|\{\{|\}\}`).FindString(dockerfileClang); m != "" {
		t.Fatalf("unreplaced placeholder %q", m)
	}
}
