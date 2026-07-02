package subswapper

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

type Config struct {
	BackupRoot string          `json:"backup_root,omitempty"`
	StatePath  string          `json:"state_path,omitempty"`
	Monitor    MonitorConfig   `json:"monitor"`
	Services   []ServiceConfig `json:"services"`
}

type MonitorConfig struct {
	Interval   Duration `json:"interval"`
	AutoSwitch *bool    `json:"auto_switch,omitempty"`
	// SwitchThreshold is the usage ratio (0-1] of any window at which the
	// active account becomes eligible for an auto-switch. Default 0.90.
	SwitchThreshold *float64 `json:"switch_threshold,omitempty"`
	// MinImprovement is how much lower (0-1) the best account's worst-window
	// ratio must be before an auto-switch is worthwhile. Default 0.10.
	MinImprovement *float64 `json:"min_improvement,omitempty"`
	// Cooldown is the minimum time between threshold-driven auto-switches.
	// It does not delay escaping an exhausted or broken active account.
	// Default 30m.
	Cooldown *Duration `json:"cooldown,omitempty"`
}

type Duration struct {
	time.Duration
}

type ServiceConfig struct {
	Name         string        `json:"name"`
	Kind         string        `json:"kind"`
	DisplayName  string        `json:"display_name,omitempty"`
	Files        []ManagedFile `json:"files,omitempty"`
	UsageCommand []string      `json:"usage_command,omitempty"`
	Disabled     bool          `json:"disabled,omitempty"`
}

type ManagedFile struct {
	Path       string `json:"path"`
	BackupName string `json:"backup_name"`
	Required   *bool  `json:"required,omitempty"`
}

func LoadConfig(path string) (*Config, error) {
	data, err := os.ReadFile(ExpandPath(path))
	if err != nil {
		return nil, err
	}

	var cfg Config
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&cfg); err != nil {
		return nil, err
	}
	cfg.ApplyDefaults()
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

func (c *Config) ApplyDefaults() {
	dataRoot := DefaultDataRoot()
	if c.BackupRoot == "" {
		c.BackupRoot = filepath.Join(dataRoot, "accounts")
	}
	if c.StatePath == "" {
		c.StatePath = filepath.Join(dataRoot, "state.json")
	}
	if c.Monitor.Interval.Duration == 0 {
		c.Monitor.Interval.Duration = time.Minute
	}
	for index := range c.Services {
		service := &c.Services[index]
		if service.Kind == "" {
			service.Kind = service.Name
		}
		if service.DisplayName == "" {
			service.DisplayName = service.Name
		}
		if len(service.Files) == 0 {
			service.Files = defaultManagedFiles(service.Kind)
		}
	}
}

func (c Config) Validate() error {
	if len(c.Services) == 0 {
		return errors.New("config must define at least one service")
	}
	if c.Monitor.Interval.Duration < 0 {
		return errors.New("monitor.interval must be non-negative")
	}
	if t := c.Monitor.SwitchThreshold; t != nil && (*t <= 0 || *t > 1) {
		return errors.New("monitor.switch_threshold must be a ratio in (0, 1], e.g. 0.9 for 90%")
	}
	if i := c.Monitor.MinImprovement; i != nil && (*i < 0 || *i > 1) {
		return errors.New("monitor.min_improvement must be a ratio in [0, 1], e.g. 0.1 for 10 points")
	}
	if d := c.Monitor.Cooldown; d != nil && d.Duration < 0 {
		return errors.New("monitor.cooldown must be non-negative")
	}
	seen := map[string]struct{}{}
	for _, service := range c.Services {
		if strings.TrimSpace(service.Name) == "" {
			return errors.New("service name is required")
		}
		// Case-insensitive: backup directories collide on the default
		// macOS/Windows filesystems regardless of name case.
		serviceKey := strings.ToLower(service.Name)
		if _, exists := seen[serviceKey]; exists {
			return fmt.Errorf("duplicate service %q (names may not differ only by letter case)", service.Name)
		}
		seen[serviceKey] = struct{}{}
		if len(service.Files) == 0 {
			return fmt.Errorf("service %q has no managed files", service.Name)
		}
		fileNames := map[string]struct{}{}
		for _, file := range service.Files {
			if strings.TrimSpace(file.Path) == "" {
				return fmt.Errorf("service %q has a managed file without a path", service.Name)
			}
			if strings.TrimSpace(file.BackupName) == "" {
				return fmt.Errorf("service %q has a managed file without backup_name", service.Name)
			}
			backupName := filepath.Clean(file.BackupName)
			if filepath.IsAbs(backupName) || !filepath.IsLocal(backupName) {
				return fmt.Errorf("service %q backup_name %q must be relative and stay inside the account backup", service.Name, file.BackupName)
			}
			if _, exists := fileNames[backupName]; exists {
				return fmt.Errorf("service %q has duplicate backup_name %q", service.Name, file.BackupName)
			}
			fileNames[backupName] = struct{}{}
		}
		if len(service.UsageCommand) == 1 && service.UsageCommand[0] == "" {
			return fmt.Errorf("service %q has an empty usage_command", service.Name)
		}
	}
	return nil
}

