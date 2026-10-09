package main

// Embedding the retail Bink and Miles DLLs in the game executable (Visual C++ 6 and Visual Studio builds).
//
// The game links against import stubs (binkstub, milesstub) and needs the real binkw32.dll and mss32.dll at run time.
// The DLLs are the player's own copies, taken from their game folder at build time, so nothing is redistributed.
// The build is told (CMAKE_PROJECT_INCLUDE) to add three things to the game executables:
//   - an .rc file that stores the DLLs as resources,
//   - a delay-load hook that writes them to a per-user temp folder and loads them from there, and
//   - /DELAYLOAD for both DLLs, so the hook runs before the first call into them.
//
// Nothing is assumed about the PC: no game folder or missing DLLs is not an error, the build simply goes ahead without
// embedding and says so. The hook itself falls back to the normal DLL search if anything goes wrong.

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

// gameDLLs are the files looked for in the game folder (matched ignoring case, in this order).
var gameDLLs = []string{"binkw32.dll", "mss32.dll"}

// maxDLLSize keeps a wrong file (an installer, a video) from being embedded by mistake.
const maxDLLSize = 16 << 20

// DataStatus says what the agent found in the game folder.
type DataStatus struct {
	Dir     string   `json:"dir"`
	Exists  bool     `json:"exists"`
	Found   []string `json:"found"`
	Missing []string `json:"missing"`
	Ready   bool     `json:"ready"` // both DLLs are there
}

// findGameDLLs looks for the DLLs directly in dir (retail installs keep them next to the game exe).
func findGameDLLs(dir string) (paths map[string]string, st DataStatus) {
	st = DataStatus{Dir: dir, Found: []string{}, Missing: []string{}}
	paths = map[string]string{}
	if dir == "" {
		st.Missing = append(st.Missing, gameDLLs...)
		return
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		st.Missing = append(st.Missing, gameDLLs...)
		return
	}
	st.Exists = true
	for _, want := range gameDLLs {
		for _, e := range entries {
			if e.IsDir() || !strings.EqualFold(e.Name(), want) {
				continue
			}
			info, err := e.Info()
			if err != nil || info.Size() == 0 || info.Size() > maxDLLSize {
				continue
			}
			paths[want] = filepath.Join(dir, e.Name())
			break
		}
		if _, ok := paths[want]; ok {
			st.Found = append(st.Found, want)
		} else {
			st.Missing = append(st.Missing, want)
		}
	}
	st.Ready = len(st.Missing) == 0
	return
}

// DataStatus reports on the configured game folder.
func (a *Agent) DataStatus() DataStatus {
	_, st := findGameDLLs(a.dataDir())
	return st
}

func (a *Agent) dataDir() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.DataDir
}

// saveConfig remembers the folders for the next run.
func (a *Agent) saveConfig() {
	if a.ConfigPath == "" {
		return
	}
	projects := a.projectDirs()
	src := projects["ggc"]
	delete(projects, "ggc")
	raw, _ := json.Marshal(map[string]any{"Src": src, "Data": a.dataDir(), "Projects": projects})
	_ = os.WriteFile(a.ConfigPath, raw, 0o644)
}

// missingDLLsMessage explains in plain words what is wrong with the game folder.
func missingDLLsMessage(st DataStatus) string {
	switch {
	case st.Dir == "":
		return "no game folder is set."
	case !st.Exists:
		return "the game folder " + st.Dir + " cannot be read."
	}
	return "not found in " + st.Dir + ": " + strings.Join(st.Missing, ", ") + "."
}

// handleData: GET reports on the game folder; POST {"dir": "..."} sets it (an empty dir clears it).
func (a *Agent) handleData(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
	case http.MethodPost:
		var body struct {
			Dir string `json:"dir"`
		}
		dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBody))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&body); err != nil {
			writeJSON(w, 400, map[string]string{"error": "bad request body"})
			return
		}
		dir := strings.Trim(strings.TrimSpace(body.Dir), `"`)
		if dir != "" {
			abs, err := filepath.Abs(dir)
			if err != nil {
				writeJSON(w, 400, map[string]string{"error": "bad path"})
				return
			}
			if info, err := os.Stat(abs); err != nil || !info.IsDir() {
				writeJSON(w, 400, map[string]string{"error": "that folder does not exist on this PC"})
				return
			}
			dir = abs
		}
		a.mu.Lock()
		a.DataDir = dir
		a.mu.Unlock()
		a.saveConfig()
	default:
		writeJSON(w, 405, map[string]string{"error": "GET or POST only"})
		return
	}
	writeJSON(w, 200, a.DataStatus())
}

// canEmbed is true for the builds that can delay-load: Visual C++ 6 and Visual Studio. MinGW-w64 has no /DELAYLOAD.
func canEmbed(family string) bool { return family == "vc6" || family == "msvc" }

