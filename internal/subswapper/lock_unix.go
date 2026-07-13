//go:build !windows

package subswapper

import (
	"context"
	"errors"
	"os"
	"syscall"
	"time"
)

func openLockFile(ctx context.Context, path string) (*os.File, error) {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	notified := false
	for {
		err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return file, nil
		}
		if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
			if !notified {
				notified = true
				notifyLockWait(path)
			}
			select {
			case <-ctx.Done():
				_ = file.Close()
				return nil, ctx.Err()
			case <-time.After(25 * time.Millisecond):
			}
			continue
		}
		if !errors.Is(err, syscall.EINTR) {
			_ = file.Close()
			return nil, err
		}
	}
}

func releaseLockFile(file *os.File) error {
	err := syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
	if closeErr := file.Close(); err == nil {
		err = closeErr
	}
	return err
}
