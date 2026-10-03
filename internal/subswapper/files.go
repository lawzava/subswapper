package subswapper

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode"
)

func SwitchAccount(cfg Config, serviceName, accountName string) error {
	service, ok := cfg.Service(serviceName)
	if !ok {
		return fmt.Errorf("service %q not found", serviceName)
	}
	if service.Disabled {
		return fmt.Errorf("service %q is disabled", serviceName)
	}
	lock, err := AcquireStateLock(context.Background(), cfg)
	if err != nil {
		return err
	}
	defer lock.Release()

	state, err := LoadState(cfg.StatePath)
	if err != nil {
		return err
	}
	if _, ok := state.Account(service.Name, accountName); !ok {
		return fmt.Errorf("account %q not found for service %q", accountName, service.Name)
	}
	if state.Service(service.Name).ActiveAccount == accountName {
		return nil
	}
	selectAccount(state, service, accountName, time.Now().UTC())
	return SaveState(cfg.StatePath, state)
}

// selectAccount makes accountName the route for new requests. Running
// processes follow through the proxy; nothing on disk changes.
func selectAccount(state *State, service ServiceConfig, accountName string, switchedAt time.Time) {
	serviceState := state.Service(service.Name)
	serviceState.ActiveAccount = accountName
	serviceState.LastSwitchedAt = switchedAt
}

func RemoveAccountWithOptions(cfg Config, serviceName, accountName string, force, deleteHome bool) error {
	if strings.TrimSpace(accountName) == "" {
		return errors.New("account name is required")
	}
	service, ok := cfg.Service(serviceName)
	if !ok {
		return fmt.Errorf("service %q not found", serviceName)
	}
	lock, err := AcquireStateLock(context.Background(), cfg)
	if err != nil {
		return err
	}
	defer lock.Release()

	state, err := LoadState(cfg.StatePath)
	if err != nil {
		return err
	}
	serviceState := state.Service(service.Name)
	account, ok := serviceState.Accounts[accountName]
	if !ok {
		return fmt.Errorf("account %q not found for service %q", accountName, service.Name)
	}
	if serviceState.ActiveAccount == accountName && !force {
		return fmt.Errorf("account %q is active; switch away first or pass -force", accountName)
	}
	if isClaudeService(service) {
		_, tokenExists, tokenErr := readClaudeSetupTokenEnvelope(cfg, service.Name, accountName)
		if tokenErr != nil {
			return tokenErr
		}
		if tokenExists || account.SetupTokenRevision != "" {
			return errors.New("remove the Claude setup token before unregistering this account")
		}
	}
	delete(serviceState.Accounts, accountName)
	if serviceState.ActiveAccount == accountName {
		serviceState.ActiveAccount = ""
	}
	if err := SaveState(cfg.StatePath, state); err != nil {
		return err
	}
	if !deleteHome {
		return nil
	}
	return os.RemoveAll(AccountDir(cfg, service.Name, accountName))
}

func validateAccountName(name string) error {
	if strings.TrimSpace(name) == "" {
		return errors.New("account name is required")
	}
	if name == "auto" {
		return errors.New(`account name "auto" is reserved for switch -account auto`)
	}
	if strings.Trim(name, ".") == "" {
		return fmt.Errorf("account name %q is not allowed", name)
	}
	return nil
}

func writeFileAtomic(path string, data []byte) error {
	staged, err := stageFile(path, bytes.NewReader(data))
	if err != nil {
		return err
	}
	if err := staged.commit(); err != nil {
		staged.discard()
		return err
	}
	return nil
}

func AccountDir(cfg Config, serviceName, accountName string) string {
	return filepath.Join(ExpandPath(cfg.BackupRoot), safeName(serviceName), safeName(accountName))
}

func safeName(value string) string {
	var b strings.Builder
	for _, r := range value {
		switch {
		case r == '-' || r == '_' || r == '.':
			b.WriteRune(r)
		case r <= unicode.MaxASCII && (unicode.IsLetter(r) || unicode.IsDigit(r)):
			b.WriteRune(r)
		default:
			b.WriteRune('_')
		}
	}
	sanitized := b.String()
	if strings.Trim(sanitized, ".") == "" {
		sanitized = "account"
	}
	if sanitized == value {
		return sanitized
	}
	sum := sha256.Sum256([]byte(value))
	return sanitized + "-" + hex.EncodeToString(sum[:])[:12]
}
func discardStagedFiles(staged []stagedFile) {
	for _, file := range staged {
		file.discard()
	}
}

type stagedFile struct {
	tmpPath string
	target  string
	remove  bool
}

func stageFile(targetPath string, content io.Reader) (stagedFile, error) {
	target := resolveTargetPath(targetPath)
	if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
		return stagedFile{}, err
	}
	tmp, err := os.CreateTemp(filepath.Dir(target), ".subswapper-*")
	if err != nil {
		return stagedFile{}, err
	}
	tmpPath := tmp.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(tmpPath)
		}
	}()
	if _, err := io.Copy(tmp, content); err != nil {
		_ = tmp.Close()
		return stagedFile{}, err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return stagedFile{}, err
	}
	if err := tmp.Close(); err != nil {
		return stagedFile{}, err
	}
	if err := os.Chmod(tmpPath, 0o600); err != nil {
		return stagedFile{}, err
	}
	cleanup = false
	return stagedFile{tmpPath: tmpPath, target: target}, nil
}

func (s stagedFile) commit() error {
	if s.remove {
		if err := os.Remove(s.target); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return nil
	}
	if err := os.Rename(s.tmpPath, s.target); err != nil {
		return err
	}
	return os.Chmod(s.target, 0o600)
}

func (s stagedFile) discard() {
	if s.tmpPath != "" {
		_ = os.Remove(s.tmpPath)
	}
}

func resolveTargetPath(path string) string {
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return path
	}
	return resolved
}
