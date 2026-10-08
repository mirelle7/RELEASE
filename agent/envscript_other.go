//go:build !windows

package main

import "errors"

func loadEnvScript(script, args string) ([]string, error) {
	return nil, errors.New("--env-script is only for Windows, where the compilers live")
}
