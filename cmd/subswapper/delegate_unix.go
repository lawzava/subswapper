//go:build !windows

package main

import (
	"errors"
	"os"
	"os/exec"
	"os/signal"
	"syscall"
	"time"
)

func executeDelegate(cmd *exec.Cmd, timeout time.Duration) error {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	// A descendant retaining an output pipe must not block Wait forever.
	cmd.WaitDelay = time.Second
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(signals)
	if err := cmd.Start(); err != nil {
		return errors.New("delegate process could not start")
	}
	// All home-run children inherit this group, including the Claude auth probe.
	// Forced termination also covers children that ignore INT and TERM.
	kill := func() { _ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	defer kill()
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case err := <-done:
		if err != nil {
			return &processFailure{processExitCode(err), "delegate process failed"}
		}
		return nil
	case <-timer.C:
		kill()
		<-done
		return &processFailure{124, "delegate timed out"}
	case received := <-signals:
		kill()
		<-done
		return &processFailure{128 + int(received.(syscall.Signal)), "delegate cancelled"}
	}
}

func signalExitCode(exit *exec.ExitError) int {
	if status, ok := exit.Sys().(syscall.WaitStatus); ok && status.Signaled() {
		return 128 + int(status.Signal())
	}
	return 1
}
