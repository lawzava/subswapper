package subswapper

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// CaptureAccount copies the live managed files into a new bundle-mode
// account. Only tests still build bundle-mode fixtures with it.
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
