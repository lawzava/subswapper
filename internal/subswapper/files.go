package subswapper

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode"
)

func CaptureAccount(cfg Config, serviceName, accountName, email string) (AccountState, error) {
	if err := validateAccountName(accountName); err != nil {
		return AccountState{}, err
	}
	service, ok := cfg.Service(serviceName)
	if !ok {
		return AccountState{}, fmt.Errorf("service %q not found", serviceName)
	}
	if service.Disabled {
		return AccountState{}, fmt.Errorf("service %q is disabled", serviceName)
	}

	lock, err := AcquireStateLock(context.Background(), cfg)
	if err != nil {
		return AccountState{}, err
	}
	defer lock.Release()

	state, err := LoadState(cfg.StatePath)
	if err != nil {
		return AccountState{}, err
	}
	serviceState := state.Service(service.Name)
	for existing := range serviceState.Accounts {
		if existing != accountName && strings.EqualFold(existing, accountName) {
			return AccountState{}, fmt.Errorf("account name %q conflicts with existing account %q: names may not differ only by letter case", accountName, existing)
		}
	}
	accountDir := AccountDir(cfg, service.Name, accountName)
	if err := os.MkdirAll(accountDir, 0o700); err != nil {
		return AccountState{}, err
	}

	copied := false
	specs := make([]copySpec, 0, len(service.Files))
	for _, file := range service.Files {
		sourcePath := ExpandPath(file.Path)
		if _, err := os.Stat(sourcePath); err == nil {
			copied = true
		}
		specs = append(specs, copySpec{
			source:   sourcePath,
			target:   filepath.Join(accountDir, file.BackupName),
			required: file.IsRequired(),
		})
	}
	if !copied {
		return AccountState{}, fmt.Errorf("service %q had no active files to capture", service.Name)
	}
	if email == "" {
		email = inferAccountEmailFromPaths(service, func(file ManagedFile) string {
			return ExpandPath(file.Path)
		})
	}
	account := AccountState{
		Name:    accountName,
		Email:   email,
		AddedAt: time.Now().UTC(),
	}
	serviceState.Accounts[accountName] = account
	serviceState.ActiveAccount = accountName
	staged, err := stageManagedFiles(specs)
	if err != nil {
		return AccountState{}, err
	}
	if err := executeStagedFilesAndState(cfg, staged, state); err != nil {
		return AccountState{}, err
	}
	return account, nil
}

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
		// Restoring the backup over the live files would discard any
		// credentials the live client rotated since the last sync.
		return nil
	}
	if err := switchServiceFiles(cfg, service, state, accountName, time.Now().UTC()); err != nil {
		return err
	}
	return nil
}