func (c Config) Service(name string) (ServiceConfig, bool) {
	for _, service := range c.Services {
		if service.Name == name {
			return service, true
		}
	}
	return ServiceConfig{}, false
}

func (m MonitorConfig) AutoSwitchEnabled() bool {
	return m.AutoSwitch == nil || *m.AutoSwitch
}

func (m MonitorConfig) SwitchThresholdRatio() float64 {
	if m.SwitchThreshold == nil {
		return defaultAutoSwitchThreshold
	}
	return *m.SwitchThreshold
}

func (m MonitorConfig) MinImprovementRatio() float64 {
	if m.MinImprovement == nil {
		return defaultAutoSwitchMinImprovement
	}
	return *m.MinImprovement
}

func (m MonitorConfig) CooldownDuration() time.Duration {
	if m.Cooldown == nil {
		return defaultAutoSwitchCooldown
	}
	return m.Cooldown.Duration
}

func (d *Duration) UnmarshalJSON(data []byte) error {
	var raw any
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	switch value := raw.(type) {
	case string:
		parsed, err := time.ParseDuration(value)
		if err != nil {
			return err
		}
		d.Duration = parsed
	case float64:
		d.Duration = time.Duration(value * float64(time.Second))
	default:
		return fmt.Errorf("duration must be a string or number, got %T", raw)
	}
	return nil
}

func (d Duration) MarshalJSON() ([]byte, error) {
	return json.Marshal(d.String())
}

func DefaultDataRoot() string {
	if xdg := os.Getenv("XDG_DATA_HOME"); xdg != "" {
		expanded := ExpandPath(xdg)
		if filepath.IsAbs(expanded) {
			return filepath.Join(expanded, "subswapper")
		}
	}
	switch runtime.GOOS {
	case "darwin":
		home, err := os.UserHomeDir()
		if err == nil {
			return filepath.Join(home, "Library", "Application Support", "subswapper")
		}
	case "windows":
		if appData := os.Getenv("LOCALAPPDATA"); appData != "" {
			return filepath.Join(appData, "subswapper")
		}
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ".subswapper"
	}
	return filepath.Join(home, ".local", "share", "subswapper")
}

func DefaultConfigPath() string {
	if configHome := os.Getenv("XDG_CONFIG_HOME"); configHome != "" {
		expanded := ExpandPath(configHome)
		if filepath.IsAbs(expanded) {
			return filepath.Join(expanded, "subswapper", "config.json")
		}
	}
	switch runtime.GOOS {
	case "darwin":
		home, err := os.UserHomeDir()
		if err == nil {
			return filepath.Join(home, "Library", "Application Support", "subswapper", "config.json")
		}
	case "windows":
		if appData := os.Getenv("APPDATA"); appData != "" {
			return filepath.Join(appData, "subswapper", "config.json")
		}
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(".config", "subswapper", "config.json")
	}
	return filepath.Join(home, ".config", "subswapper", "config.json")
}

func ExpandPath(path string) string {
	expanded := os.ExpandEnv(path)
	if expanded == "~" {
		home, err := os.UserHomeDir()
		if err == nil {
			return home
		}
	}
	if strings.HasPrefix(expanded, "~/") {
		home, err := os.UserHomeDir()
		if err == nil {
			return filepath.Join(home, strings.TrimPrefix(expanded, "~/"))
		}
	}
	return expanded
}

func defaultManagedFiles(kind string) []ManagedFile {
	switch strings.ToLower(kind) {
	case "claude", "claude-code":
		return defaultClaudeFiles()
	case "codex":
		return []ManagedFile{
			requiredFile(codexHomeFile("auth.json"), "auth.json"),
		}
	default:
		return nil
	}
}

func defaultClaudeFiles() []ManagedFile {
	configHome := os.Getenv("CLAUDE_CONFIG_DIR")
	fallbackDir := configHome
	if configHome == "" {
		configHome = "~/.claude"
		fallbackDir = "~"
	}
	globalConfig := filepath.Join(configHome, ".config.json")
	if _, err := os.Stat(ExpandPath(globalConfig)); err != nil {
		globalConfig = filepath.Join(fallbackDir, ".claude.json")
	}
	return []ManagedFile{
		requiredFile(filepath.Join(configHome, ".credentials.json"), "credentials.json"),
		optionalFile(globalConfig, "claude.json"),
	}
}

func codexHomeFile(name string) string {
	home := os.Getenv("CODEX_HOME")
	if home == "" {
		home = "~/.codex"
	}
	return filepath.Join(home, name)
}

func requiredFile(path, backupName string) ManagedFile {
	value := true
	return ManagedFile{Path: path, BackupName: backupName, Required: &value}
}

func optionalFile(path, backupName string) ManagedFile {
	value := false
	return ManagedFile{Path: path, BackupName: backupName, Required: &value}
}

func (f ManagedFile) IsRequired() bool {
	return f.Required == nil || *f.Required
}
