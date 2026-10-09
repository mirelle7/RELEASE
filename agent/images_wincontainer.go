package main

import _ "embed"

// Windows container images: the real tools (Visual C++ 6 from the package TheSuperHackers' CI uses, and the Visual
// Studio Build Tools) in a Windows Server Core image. They run on Docker in Windows-container mode, with
// --isolation=hyperv, and are built on the player's PC the first time they are needed (after --accept-licenses).
// Neither recipe has been built yet.

//go:embed toolchain/Dockerfile.vc6.windows
var dockerfileVC6Win string

//go:embed toolchain/Dockerfile.msvc.windows
var dockerfileMSVCWin string

const (
	winImageVC6  = "generals-portal-vc6-win:1"
	winImageMSVC = "generals-portal-msvc-win:1"
)

var winContainerImages = map[string]imageSpec{
	"vc6":  {Name: winImageVC6, Dockerfile: dockerfileVC6Win, Windows: true},
	"msvc": {Name: winImageMSVC, Dockerfile: dockerfileMSVCWin, Windows: true},
}
