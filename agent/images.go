package main

// Build-environment images for Visual C++ 6 and Visual Studio, in two flavours per compiler:
//   - a Linux image that runs the Windows compilers under Wine (works with Docker, Podman and WSL), and
//   - a Windows container image with the real tools (needs Docker in Windows-container mode).
// MinGW-w64 keeps its own image (backend.go). Images are built on the player's PC from these recipes the first time
// they are needed; nothing from Microsoft is shipped in this program.

// wineParallel is how many compiler processes run at once under Wine. The C1060 failure turned out to come from
// precompiled headers, not from the job count (it also failed with 2), so this is back to a high number.
const wineParallel = 8

// imageSpec says how to build one image.
type imageSpec struct {
	Name       string // image tag
	Dockerfile string // the recipe, fed to `build -t Name -`
	Windows    bool   // a Windows container image (runs on Docker in Windows-container mode)
}

// wineImages (images_wine.go) and winContainerImages (images_wincontainer.go) are keyed by compiler family: "vc6", "msvc".

// imageForFamily picks the image for a compiler family on a backend. windowsMode means Docker is in Windows-container mode.
func imageForFamily(family string, windowsMode bool) (imageSpec, bool) {
	var s imageSpec
	var ok bool
	if windowsMode {
		s, ok = winContainerImages[family]
	} else {
		s, ok = wineImages[family]
	}
	return s, ok
}

// Microsoft's licence terms apply to the compilers the images download. The agent only builds those images after
// --accept-licenses, so the player decides, not us.
const licenseNotice = "Building this environment downloads Microsoft's compiler tools onto this PC. Start the agent with --accept-licenses to confirm you accept their licence terms (https://visualstudio.microsoft.com/license-terms/ for Visual Studio; the Visual C++ 6 package is the one TheSuperHackers' CI uses)."
