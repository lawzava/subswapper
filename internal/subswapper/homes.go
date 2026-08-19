package subswapper

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	AccountModeHome   = "home"
	AccountModeBundle = "bundle"
)

type HomeMigrationResult struct {
	Copied  int
	Skipped int
}

func isBuiltInKind(kind string) bool {
	switch strings.ToLower(kind) {
	case "claude", "claude-code", "codex":
		return true
	default:
		return false
	}
}

func (s ServiceConfig) UsesAccountHomes() bool {
	return s.AccountMode == AccountModeHome
}

func AccountEnvironment(cfg Config, service ServiceConfig, accountName string) map[string]string {
	home := AccountDir(cfg, service.Name, accountName)
	switch {
	case isClaudeService(service):
		return map[string]string{"CLAUDE_CONFIG_DIR": home}
	case isCodexService(service):
		return map[string]string{"CODEX_HOME": home}
	default:
		return map[string]string{}
	}
}

func CreateAccountHome(cfg Config, serviceName, accountName, email string) (AccountState, string, error) {
	if err := validateAccountName(accountName); err != nil {
		return AccountState{}, "", err
	}
	service, ok := cfg.Service(serviceName)
	if !ok {
		return AccountState{}, "", fmt.Errorf("service %q not found", serviceName)
	}
	if service.Disabled {
		return AccountState{}, "", fmt.Errorf("service %q is disabled", serviceName)
	}
	if !service.UsesAccountHomes() {
		return AccountState{}, "", fmt.Errorf("service %q uses credential bundles; set account_mode to %q", serviceName, AccountModeHome)
	}

	lock, err := AcquireStateLock(context.Background(), cfg)
	if err != nil {
		return AccountState{}, "", err
	}
	defer lock.Release()
	state, err := LoadState(cfg.StatePath)
	if err != nil {
		return AccountState{}, "", err
	}
	serviceState := state.Service(service.Name)
	for existing := range serviceState.Accounts {
		if strings.EqualFold(existing, accountName) {
			return AccountState{}, "", fmt.Errorf("account %q already exists for service %q", existing, service.Name)
		}
	}

	home := AccountDir(cfg, service.Name, accountName)
	_, statErr := os.Stat(home)
	existed := statErr == nil
	if statErr != nil && !errors.Is(statErr, os.ErrNotExist) {
		return AccountState{}, "", statErr
	}
	if err := os.MkdirAll(home, 0o700); err != nil {
		return AccountState{}, "", err
	}
	if err := os.Chmod(home, 0o700); err != nil {
		return AccountState{}, "", err
	}
	account := AccountState{Name: accountName, Email: email, AddedAt: time.Now().UTC()}
	serviceState.Accounts[accountName] = account
	if serviceState.ActiveAccount == "" {
		serviceState.ActiveAccount = accountName
	}
	if err := SaveState(cfg.StatePath, state); err != nil {
		delete(serviceState.Accounts, accountName)
		if !existed {
			_ = os.Remove(home)
		}
		return AccountState{}, "", err
	}
	return account, home, nil
}

func ResetAccountProbeState(cfg Config, serviceName, accountName string) error {
	lock, err := AcquireStateLock(context.Background(), cfg)
	if err != nil {
		return err
	}
	defer lock.Release()
	state, err := LoadState(cfg.StatePath)
	if err != nil {
		return err
	}
	account, ok := state.Account(serviceName, accountName)
	if !ok {
		return fmt.Errorf("account %q not found for service %q", accountName, serviceName)
	}
	account.FetchBackoffUntil = time.Time{}
	account.CredentialsError = ""
	account.LastProbeError = ""
	state.Service(serviceName).Accounts[accountName] = account
	return SaveState(cfg.StatePath, state)
}

func MigrateAccountHomes(cfg Config) (HomeMigrationResult, error) {
	lock, err := AcquireStateLock(context.Background(), cfg)
	if err != nil {
		return HomeMigrationResult{}, err
	}
	defer lock.Release()
	state, err := LoadState(cfg.StatePath)
	if err != nil {
		return HomeMigrationResult{}, err
	}
	result := HomeMigrationResult{}
	for _, service := range cfg.Services {
		if !service.UsesAccountHomes() {
			continue
		}
		legacyNames := map[string]string{}
		if isClaudeService(service) {
			legacyNames = map[string]string{
				"credentials.json": ".credentials.json",
				"claude.json":      ".config.json",
			}
		}
		for accountName := range state.Service(service.Name).Accounts {
			home := AccountDir(cfg, service.Name, accountName)
			if err := os.MkdirAll(home, 0o700); err != nil {
				return result, err
			}
			if err := os.Chmod(home, 0o700); err != nil {
				return result, err
			}
			for legacyName, nativeName := range legacyNames {
				source := filepath.Join(home, legacyName)
				target := filepath.Join(home, nativeName)
				if _, err := os.Stat(source); errors.Is(err, os.ErrNotExist) {
					continue
				} else if err != nil {
					return result, err
				}
				if _, err := os.Stat(target); err == nil {
					result.Skipped++
					continue
				} else if !errors.Is(err, os.ErrNotExist) {
					return result, err
				}
				data, err := os.ReadFile(source)
				if err != nil {
					return result, err
				}
				if err := writeFileAtomic(target, data); err != nil {
					return result, err
				}
				result.Copied++
			}
		}
	}
	return result, nil
}
