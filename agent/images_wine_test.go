package main

import (
	"strings"
	"testing"
)

func TestWineImagesCoverBothCompilers(t *testing.T) {
	want := map[string]string{"vc6": "generals-portal-vc6-wine:3", "msvc": "generals-portal-msvc-wine:1"}
	if len(wineImages) != len(want) {
		t.Fatalf("expected %d images, have %d", len(want), len(wineImages))
	}
	for fam, name := range want {
		s, ok := wineImages[fam]
		if !ok {
			t.Fatalf("no Wine image for %s", fam)
		}
		if s.Name != name {
			t.Errorf("%s: name %q, want %q", fam, s.Name, name)
		}
		if s.Windows {
			t.Errorf("%s: a Wine image is a Linux image", fam)
		}
		if strings.TrimSpace(s.Dockerfile) == "" {
			t.Errorf("%s: empty Dockerfile", fam)
		}
		if got, ok := imageForFamily(fam, false); !ok || got.Name != name {
			t.Errorf("%s: imageForFamily(false) = %q, %v", fam, got.Name, ok)
		}
	}
}

func TestWineRecipesCommonParts(t *testing.T) {
	for fam, s := range wineImages {
		d := s.Dockerfile
		if !strings.HasPrefix(d, "# ") || !strings.Contains(d, "NEVER BUILT") {
			t.Errorf("%s: the recipe must say up front that it was never built", fam)
		}
		for _, want := range []string{
			"FROM ubuntu:24.04", "WINEDEBUG=-all", "WINEPREFIX=/opt/wine", "chmod -R a+rwX /opt/wine",
			"/usr/local/bin/toolprobe", "MISSING:", "SHA-256", "sha256sum -c", "/usr/local/bin/wine-run",
		} {
			if !strings.Contains(d, want) {
				t.Errorf("%s: recipe lacks %q", fam, want)
			}
		}
		for _, tool := range []string{"cmake", "ninja", "git", "cl", "link", "rc", "midl"} {
			if !strings.Contains(d, tool) {
				t.Errorf("%s: toolprobe does not mention %s", fam, tool)
			}
		}
		// Nothing in these recipes is templated: a leftover marker means a copy-paste slip.
		for _, bad := range []string{"{{", "}}", "@@", "TODO", "FIXME", "XXX"} {
			if strings.Contains(d, bad) {
				t.Errorf("%s: unreplaced placeholder %q", fam, bad)
			}
		}
	}
}

func TestVC6RecipeUsesTheSamePinAsTheSuperHackers(t *testing.T) {
	d := wineImages["vc6"].Dockerfile
	for _, want := range []string{
		"001c4bafdcf2ef4b474d693acccd35a91e848f40",
		"itsmattkc/MSVC600",
		"VC6SP6",
		"VcOsDir=WINNT",
		`MSDevDir\\BIN`,
		`MFC\\LIB`,
		"WINEPATH",
	} {
		if !strings.Contains(d, want) {
			t.Errorf("VC6 recipe lacks %q", want)
		}
	}
	// The hash is compared case-insensitively: the recipe lower-cases it for sha256sum.
	if !strings.Contains(strings.ToLower(d), "d0ee1f6dcef7db3ad703120d9fb4fad49ebca28f44372e40550348b1c00ca583") {
		t.Error("VC6 recipe does not carry the pinned SHA-256")
	}
	if !strings.Contains(d, "sha256sum -c") {
		t.Error("the VC6 download is not verified")
	}
}

func TestMSVCRecipeIsGatedAndTargetsX86(t *testing.T) {
	d := wineImages["msvc"].Dockerfile
	for _, want := range []string{"mstorsjo/msvc-wine", "vsdownload.py", "--accept-license", "--accept-licenses", "bin/x86/msvcenv.sh", "INCLUDE", "LIB", "WINEPATH"} {
		if !strings.Contains(d, want) {
			t.Errorf("MSVC recipe lacks %q", want)
		}
	}
}
