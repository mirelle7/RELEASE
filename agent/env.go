package main

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// Everything the agent runs uses a.env, which is the agent's own environment, optionally extended by a compiler
// setup script (vcvars32.bat and friends). Programs are looked up in that environment's PATH, not the agent's.

func envGet(env []string, key string) string {
	for i := len(env) - 1; i >= 0; i-- {
		k, v, ok := strings.Cut(env[i], "=")
		if ok && (k == key || (runtime.GOOS == "windows" && strings.EqualFold(k, key))) {
			return v
		}
	}
	return ""
}

// which finds name in env's PATH. On Windows it also tries the PATHEXT extensions.
func which(env []string, name string) string {
	exts := []string{""}
	if runtime.GOOS == "windows" && filepath.Ext(name) == "" {
		pe := envGet(env, "PATHEXT")
		if pe == "" {
			pe = ".COM;.EXE;.BAT;.CMD"
		}
		exts = strings.Split(strings.ToLower(pe), ";")
	}
	for _, dir := range filepath.SplitList(envGet(env, "PATH")) {
		if dir == "" {
			continue
		}
		for _, ext := range exts {
			p := filepath.Join(dir, name+ext)
			if st, err := os.Stat(p); err == nil && !st.IsDir() && (runtime.GOOS == "windows" || st.Mode()&0o111 != 0) {
				return p
			}
		}
	}
	return ""
}

// parseSetOutput reads the output of the cmd.exe `set` command (NAME=value lines) into an environment.
func parseSetOutput(text string) []string {
	var env []string
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimRight(line, "\r")
		if i := strings.Index(line, "="); i > 0 && !strings.HasPrefix(line, "=") {
			env = append(env, line)
		}
	}
	return env
}
