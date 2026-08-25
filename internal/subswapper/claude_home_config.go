package subswapper

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
)

// claudeSharedConfigNames is an explicit allowlist of user-authored Claude
// configuration. Generated state must remain in each account home.
var claudeSharedConfigNames = []string{
	"CLAUDE.md",
	"settings.json",
	"keybindings.json",
	"plugins",
	"skills",
	"agents",
	"output-styles",
	"rules",
	"commands",
	"workflows",
	"themes",
}

type HomeRepairResult struct {
	Linked    []string
	Unchanged []string
	Missing   []string
	Conflicts []string
}

// RepairAccountHome adds missing links to user-authored Claude configuration.
// It does not replace any existing account-home entry.
func RepairAccountHome(cfg Config, serviceName, accountName string) (HomeRepairResult, error) {
	if err := validateAccountName(accountName); err != nil {
		return HomeRepairResult{}, err
	}
	service, ok := cfg.Service(serviceName)
	if !ok {
		return HomeRepairResult{}, fmt.Errorf("service %q not found", serviceName)
	}
	if !service.UsesAccountHomes() {
		return HomeRepairResult{}, fmt.Errorf("service %q uses credential bundles; set account_mode to %q", serviceName, AccountModeHome)
	}
	if !isClaudeService(service) {
		return HomeRepairResult{}, fmt.Errorf("service %q does not support shared Claude user configuration", serviceName)
	}

	lock, err := AcquireStateLock(context.Background(), cfg)
	if err != nil {
		return HomeRepairResult{}, err
	}
	defer lock.Release()
	state, err := LoadState(cfg.StatePath)
	if err != nil {
		return HomeRepairResult{}, err
	}
	if _, ok := state.Account(service.Name, accountName); !ok {
		return HomeRepairResult{}, fmt.Errorf("account %q not found for service %q", accountName, service.Name)
	}
	home := AccountDir(cfg, service.Name, accountName)
	info, err := os.Lstat(home)
	if err != nil {
		return HomeRepairResult{}, fmt.Errorf("inspect account home: %w", err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return HomeRepairResult{}, errors.New("account home is not a regular directory")
	}
	return repairClaudeSharedConfig(home)
}

func repairClaudeSharedConfig(accountHome string) (HomeRepairResult, error) {
	nativeUserHome, err := os.UserHomeDir()
	if err != nil {
		return HomeRepairResult{}, errors.New("native user home is unavailable")
	}
	nativeClaudeHome := filepath.Join(nativeUserHome, ".claude")
	result := HomeRepairResult{}
	for _, name := range claudeSharedConfigNames {
		source := filepath.Join(nativeClaudeHome, name)
		if _, err := os.Lstat(source); errors.Is(err, os.ErrNotExist) {
			result.Missing = append(result.Missing, name)
			continue
		} else if err != nil {
			return result, fmt.Errorf("inspect native Claude configuration %q: %w", name, err)
		}

		target := filepath.Join(accountHome, name)
		info, err := os.Lstat(target)
		if err == nil {
			if info.Mode()&os.ModeSymlink != 0 {
				link, readErr := os.Readlink(target)
				if readErr != nil {
					return result, fmt.Errorf("inspect account configuration link %q: %w", name, readErr)
				}
				if link == source {
					result.Unchanged = append(result.Unchanged, name)
					continue
				}
			}
			result.Conflicts = append(result.Conflicts, name)
			continue
		}
		if !errors.Is(err, os.ErrNotExist) {
			return result, fmt.Errorf("inspect account configuration %q: %w", name, err)
		}
		if err := os.Symlink(source, target); err != nil {
			return result, claudeSharedConfigLinkError(name, err)
		}
		result.Linked = append(result.Linked, name)
	}
	return result, nil
}

func claudeSharedConfigLinkError(name string, err error) error {
	if runtime.GOOS == "windows" {
		return fmt.Errorf("link Claude user configuration %q: Windows could not create a symbolic link; enable Developer Mode or grant symbolic-link privilege: %w", name, err)
	}
	return fmt.Errorf("link Claude user configuration %q: %w", name, err)
}

func removeClaudeSharedConfigLinks(accountHome string, names []string) {
	nativeUserHome, err := os.UserHomeDir()
	if err != nil {
		return
	}
	for _, name := range names {
		path := filepath.Join(accountHome, name)
		info, err := os.Lstat(path)
		if err != nil || info.Mode()&os.ModeSymlink == 0 {
			continue
		}
		target, err := os.Readlink(path)
		if err != nil || target != filepath.Join(nativeUserHome, ".claude", name) {
			continue
		}
		_ = os.Remove(path)
	}
}