// prepareEmbed writes the generated files into outDir and returns the path of the CMake include file.
// It needs both DLLs; the caller decides what to do when they are missing.
func prepareEmbed(outDir string, dlls map[string]string) (string, error) {
	for _, n := range gameDLLs {
		if dlls[n] == "" {
			return "", fmt.Errorf("%s was not found in your game folder", n)
		}
	}
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return "", err
	}
	var rc bytes.Buffer
	rc.WriteString("// generated by the build agent: your own game DLLs, stored in the executable\n")
	hashes := make([]string, 0, len(gameDLLs))
	for i, n := range gameDLLs {
		raw, err := os.ReadFile(dlls[n])
		if err != nil {
			return "", err
		}
		sum := sha256.Sum256(raw)
		hashes = append(hashes, hex.EncodeToString(sum[:8]))
		copyPath := filepath.Join(outDir, n)
		if err := os.WriteFile(copyPath, raw, 0o644); err != nil {
			return "", err
		}
		fmt.Fprintf(&rc, "%d RCDATA \"%s\"\n", embedResourceBase+i, filepath.ToSlash(copyPath))
	}
	rcPath := filepath.Join(outDir, "embedded_dlls.rc")
	hookPath := filepath.Join(outDir, "embedded_dlls_hook.cpp")
	incPath := filepath.Join(outDir, "embed.cmake")
	hook := strings.NewReplacer("@BASE@", fmt.Sprint(embedResourceBase), "@TAG@", strings.Join(hashes, "-")).Replace(embedHookSource)
	files := map[string]string{
		rcPath:   rc.String(),
		hookPath: hook,
		incPath:  strings.NewReplacer("@RC@", filepath.ToSlash(rcPath), "@HOOK@", filepath.ToSlash(hookPath)).Replace(embedCMake),
	}
	for p, content := range files {
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			return "", err
		}
	}
	return incPath, nil
}

const embedResourceBase = 7001

// embedCMake is included after project(); the attach step is deferred to the end of the top-level directory, when the
// game's targets exist. Properties are set directly so no CMake policy decides whether it works.
const embedCMake = `# generated by the build agent
function(_rts_embed_attach)
  foreach(t g_generals z_generals)
    if(TARGET ${t})
      set_property(TARGET ${t} APPEND PROPERTY SOURCES "@RC@" "@HOOK@")
      set_property(TARGET ${t} APPEND PROPERTY LINK_OPTIONS "/DELAYLOAD:binkw32.dll" "/DELAYLOAD:mss32.dll")
      set_property(TARGET ${t} APPEND PROPERTY LINK_LIBRARIES "delayimp.lib")
      message(STATUS "Embedding binkw32.dll and mss32.dll in ${t}")
    endif()
  endforeach()
endfunction()
cmake_language(DEFER DIRECTORY "${CMAKE_SOURCE_DIR}" CALL _rts_embed_attach)
`

// embedHookSource compiles with Visual C++ 6 (which only knows the writable __pfnDliNotifyHook) and with later
// compilers (which look for the read-only __pfnDliNotifyHook2).
const embedHookSource = `// generated by the build agent
#include <windows.h>
#include <delayimp.h>
#include <string.h>

static const int kFirstResource = @BASE@;
static const char *kNames[2] = { "binkw32.dll", "mss32.dll" };
static const char kTag[] = "@TAG@";

static bool SameBytes(const char *path, const void *data, DWORD size)
{
	HANDLE f = CreateFileA(path, GENERIC_READ, FILE_SHARE_READ | FILE_SHARE_WRITE, NULL, OPEN_EXISTING, FILE_ATTRIBUTE_NORMAL, NULL);
	if (f == INVALID_HANDLE_VALUE)
		return false;
	bool same = false;
	if (GetFileSize(f, NULL) == size)
	{
		BYTE *buf = (BYTE *)HeapAlloc(GetProcessHeap(), 0, size);
		DWORD got = 0;
		if (buf)
		{
			if (ReadFile(f, buf, size, &got, NULL) && got == size)
				same = memcmp(buf, data, size) == 0;
			HeapFree(GetProcessHeap(), 0, buf);
		}
	}
	CloseHandle(f);
	return same;
}

static HMODULE LoadEmbedded(int index)
{
	HRSRC res = FindResourceA(NULL, MAKEINTRESOURCEA(kFirstResource + index), (LPCSTR)RT_RCDATA);
	if (!res)
		return NULL;
	HGLOBAL mem = LoadResource(NULL, res);
	DWORD size = SizeofResource(NULL, res);
	const void *data = mem ? LockResource(mem) : NULL;
	if (!data || !size)
		return NULL;

	char path[MAX_PATH];
	DWORD n = GetTempPathA(MAX_PATH, path);
	if (n == 0 || n + 64 >= MAX_PATH)
		return NULL;
	lstrcatA(path, "rts-embedded-");
	lstrcatA(path, kTag);
	CreateDirectoryA(path, NULL); // per user (the user's own temp folder); fine if it already exists
	lstrcatA(path, "\\");
	lstrcatA(path, kNames[index]);

	if (!SameBytes(path, data, size))
	{
		HANDLE f = CreateFileA(path, GENERIC_WRITE, 0, NULL, CREATE_ALWAYS, FILE_ATTRIBUTE_NORMAL, NULL);
		if (f == INVALID_HANDLE_VALUE)
			return NULL;
		DWORD put = 0;
		BOOL ok = WriteFile(f, data, size, &put, NULL) && put == size;
		CloseHandle(f);
		if (!ok)
			return NULL;
	}
	return LoadLibraryA(path);
}

static FARPROC WINAPI EmbeddedDllHook(unsigned dliNotify, PDelayLoadInfo pdli)
{
	if (dliNotify == dliNotePreLoadLibrary && pdli && pdli->szDll)
	{
		for (int i = 0; i < 2; ++i)
			if (lstrcmpiA(pdli->szDll, kNames[i]) == 0)
				return (FARPROC)LoadEmbedded(i); // NULL = fall back to the normal search
	}
	return NULL;
}

#if defined(_MSC_VER) && _MSC_VER >= 1400
extern "C" const PfnDliHook __pfnDliNotifyHook2 = EmbeddedDllHook;
#else
extern "C" PfnDliHook __pfnDliNotifyHook = EmbeddedDllHook;
#endif
`
