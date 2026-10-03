package subswapper

import (
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
