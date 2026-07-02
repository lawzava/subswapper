//go:build windows

package subswapper

import (
	"errors"
	"os"
	"syscall"
	"time"
)

const errorSharingViolation = syscall.Errno(32)

// openLockFile opens the lock file with no sharing allowed, so a second
// process blocks (polling) until the handle is closed or its owner dies.
func openLockFile(path string) (*os.File, error) {
	pathPtr, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	notified := false
	for {
		handle, err := syscall.CreateFile(pathPtr,
			syscall.GENERIC_READ|syscall.GENERIC_WRITE,
			0, nil, syscall.OPEN_ALWAYS, syscall.FILE_ATTRIBUTE_NORMAL, 0)
		if err == nil {
			return os.NewFile(uintptr(handle), path), nil
		}
		if !errors.Is(err, errorSharingViolation) {
			return nil, err
		}
		if !notified {
			notified = true
			notifyLockWait(path)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func releaseLockFile(file *os.File) error {
	return file.Close()
}
