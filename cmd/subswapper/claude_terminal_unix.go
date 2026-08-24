//go:build !windows

package main

import (
	"errors"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"syscall"

	"github.com/creack/pty"
	"golang.org/x/term"
)

func runClaudeTerminalCommand(
	cmd *exec.Cmd,
	stdin io.Reader,
	stdout io.Writer,
	stderr io.Writer,
	token string,
) (bool, error) {
	input, inputOK := stdin.(*os.File)
	output, outputOK := stdout.(*os.File)
	errorOutput, errorOK := stderr.(*os.File)
	if !inputOK || !outputOK || !errorOK ||
		!term.IsTerminal(int(input.Fd())) ||
		!term.IsTerminal(int(output.Fd())) ||
		!term.IsTerminal(int(errorOutput.Fd())) {
		return false, nil
	}

	state, err := term.MakeRaw(int(input.Fd()))
	if err != nil {
		return true, errors.New("claude terminal setup failed")
	}
	defer func() { _ = term.Restore(int(input.Fd()), state) }()

	size, _ := pty.GetsizeFull(output)
	pseudoTerminal, err := pty.StartWithSize(cmd, size)
	if err != nil {
		return true, errors.New("claude terminal setup failed")
	}
	defer func() { _ = pseudoTerminal.Close() }()

	resize := make(chan os.Signal, 1)
	signal.Notify(resize, syscall.SIGWINCH)
	defer func() {
		signal.Stop(resize)
		close(resize)
	}()
	go func() {
		for range resize {
			_ = pty.InheritSize(output, pseudoTerminal)
		}
	}()

	forward := make(chan os.Signal, 4)
	signal.Notify(forward, syscall.SIGHUP, syscall.SIGINT, syscall.SIGTERM)
	defer func() {
		signal.Stop(forward)
		close(forward)
	}()
	go func() {
		for received := range forward {
			_ = syscall.Kill(-cmd.Process.Pid, received.(syscall.Signal))
		}
	}()

	// The command owns the session lifetime. The CLI exits immediately after
	// Wait, so the blocked stdin relay cannot consume input for another command.
	go func() { _, _ = io.Copy(pseudoTerminal, input) }()
	redactor := newSecretRedactingWriter(output, token)
	outputDone := make(chan error, 1)
	go func() {
		_, copyErr := io.Copy(redactor, pseudoTerminal)
		if errors.Is(copyErr, syscall.EIO) {
			copyErr = nil
		}
		outputDone <- errors.Join(copyErr, redactor.Flush())
	}()

	runErr := cmd.Wait()
	if outputErr := <-outputDone; outputErr != nil {
		return true, errors.New("claude output forwarding failed")
	}
	return true, runErr
}
