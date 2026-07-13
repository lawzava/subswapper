package subswapper

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
)

// StateLock is a cross-process exclusive lock guarding every
// load-modify-save cycle on the state file and the managed credential
// files. It blocks until acquired and is released automatically by the
// OS if the process dies.
type StateLock struct {
	file *os.File
}

func AcquireStateLock(ctx context.Context, cfg Config) (*StateLock, error) {
	path := ExpandPath(cfg.StatePath) + ".lock"
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	file, err := openLockFile(ctx, path)
	if err != nil {
		return nil, fmt.Errorf("lock state file %s: %w", path, err)
	}
	lock := &StateLock{file: file}
	if err := recoverFileTransaction(cfg); err != nil {
		lock.Release()
		return nil, err
	}
	return lock, nil
}

func (l *StateLock) Release() {
	if l == nil || l.file == nil {
		return
	}
	_ = releaseLockFile(l.file)
	l.file = nil
}

func notifyLockWait(path string) {
	fmt.Fprintf(os.Stderr, "waiting for another subswapper process to release %s...\n", path)
}
