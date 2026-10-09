package main

import _ "embed"

// Linux images that run the Windows compilers under Wine. They work with Docker (Linux containers), Podman and WSL.
// Both recipes are untested: see the NEVER BUILT notes at the top of each Dockerfile.
// Inside each image, /usr/local/bin/toolprobe prints MISSING:<tool> for anything absent (cmake ninja git cl link rc midl),
// then the cmake version and the cl banner, so the agent can check the image the same way it checks the MinGW one.

//go:embed toolchain/Dockerfile.vc6.linux
var dockerfileVC6Wine string

//go:embed toolchain/Dockerfile.msvc.linux
var dockerfileMSVCWine string

var wineImages = map[string]imageSpec{
	"vc6":  {Name: "generals-portal-vc6-wine:3", Dockerfile: dockerfileVC6Wine, Windows: false},
	"msvc": {Name: "generals-portal-msvc-wine:1", Dockerfile: dockerfileMSVCWine, Windows: false},
}
