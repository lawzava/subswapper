package subswapper

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDurationUnmarshalString(t *testing.T) {
	var got Duration
	if err := json.Unmarshal([]byte(`"30s"`), &got); err != nil {
		t.Fatal(err)
	}
	if got.Duration != 30*time.Second {
		t.Fatalf("expected 30s, got %s", got.Duration)
	}
}

func TestValidateRejectsUnsafeBackupName(t *testing.T) {
	cfg := Config{
		Services: []ServiceConfig{
			{
				Name: "claude",
				Kind: "custom",
				Files: []ManagedFile{
					{Path: "/tmp/source", BackupName: "../outside"},
				},
			},
		},
	}

	cfg.ApplyDefaults()
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected validation error")
	}
}

func TestWriteSampleConfigCreatesParentDirectory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "config.json")
	if err := WriteSampleConfig(path); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("expected 0600 permissions, got %o", got)
	}
}

func TestDefaultMonitorIntervalIsFiveMinutes(t *testing.T) {
	cfg := Config{Services: []ServiceConfig{{
		Name:  "svc",
		Kind:  "custom",
		Files: []ManagedFile{{Path: "/tmp/source", BackupName: "source"}},
	}}}
	cfg.ApplyDefaults()
	if got := cfg.Monitor.Interval.Duration; got != 5*time.Minute {
		t.Fatalf("default interval = %s, want 5m", got)
	}
}

func TestSampleConfigUsesFiveMinuteInterval(t *testing.T) {
	if !strings.Contains(sampleConfig, `"interval": "5m"`) {
		t.Fatalf("sample config does not use five minutes:\n%s", sampleConfig)
	}
}

func TestValidateSharedRuntimeHomeRequiresClaudeHomeModeAndAbsolutePath(t *testing.T) {
	dir := t.TempDir()
	valid := Config{Services: []ServiceConfig{{
		Name:              "claude",
		Kind:              "claude",
		AccountMode:       AccountModeHome,
		SharedRuntimeHome: filepath.Join(dir, "shared"),
	}}}
	valid.ApplyDefaults()
	if err := valid.Validate(); err != nil {
		t.Fatalf("valid shared runtime home rejected: %v", err)
	}

	for _, test := range []struct {
		name    string
		service ServiceConfig
	}{
		{
			name: "codex",
			service: ServiceConfig{
				Name:              "codex",
				Kind:              "codex",
				AccountMode:       AccountModeHome,
				SharedRuntimeHome: filepath.Join(dir, "shared"),
			},
		},
		{
			name: "bundle mode",
			service: ServiceConfig{
				Name:              "claude",
				Kind:              "claude",
				AccountMode:       AccountModeBundle,
				SharedRuntimeHome: filepath.Join(dir, "shared"),
			},
		},
		{
			name: "relative path",
			service: ServiceConfig{
				Name:              "claude",
				Kind:              "claude",
				AccountMode:       AccountModeHome,
				SharedRuntimeHome: "relative/shared",
			},
		},
		{
			name: "blank path",
			service: ServiceConfig{
				Name:              "claude",
				Kind:              "claude",
				AccountMode:       AccountModeHome,
				SharedRuntimeHome: " ",
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			cfg := Config{Services: []ServiceConfig{test.service}}
			cfg.ApplyDefaults()
			if err := cfg.Validate(); err == nil {
				t.Fatal("invalid shared runtime home was accepted")
			}
		})
	}
}
