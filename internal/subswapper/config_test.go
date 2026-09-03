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
			name: "custom kind",
			service: ServiceConfig{
				Name:              "other",
				Kind:              "custom",
				Files:             []ManagedFile{requiredFile(filepath.Join(dir, "other.json"), "other.json")},
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

func TestConfigValidatesProxyListen(t *testing.T) {
	for _, test := range []struct {
		name    string
		service ServiceConfig
		wantErr string
	}{
		{name: "loopback ok", service: ServiceConfig{Name: "claude", Kind: "claude", ProxyListen: "127.0.0.1:7878"}},
		{name: "localhost ok", service: ServiceConfig{Name: "claude", Kind: "claude", ProxyListen: "localhost:7878"}},
		{name: "public host", service: ServiceConfig{Name: "claude", Kind: "claude", ProxyListen: "0.0.0.0:7878"}, wantErr: "loopback"},
		{name: "no port", service: ServiceConfig{Name: "claude", Kind: "claude", ProxyListen: "127.0.0.1"}, wantErr: "host:port"},
		{name: "ephemeral port", service: ServiceConfig{Name: "claude", Kind: "claude", ProxyListen: "127.0.0.1:0"}, wantErr: "port"},
		{name: "custom kind", service: ServiceConfig{Name: "other", Kind: "custom", Files: []ManagedFile{requiredFile("/tmp/other.json", "other.json")}, ProxyListen: "127.0.0.1:7878"}, wantErr: "requires Claude or Codex"},
		{name: "upstream without listen", service: ServiceConfig{Name: "claude", Kind: "claude", ProxyUpstream: "https://example.com"}, wantErr: "requires proxy_listen"},
		{name: "upstream with path", service: ServiceConfig{Name: "claude", Kind: "claude", ProxyListen: "127.0.0.1:7878", ProxyUpstream: "https://example.com/v1"}, wantErr: "origin"},
	} {
		t.Run(test.name, func(t *testing.T) {
			cfg := Config{Services: []ServiceConfig{test.service}}
			cfg.ApplyDefaults()
			err := cfg.Validate()
			if test.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				if !cfg.Services[0].ClaudeProxyEnabled() {
					t.Fatal("proxy not reported enabled")
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("err = %v, want %q", err, test.wantErr)
			}
		})
	}
}

func TestConfigNativeRuntimeHomeRequiresProxy(t *testing.T) {
	cfg := Config{Services: []ServiceConfig{{Name: "claude", Kind: "claude", SharedRuntimeHome: NativeRuntimeHome}}}
	cfg.ApplyDefaults()
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "proxy_listen") {
		t.Fatalf("err = %v", err)
	}
	cfg.Services[0].ProxyListen = "127.0.0.1:7878"
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	t.Setenv("HOME", t.TempDir())
	if got := RuntimeHome(cfg, cfg.Services[0], "work"); got != NativeClaudeHome() || !strings.HasSuffix(got, ".claude") {
		t.Fatalf("runtime home = %q", got)
	}
	t.Setenv("CLAUDE_CONFIG_DIR", "/custom/home")
	if got := RuntimeHome(cfg, cfg.Services[0], "work"); got != "/custom/home" {
		t.Fatalf("runtime home = %q", got)
	}
}
