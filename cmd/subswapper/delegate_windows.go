package main

import (
	"errors"
	"os/exec"
	"time"
)

func executeDelegate(_ *exec.Cmd, _ time.Duration) error {
	return errors.New("delegate requires Unix process-group cancellation; Windows is not supported")
}
func signalExitCode(_ *exec.ExitError) int { return 1 }
