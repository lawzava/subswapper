//go:build windows

package main

import (
	"io"
	"os/exec"
)

func runClaudeTerminalCommand(
	cmd *exec.Cmd,
	stdin io.Reader,
	stdout io.Writer,
	stderr io.Writer,
	token string,
) (bool, error) {
	return false, nil
}