func RemoveAccount(cfg Config, serviceName, accountName string, force bool) error {
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
	if _, ok := serviceState.Accounts[accountName]; !ok {
		return fmt.Errorf("account %q not found for service %q", accountName, service.Name)
	}
	if serviceState.ActiveAccount == accountName && !force {
		return fmt.Errorf("account %q is active; switch away first or pass -force", accountName)
	}
	accountDir := AccountDir(cfg, service.Name, accountName)
	staged := make([]stagedFile, 0, len(service.Files))
	for _, file := range service.Files {
		staged = append(staged, stagedFile{
			target: filepath.Join(accountDir, file.BackupName),
			remove: true,
		})
	}
	delete(serviceState.Accounts, accountName)
	if serviceState.ActiveAccount == accountName {
		serviceState.ActiveAccount = ""
	}
	if err := executeStagedFilesAndState(cfg, staged, state); err != nil {
		return err
	}
	return os.RemoveAll(accountDir)
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

// switchServiceFiles makes accountName's backup the live file set and commits
// the matching active-account state in the same recoverable transaction. It
// first syncs the outgoing account so rotated credentials are not lost.
func switchServiceFiles(cfg Config, service ServiceConfig, state *State, accountName string, switchedAt time.Time) error {
	serviceState := state.Service(service.Name)
	outgoing := serviceState.ActiveAccount
	if outgoing != "" && outgoing != accountName {
		if _, ok := serviceState.Accounts[outgoing]; ok {
			if err := syncAccountFiles(cfg, service, outgoing); err != nil {
				return fmt.Errorf("sync active account %q before switch: %w", outgoing, err)
			}
		}
	}
	specs, err := accountRestoreSpecs(cfg, service, accountName)
	if err != nil {
		return err
	}
	staged, err := stageManagedFiles(specs)
	if err != nil {
		return err
	}
	oldActive := serviceState.ActiveAccount
	oldSwitchedAt := serviceState.LastSwitchedAt
	restoreState := func() {
		serviceState.ActiveAccount = oldActive
		serviceState.LastSwitchedAt = oldSwitchedAt
	}
	serviceState.ActiveAccount = accountName
	serviceState.LastSwitchedAt = switchedAt
	if err := executeStagedFilesAndState(cfg, staged, state); err != nil {
		restoreState()
		return err
	}
	return nil
}

// syncAccountFiles copies the live managed files into accountName's backup.
// A missing required live file keeps the existing backup copy; a missing
// optional live file removes the stale backup, mirroring capture.
func syncAccountFiles(cfg Config, service ServiceConfig, accountName string) error {
	if err := verifyActiveIdentity(cfg, service, AccountState{Name: accountName}); err != nil {
		return err
	}
	accountDir := AccountDir(cfg, service.Name, accountName)
	if err := os.MkdirAll(accountDir, 0o700); err != nil {
		return err
	}
	specs := make([]copySpec, 0, len(service.Files))
	for _, file := range service.Files {
		sourcePath := ExpandPath(file.Path)
		backupPath := filepath.Join(accountDir, file.BackupName)
		if _, err := os.Stat(sourcePath); err != nil {
			if errors.Is(err, os.ErrNotExist) && file.IsRequired() {
				continue
			}
			if !errors.Is(err, os.ErrNotExist) {
				return err
			}
		} else if syncWouldCorruptBackup(service, sourcePath, backupPath) {
			continue
		}
		specs = append(specs, copySpec{
			source:   sourcePath,
			target:   backupPath,
			required: file.IsRequired(),
		})
	}
	return copyManagedFiles(cfg, specs)
}

// syncWouldCorruptBackup reports whether copying the live file over the
// backup would replace working credentials with empty or unparseable
// content — e.g. a truncated file left by a crashed client or a logout
// happening at switch time. The good backup is kept in that case.
func syncWouldCorruptBackup(service ServiceConfig, livePath, backupPath string) bool {
	backup, err := os.ReadFile(backupPath)
	if err != nil || len(bytes.TrimSpace(backup)) == 0 {
		return false
	}
	live, err := os.ReadFile(livePath)
	if err != nil {
		return false
	}
	if len(bytes.TrimSpace(live)) == 0 {
		return true
	}
	if isClaudeService(service) && claudeCredentialsUsable(backup) && !claudeCredentialsUsable(live) {
		return true
	}
	if isCodexService(service) && validateCodexAuth(backup) == nil && validateCodexAuth(live) != nil {
		return true
	}
	return false
}

func claudeCredentialsUsable(data []byte) bool {
	oauth, err := parseClaudeOAuth(data)
	return err == nil && (oauth.AccessToken != "" || oauth.RefreshToken != "")
}

func accountRestoreSpecs(cfg Config, service ServiceConfig, accountName string) ([]copySpec, error) {
	accountDir := AccountDir(cfg, service.Name, accountName)
	specs := make([]copySpec, 0, len(service.Files))
	for _, file := range service.Files {
		sourcePath := filepath.Join(accountDir, file.BackupName)
		if _, err := os.Stat(sourcePath); err != nil {
			if !errors.Is(err, os.ErrNotExist) || file.IsRequired() {
				return nil, fmt.Errorf("account %q missing backup %s: %w", accountName, file.BackupName, err)
			}
		}
		specs = append(specs, copySpec{
			source:   sourcePath,
			target:   ExpandPath(file.Path),
			required: file.IsRequired(),
		})
	}
	return specs, nil
}

type copySpec struct {
	source   string
	target   string
	required bool
}

// copyManagedFiles applies all copies in two phases: every source is staged
// into a temp file next to its target first, then all targets are committed.
// A failure while staging leaves every target untouched.
func copyManagedFiles(cfg Config, specs []copySpec) error {
	staged, err := stageManagedFiles(specs)
	if err != nil {
		return err
	}
	discard := func() {
		for _, s := range staged {
			s.discard()
		}
	}
	err = executeFileTransaction(cfg, staged)
	discard()
	return err
}

func stageManagedFiles(specs []copySpec) ([]stagedFile, error) {
	staged := make([]stagedFile, 0, len(specs))
	for _, spec := range specs {
		s, err := stageCopy(spec.source, spec.target, spec.required)
		if err != nil {
			for _, file := range staged {
				file.discard()
			}
			return nil, err
		}
		staged = append(staged, s)
	}
	return staged, nil
}

func executeStagedFilesAndState(cfg Config, staged []stagedFile, state *State) error {
	stateData, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		discardStagedFiles(staged)
		return err
	}
	stagedState, err := stageFile(ExpandPath(cfg.StatePath), bytes.NewReader(stateData))
	if err != nil {
		discardStagedFiles(staged)
		return err
	}
	staged = append(staged, stagedState)
	err = executeFileTransaction(cfg, staged)
	discardStagedFiles(staged)
	return err
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

func stageCopy(sourcePath, targetPath string, required bool) (stagedFile, error) {
	source, err := os.Open(sourcePath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) && !required {
			return stagedFile{target: targetPath, remove: true}, nil
		}
		return stagedFile{}, err
	}
	defer func() { _ = source.Close() }()
	return stageFile(targetPath, source)
}

// stageFile writes content to a 0600 temp file next to targetPath, ready to
// be committed into place atomically.
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

// resolveTargetPath follows an existing symlink so writes land in the file it
// points at instead of replacing the link with a regular file.
func resolveTargetPath(path string) string {
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return path
	}
	return resolved
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

func inferAccountEmailFromPaths(service ServiceConfig, pathFor func(ManagedFile) string) string {
	for _, file := range service.Files {
		data, err := os.ReadFile(pathFor(file))
		if err != nil {
			continue
		}
		var decoded any
		if err := json.Unmarshal(data, &decoded); err != nil {
			continue
		}
		if email := findEmail(decoded); email != "" {
			return email
		}
	}
	return ""
}

func findEmail(value any) string {
	switch typed := value.(type) {
	case map[string]any:
		for _, key := range []string{"email", "emailAddress", "email_address", "accountEmail"} {
			if raw, ok := typed[key].(string); ok && strings.Contains(raw, "@") {
				return raw
			}
		}
		for _, child := range typed {
			if email := findEmail(child); email != "" {
				return email
			}
		}
	case []any:
		for _, child := range typed {
			if email := findEmail(child); email != "" {
				return email
			}
		}
	}
	return ""
}
