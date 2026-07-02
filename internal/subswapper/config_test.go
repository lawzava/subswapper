package subswapper

import (
	"encoding/json"
	"os"
	"path/filepath"
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
