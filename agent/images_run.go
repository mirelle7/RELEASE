package main

import (
	"context"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"time"
)

// pickBackendFor chooses where a build of this compiler family runs. MinGW-w64 uses the Linux image (or WSL).
// Visual C++ 6 and Visual Studio use a container only: Docker in Windows-container mode (real tools) or any engine in
// Linux mode (Wine). A Windows-mode Docker is preferred for them, because it runs the real compilers.
func pickBackendFor(family string, bs []Backend, pref string) *Backend {
	if family == "mingw" {
		return pickBackend(bs, pref)
	}
	var linux *Backend
	for i := range bs {
		b := &bs[i]
		if b.Kind == "wsl" || (pref != "" && pref != "auto" && pref != b.Kind) {
			continue
		}
		switch {
		case b.Windows && b.Path != "":
			return b
		case b.Ready && linux == nil:
			linux = b
		}
	}
	return linux
}

// needsLicense is true for the images that download Microsoft's compilers (VC6, Visual Studio). The MinGW images are open source.
func needsLicense(spec imageSpec) bool {
	return strings.Contains(spec.Name, "-vc6-") || strings.Contains(spec.Name, "-msvc-")
}

// mingwImage says which image the MinGW-w64 route uses: a custom one from --image (pulled, shipped=false), or one of the
// recipes shipped here, Clang (the modern build) or GCC, selected by --mingw-compiler.
func (a *Agent) mingwImage() (spec imageSpec, shipped bool) { return a.mingwImageFor("") }

// toolchainOf is the compiler a MinGW job uses: its own choice, else what the agent was started with.
func (a *Agent) toolchainOf(tc string) string {
	if tc == "" {
		return a.Mingw
	}
	return tc
}

// mingwImageFor is mingwImage for one job's toolchain ("" means the agent's own).
func (a *Agent) mingwImageFor(tc string) (spec imageSpec, shipped bool) {
	if a.Image != "" && a.Image != defaultImage {
		return imageSpec{Name: a.Image}, false
	}
	if a.toolchainOf(tc) == "gcc" {
		return imageSpec{Name: defaultImage, Dockerfile: toolchainDockerfile}, true
	}
	return clangImage, true
}

// imageHere picks the image for a family on a backend.
func imageHere(family string, b Backend) (imageSpec, bool) { return imageForFamily(family, b.Windows) }

func (a *Agent) imageExists(b Backend, name string) bool {
	_, ok := a.capture(20*time.Second, b.Kind, "image", "inspect", name)
	return ok
}

// ensureImageSpec builds a VC6 / Visual Studio image the first time it is needed, once the player has accepted the licences.
func (a *Agent) ensureImageSpec(ctx context.Context, b Backend, spec imageSpec, log io.Writer) error {
	if b.Kind == "wsl" || a.imageExists(b, spec.Name) { // WSL needs no image
		return nil
	}
	if needsLicense(spec) && !a.AcceptLicenses {
		return fmt.Errorf("%s", licenseNotice)
	}
	fmt.Fprintf(log, "\nBuilding %s (first time only; needs internet, can take a long time and several GB of disk)\n", spec.Name)
	args := []string{"build", "-t", spec.Name}
	if spec.Windows {
		args = append(args, "--isolation=hyperv", "--memory", "4g")
	}
	cmd := exec.CommandContext(ctx, b.Path, append(args, "-")...)
	cmd.Stdin = strings.NewReader(spec.Dockerfile)
	cmd.Env, cmd.Stdout, cmd.Stderr = a.env, log, log
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("could not build the %s environment: %v", spec.Name, err)
	}
	return nil
}

// doctorImage reports whether a Visual C++ 6 / Visual Studio build can run in a container on this PC.
// It returns true when it can (or could, once the image is built automatically).
func (a *Agent) doctorImage(rep *Report, fam string) bool {
	add := func(c Check) { rep.Checks = append(rep.Checks, c) }
	b := pickBackendFor(fam, a.Backends(), a.BackendPref)
	if b == nil {
		add(Check{ID: "container", Name: "A container to build in", Status: "fail",
			Detail: "No container engine is ready on this computer.",
			Fix:    "Install and start Docker Desktop or Podman. Docker in Windows-container mode runs the real compilers; any engine in Linux mode runs them under Wine."})
		return false
	}
	spec, ok := imageHere(fam, *b)
	if !ok {
		add(Check{ID: "image", Name: "Build environment image", Status: "fail", Detail: "No image recipe for this compiler."})
		return false
	}
	rep.Backend = b.Kind
	if b.Windows {
		rep.Backend += " (Windows containers)"
	} else {
		rep.Backend += " (Linux, Wine)"
	}
	rep.Image = spec.Name
	add(Check{ID: "backend", Name: "Build runs in", Status: "ok", Detail: rep.Backend})
	switch {
	case a.imageExists(*b, spec.Name):
		add(Check{ID: "image", Name: "Build environment image", Status: "ok", Detail: spec.Name})
	case !a.AcceptLicenses:
		add(Check{ID: "image", Name: "Build environment image", Status: "fail", Detail: "It has not been built yet. " + licenseNotice})
		return false
	default:
		add(Check{ID: "image", Name: "Build environment image", Status: "warn", Detail: "It is built automatically the first time (needs internet; can take a long time and several GB of disk)."})
	}
	return true
}
