package main

import (
	"regexp"
	"strings"
	"testing"
)

func TestWinContainerImagesCoverBothCompilers(t *testing.T) {
	want := map[string]string{"vc6": "generals-portal-vc6-win:1", "msvc": "generals-portal-msvc-win:1"}
	if len(winContainerImages) != len(want) {
		t.Fatalf("expected %d families, got %d", len(want), len(winContainerImages))
	}
	for fam, name := range want {
		s, ok := winContainerImages[fam]
		if !ok {
			t.Fatalf("no Windows container image for %s", fam)
		}
		if s.Name != name || !s.Windows || strings.TrimSpace(s.Dockerfile) == "" {
			t.Fatalf("%s: %+v", fam, s)
		}
		if got, ok := imageForFamily(fam, true); !ok || got.Name != name {
			t.Fatalf("imageForFamily(%s, true) = %+v, %v", fam, got, ok)
		}
	}
}

var rePlaceholder = regexp.MustCompile(`@[A-Z0-9_]+@|\{\{|\}\}|TODO|FIXME`)

func TestWinContainerRecipesAreComplete(t *testing.T) {
	for fam, s := range winContainerImages {
		d := s.Dockerfile
		if m := rePlaceholder.FindString(d); m != "" {
			t.Errorf("%s: unreplaced placeholder %q", fam, m)
		}
		for _, need := range []string{
			"# escape=`",
			"FROM mcr.microsoft.com/windows/servercore:ltsc2022",
			"NEVER BUILT",
			"hyperv",
			"C:\\toolprobe.cmd",
			"MISSING:%%T",
			"cmake-$v-SHA-256.txt",
			"ninja-win.zip",
			"MinGit-",
		} {
			if !strings.Contains(d, need) {
				t.Errorf("%s: recipe lacks %q", fam, need)
			}
		}
	}
}

func TestVC6RecipeIsPinnedToTheCommitAndHashTSHUses(t *testing.T) {
	d := winContainerImages["vc6"].Dockerfile
	for _, need := range []string{
		"001c4bafdcf2ef4b474d693acccd35a91e848f40",
		"D0EE1F6DCEF7DB3AD703120D9FB4FAD49EBCA28F44372E40550348B1C00CA583",
		"VC6 hash mismatch",
		"C:\\VC6\\VC6SP6",
		`ENV VcOsDir="WINNT"`,
		`ENV INCLUDE="C:\VC6\VC6SP6\VC98\ATL\INCLUDE;C:\VC6\VC6SP6\VC98\INCLUDE;C:\VC6\VC6SP6\VC98\MFC\INCLUDE"`,
		`ENV LIB="C:\VC6\VC6SP6\VC98\LIB;C:\VC6\VC6SP6\VC98\MFC\LIB"`,
	} {
		if !strings.Contains(d, need) {
			t.Errorf("VC6 recipe lacks %q", need)
		}
	}
	if strings.Contains(d, "ENTRYPOINT") {
		t.Error("the VC6 image sets its environment with ENV and needs no entrypoint")
	}
}

func TestMSVCRecipeSetsUpTheX86CompilerBeforeEveryCommand(t *testing.T) {
	d := winContainerImages["msvc"].Dockerfile
	for _, need := range []string{
		"Microsoft.VisualStudio.Workload.VCTools",
		"--includeRecommended",
		"vcvarsall.bat\" x86",
		"C:\\entry.cmd",
		`ENTRYPOINT ["C:\\entry.cmd"]`,
		"3010",
	} {
		if !strings.Contains(d, need) {
			t.Errorf("MSVC recipe lacks %q", need)
		}
	}
}
