//go:build windows

package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
)

// loadEnvScript runs a compiler setup script (for example vcvars32.bat, or vcvarsall.bat with arguments) in cmd.exe
// and returns the environment it leaves behind.
func loadEnvScript(script, args string) ([]string, error) {
	ext := strings.ToLower(filepath.Ext(script))
	if ext != ".bat" && ext != ".cmd" {
		return nil, fmt.Errorf("--env-script must be a .bat or .cmd file")
	}
	if st, err := os.Stat(script); err != nil || st.IsDir() {
		return nil, fmt.Errorf("--env-script %q was not found", script)
	}
	if strings.ContainsAny(script+args, "\"&|<>^%\r\n") {
		return nil, fmt.Errorf("--env-script and --env-args may not contain quotes or shell characters")
	}
	cmd := exec.Command(os.Getenv("ComSpec"))
	cmd.SysProcAttr = &syscall.SysProcAttr{CmdLine: fmt.Sprintf(`cmd.exe /d /c "call "%s" %s >nul 2>&1 && set"`, script, args)}
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("running %s failed: %v", script, err)
	}
	return parseSetOutput(string(out)), nil
}
