package subswapper

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// LaunchConfig is what an integration such as Paseo runs instead of the
// provider CLI. Command is a provider shim, so the integration sees the real
// CLI's version while every launch still goes through subswapper.
type LaunchConfig struct {
	Command []string          `json:"command"`
	Env     map[string]string `json:"env"`
}

// ProviderCLI names the CLI a Claude or Codex service launches.
func ProviderCLI(service ServiceConfig) (string, bool) {
	switch {
	case isClaudeService(service):
		return "claude", true
	case isCodexService(service):
		return "codex", true
	}
	return "", false
}

// ProviderShimName returns "claude" or "codex" when path names a provider
// CLI, and "" otherwise.
func ProviderShimName(path string) string {
	base := filepath.Base(path)
	if runtime.GOOS == "windows" {
		base = strings.ToLower(strings.TrimSuffix(base, filepath.Ext(base)))
	}
	switch base {
	case "claude", "codex":
		return base
	}
	return ""
}

// ProviderShimPath is where EnsureProviderShim links the named CLI.
func ProviderShimPath(name string) string {
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	return filepath.Join(DefaultDataRoot(), "bin", name)
}

// EnsureProviderShim makes ProviderShimPath(name) a symlink to executable.
// A link rather than a copy keeps the shim current when subswapper is
// upgraded in place.
func EnsureProviderShim(name, executable string) (string, error) {
	path := ProviderShimPath(name)
	if target, err := os.Readlink(path); err == nil && target == executable {
		return path, nil
	}
	if info, err := os.Lstat(path); err == nil && info.Mode()&os.ModeSymlink == 0 {
		return "", fmt.Errorf("%s exists and is not a link; remove it and retry", path)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", err
	}
	temporary := fmt.Sprintf("%s.%d.tmp", path, os.Getpid())
	_ = os.Remove(temporary)
	if err := os.Symlink(executable, temporary); err != nil {
		if runtime.GOOS == "windows" {
			return "", fmt.Errorf("link %s: %w (Windows needs Developer Mode or an elevated shell to create symlinks)", path, err)
		}
		return "", fmt.Errorf("link %s: %w", path, err)
	}
	if err := os.Rename(temporary, path); err != nil {
		_ = os.Remove(temporary)
		return "", err
	}
	return path, nil
}

// ExportLaunchConfig links the service's provider shim to executable and
// returns how to launch it. The environment names the config and service;
// it carries no credentials.
func ExportLaunchConfig(cfg Config, configPath, serviceName, executable string) (LaunchConfig, error) {
	service, ok := cfg.Service(serviceName)
	if !ok || service.Disabled {
		return LaunchConfig{}, fmt.Errorf("service %q is missing or disabled", serviceName)
	}
	name, ok := ProviderCLI(service)
	if !ok {
		return LaunchConfig{}, errors.New("launch-config supports Claude and Codex services")
	}
	shim, err := EnsureProviderShim(name, executable)
	if err != nil {
		return LaunchConfig{}, err
	}
	configPath, err = filepath.Abs(configPath)
	if err != nil {
		return LaunchConfig{}, err
	}
	return LaunchConfig{
		Command: []string{shim},
		Env: map[string]string{
			"SUBSWAPPER_CONFIG_PATH": configPath,
			"SUBSWAPPER_SERVICE":     service.Name,
		},
	}, nil
}
