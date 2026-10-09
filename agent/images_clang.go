package main

import _ "embed"

// The "modern" build: the mingw-w64-i686 presets compiled with Clang, in a Linux container. Recipe: toolchain/Dockerfile.clang.
// It is a Linux image, like the GCC MinGW one; the parent decides where it is wired in (backend, doctor, site).

//go:embed toolchain/Dockerfile.clang
var dockerfileClang string

const clangImageName = "generals-portal-clang:1"

var clangImage = imageSpec{Name: clangImageName, Dockerfile: dockerfileClang, Windows: false}
