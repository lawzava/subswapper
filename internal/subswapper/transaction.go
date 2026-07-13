package subswapper

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

type fileTransactionJournal struct {
	Entries []fileTransactionEntry `json:"entries"`
}

type fileTransactionEntry struct {
	Target       string `json:"target"`
	RollbackPath string `json:"rollback_path,omitempty"`
	Existed      bool   `json:"existed"`
}

var commitStagedFile = func(file stagedFile) error { return file.commit() }
var removeRollbackFile = os.Remove

func executeFileTransaction(cfg Config, staged []stagedFile) error {
	journal := fileTransactionJournal{Entries: make([]fileTransactionEntry, 0, len(staged))}
	for _, file := range staged {
		entry, err := snapshotTransactionTarget(file.target)
		if err != nil {
			return errors.Join(err, cleanupFileTransactionRollbacks(journal))
		}
		journal.Entries = append(journal.Entries, entry)
	}

	if err := writeFileTransactionJournal(cfg, journal); err != nil {
		return errors.Join(err, cleanupFileTransactionRollbacks(journal))
	}
	for _, file := range staged {
		if err := commitStagedFile(file); err != nil {
			rollbackErr := rollbackFileTransaction(journal)
			if rollbackErr == nil {
				rollbackErr = removeFileTransactionJournal(cfg)
				if rollbackErr == nil {
					rollbackErr = cleanupFileTransactionRollbacks(journal)
				}
			}
			return errors.Join(err, rollbackErr)
		}
	}
	if err := removeFileTransactionJournal(cfg); err != nil {
		return err
	}
	// The transaction is committed once the journal is gone. Rollback cleanup
	// is best effort and must not turn a successful commit into a caller-visible
	// failure that could restore only in-memory state.
	_ = cleanupFileTransactionRollbacks(journal)
	return nil
}

func recoverFileTransaction(cfg Config) error {
	path := fileTransactionJournalPath(cfg)
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read file transaction journal: %w", err)
	}
	var journal fileTransactionJournal
	if err := json.Unmarshal(data, &journal); err != nil {
		return fmt.Errorf("decode file transaction journal: %w", err)
	}
	if err := rollbackFileTransaction(journal); err != nil {
		return fmt.Errorf("recover file transaction: %w", err)
	}
	if err := removeFileTransactionJournal(cfg); err != nil {
		return err
	}
	return cleanupFileTransactionRollbacks(journal)
}

func cleanupFileTransactionRollbacks(journal fileTransactionJournal) error {
	var errs []error
	for _, entry := range journal.Entries {
		if entry.RollbackPath == "" {
			continue
		}
		if err := removeRollbackFile(entry.RollbackPath); err != nil && !errors.Is(err, os.ErrNotExist) {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func snapshotTransactionTarget(target string) (fileTransactionEntry, error) {
	entry := fileTransactionEntry{Target: target}
	source, err := os.Open(target)
	if errors.Is(err, os.ErrNotExist) {
		return entry, nil
	}
	if err != nil {
		return entry, fmt.Errorf("snapshot transaction target %s: %w", target, err)
	}
	defer func() { _ = source.Close() }()
	info, err := source.Stat()
	if err != nil {
		return entry, err
	}
	if !info.Mode().IsRegular() {
		return entry, fmt.Errorf("transaction target %s is not a regular file", target)
	}
	rollback, err := os.CreateTemp(filepath.Dir(target), ".subswapper-rollback-*")
	if err != nil {
		return entry, err
	}
	rollbackPath := rollback.Name()
	cleanup := true
	defer func() {
		_ = rollback.Close()
		if cleanup {
			_ = os.Remove(rollbackPath)
		}
	}()
	if _, err := io.Copy(rollback, source); err != nil {
		return entry, err
	}
	if err := rollback.Sync(); err != nil {
		return entry, err
	}
	if err := rollback.Close(); err != nil {
		return entry, err
	}
	if err := os.Chmod(rollbackPath, 0o600); err != nil {
		return entry, err
	}
	cleanup = false
	entry.Existed = true
	entry.RollbackPath = rollbackPath
	return entry, nil
}

func rollbackFileTransaction(journal fileTransactionJournal) error {
	var errs []error
	for _, entry := range journal.Entries {
		if err := validateTransactionEntry(entry); err != nil {
			errs = append(errs, err)
			continue
		}
		if entry.Existed {
			source, err := os.Open(entry.RollbackPath)
			if err != nil {
				errs = append(errs, err)
				continue
			}
			staged, stageErr := stageFile(entry.Target, source)
			_ = source.Close()
			if stageErr != nil {
				errs = append(errs, stageErr)
				continue
			}
			if err := os.Remove(entry.Target); err != nil && !errors.Is(err, os.ErrNotExist) {
				staged.discard()
				errs = append(errs, err)
				continue
			}
			if err := staged.commit(); err != nil {
				staged.discard()
				errs = append(errs, err)
				continue
			}
			continue
		}
		if err := os.Remove(entry.Target); err != nil && !errors.Is(err, os.ErrNotExist) {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func validateTransactionEntry(entry fileTransactionEntry) error {
	if entry.Target == "" || !filepath.IsAbs(entry.Target) {
		return fmt.Errorf("invalid transaction target %q", entry.Target)
	}
	if !entry.Existed {
		return nil
	}
	if filepath.Dir(entry.RollbackPath) != filepath.Dir(entry.Target) ||
		!strings.HasPrefix(filepath.Base(entry.RollbackPath), ".subswapper-rollback-") {
		return fmt.Errorf("invalid rollback path %q for %q", entry.RollbackPath, entry.Target)
	}
	return nil
}

func writeFileTransactionJournal(cfg Config, journal fileTransactionJournal) error {
	data, err := json.Marshal(journal)
	if err != nil {
		return err
	}
	return writeFileAtomic(fileTransactionJournalPath(cfg), data)
}

func removeFileTransactionJournal(cfg Config) error {
	err := os.Remove(fileTransactionJournalPath(cfg))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

func fileTransactionJournalPath(cfg Config) string {
	return ExpandPath(cfg.StatePath) + ".transaction.json"
}
