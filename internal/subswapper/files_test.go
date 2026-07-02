package subswapper

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestCaptureAndSwitchAccountCopiesCredentialBundles(t *testing.T) {
	dir := t.TempDir()
	active := filepath.Join(dir, "active-auth.json")
	if err := os.WriteFile(active, []byte(`{"email":"first@example.com","token":"one"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := testConfig(dir, active)

	account, err := CaptureAccount(cfg, "codex", "first", "")
	if err != nil {
		t.Fatal(err)
	}
	if account.Email != "first@example.com" {
		t.Fatalf("expected inferred email, got %q", account.Email)
	}
	if err := os.WriteFile(active, []byte(`{"email":"second@example.com","token":"two"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := CaptureAccount(cfg, "codex", "second", ""); err != nil {
		t.Fatal(err)
	}

	if err := SwitchAccount(cfg, "codex", "first"); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(active)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != `{"email":"first@example.com","token":"one"}` {
		t.Fatalf("unexpected active auth content %q", string(data))
	}
}

func TestMonitorOnceSwitchesEachServiceToLeastUsedAccount(t *testing.T) {
	dir := t.TempDir()
	claudeActive := filepath.Join(dir, "claude-active.json")
	codexActive := filepath.Join(dir, "codex-active.json")
	if err := os.WriteFile(claudeActive, []byte("claude-a"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(codexActive, []byte("codex-a"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := Config{
		BackupRoot: filepath.Join(dir, "backups"),
		StatePath:  filepath.Join(dir, "state.json"),
		Services: []ServiceConfig{
			testService("claude", claudeActive),
			testService("codex", codexActive),
		},
	}
	cfg.ApplyDefaults()
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}

	captureWithUsage(t, cfg, "claude", claudeActive, "claude-a", "a", 90, 80)
	captureWithUsage(t, cfg, "claude", claudeActive, "claude-b", "b", 10, 20)
	captureWithUsage(t, cfg, "codex", codexActive, "codex-a", "a", 60, 90)
	captureWithUsage(t, cfg, "codex", codexActive, "codex-b", "b", 5, 10)
	if err := SwitchAccount(cfg, "claude", "a"); err != nil {
		t.Fatal(err)
	}
	if err := SwitchAccount(cfg, "codex", "a"); err != nil {
		t.Fatal(err)
	}
	setServiceLastSwitchedAt(t, cfg, "claude", time.Now().Add(-defaultAutoSwitchCooldown-time.Minute))
	setServiceLastSwitchedAt(t, cfg, "codex", time.Now().Add(-defaultAutoSwitchCooldown-time.Minute))

	result := MonitorOnce(testContext(t), cfg, true)
	if err := errors.Join(result.Errors...); err != nil {
		t.Fatal(err)
	}
	if len(result.Switches) != 2 {
		t.Fatalf("expected 2 switches, got %d", len(result.Switches))
	}

	assertFileContent(t, claudeActive, "claude-b")
	assertFileContent(t, codexActive, "codex-b")
}

func TestMonitorOnceDoesNotSwitchBelowThreshold(t *testing.T) {
	dir := t.TempDir()
	active := filepath.Join(dir, "claude-active.json")
	if err := os.WriteFile(active, []byte("claude-a"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := Config{
		BackupRoot: filepath.Join(dir, "backups"),
		StatePath:  filepath.Join(dir, "state.json"),
		Services: []ServiceConfig{
			testService("claude", active),
		},
	}
	cfg.ApplyDefaults()
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}

	captureWithUsage(t, cfg, "claude", active, "claude-a", "a", 80, 80)
	captureWithUsage(t, cfg, "claude", active, "claude-b", "b", 10, 20)
	if err := SwitchAccount(cfg, "claude", "a"); err != nil {
		t.Fatal(err)
	}
	setServiceLastSwitchedAt(t, cfg, "claude", time.Now().Add(-defaultAutoSwitchCooldown-time.Minute))

	result := MonitorOnce(testContext(t), cfg, true)
	if err := errors.Join(result.Errors...); err != nil {
		t.Fatal(err)
	}
	if len(result.Switches) != 0 {
		t.Fatalf("expected no switches below threshold, got %d", len(result.Switches))
	}
	assertFileContent(t, active, "claude-a")
}

func TestMonitorOnceDoesNotSwitchDuringCooldown(t *testing.T) {
	dir := t.TempDir()
	active := filepath.Join(dir, "claude-active.json")
	if err := os.WriteFile(active, []byte("claude-a"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := Config{
		BackupRoot: filepath.Join(dir, "backups"),
		StatePath:  filepath.Join(dir, "state.json"),
		Services: []ServiceConfig{
			testService("claude", active),
		},
	}
	cfg.ApplyDefaults()
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}

	captureWithUsage(t, cfg, "claude", active, "claude-a", "a", 95, 95)
	captureWithUsage(t, cfg, "claude", active, "claude-b", "b", 10, 20)
	if err := SwitchAccount(cfg, "claude", "a"); err != nil {
		t.Fatal(err)
	}

	result := MonitorOnce(testContext(t), cfg, true)
	if err := errors.Join(result.Errors...); err != nil {
		t.Fatal(err)
	}
	if len(result.Switches) != 0 {
		t.Fatalf("expected no switches during cooldown, got %d", len(result.Switches))
	}
	assertFileContent(t, active, "claude-a")
}

func TestMonitorOnceDoesNotSwitchForSmallImprovement(t *testing.T) {
	dir := t.TempDir()
	active := filepath.Join(dir, "claude-active.json")
	if err := os.WriteFile(active, []byte("claude-a"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := Config{
		BackupRoot: filepath.Join(dir, "backups"),
		StatePath:  filepath.Join(dir, "state.json"),
		Services: []ServiceConfig{
			testService("claude", active),
		},
	}
	cfg.ApplyDefaults()
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}

	captureWithUsage(t, cfg, "claude", active, "claude-a", "a", 95, 95)
	captureWithUsage(t, cfg, "claude", active, "claude-b", "b", 88, 88)
	if err := SwitchAccount(cfg, "claude", "a"); err != nil {
		t.Fatal(err)
	}
	setServiceLastSwitchedAt(t, cfg, "claude", time.Now().Add(-defaultAutoSwitchCooldown-time.Minute))

	result := MonitorOnce(testContext(t), cfg, true)
	if err := errors.Join(result.Errors...); err != nil {
		t.Fatal(err)
	}
	if len(result.Switches) != 0 {
		t.Fatalf("expected no switches for small improvement, got %d", len(result.Switches))
	}
	assertFileContent(t, active, "claude-a")
}

func TestMonitorOnceSwitchesWhenFableWeeklyHitsThreshold(t *testing.T) {
	dir := t.TempDir()
	active := filepath.Join(dir, "claude-active.json")
	if err := os.WriteFile(active, []byte("claude-a"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := Config{
		BackupRoot: filepath.Join(dir, "backups"),
		StatePath:  filepath.Join(dir, "state.json"),
		Services: []ServiceConfig{
			testService("claude", active),
		},
	}
	cfg.ApplyDefaults()
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}

	fableHeavy := usageForTest(10, 10)
	fableHeavy.FableWeekly = LimitWindow{Pct: PtrFloat64(90)}
	captureWithUsageSnapshot(t, cfg, "claude", active, "claude-a", "a", fableHeavy)
	captureWithUsage(t, cfg, "claude", active, "claude-b", "b", 20, 20)
	if err := SwitchAccount(cfg, "claude", "a"); err != nil {
		t.Fatal(err)
	}
	setServiceLastSwitchedAt(t, cfg, "claude", time.Now().Add(-defaultAutoSwitchCooldown-time.Minute))

	result := MonitorOnce(testContext(t), cfg, true)
	if err := errors.Join(result.Errors...); err != nil {
		t.Fatal(err)
	}
	if len(result.Switches) != 1 {
		t.Fatalf("expected Fable-triggered switch, got %d", len(result.Switches))
	}
	assertFileContent(t, active, "claude-b")
}

func TestCaptureRemovesStaleOptionalBackup(t *testing.T) {
	dir := t.TempDir()
	required := filepath.Join(dir, "required.json")
	optional := filepath.Join(dir, "optional.json")
	if err := os.WriteFile(required, []byte("required"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(optional, []byte("optional"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := Config{
		BackupRoot: filepath.Join(dir, "backups"),
		StatePath:  filepath.Join(dir, "state.json"),
		Services: []ServiceConfig{
			{
				Name: "claude",
				Kind: "custom",
				Files: []ManagedFile{
					requiredFile(required, "required.json"),
					optionalFile(optional, "optional.json"),
				},
			},
		},
	}
	cfg.ApplyDefaults()
	if _, err := CaptureAccount(cfg, "claude", "work", ""); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(optional); err != nil {
		t.Fatal(err)
	}
	if _, err := CaptureAccount(cfg, "claude", "work", ""); err != nil {
		t.Fatal(err)
	}

	backupPath := filepath.Join(AccountDir(cfg, "claude", "work"), "optional.json")
	if _, err := os.Stat(backupPath); !os.IsNotExist(err) {
		t.Fatalf("expected stale optional backup to be removed, got %v", err)
	}
}

func TestRemoveAccountDeletesStateAndBackup(t *testing.T) {
	dir := t.TempDir()
	active := filepath.Join(dir, "active-auth.json")
	if err := os.WriteFile(active, []byte(`{"email":"first@example.com","token":"one"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := testConfig(dir, active)
	if _, err := CaptureAccount(cfg, "codex", "first", ""); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(active, []byte(`{"email":"second@example.com","token":"two"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := CaptureAccount(cfg, "codex", "second", ""); err != nil {
		t.Fatal(err)
	}
	if err := SwitchAccount(cfg, "codex", "second"); err != nil {
		t.Fatal(err)
	}

	if err := RemoveAccount(cfg, "codex", "first", false); err != nil {
		t.Fatal(err)
	}
	state, err := LoadState(cfg.StatePath)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := state.Service("codex").Accounts["first"]; ok {
		t.Fatal("expected first account to be removed from state")
	}
	if _, err := os.Stat(AccountDir(cfg, "codex", "first")); !os.IsNotExist(err) {
		t.Fatalf("expected backup directory removal, got %v", err)
	}
}

func TestRemoveActiveAccountRequiresForce(t *testing.T) {
	dir := t.TempDir()
	active := filepath.Join(dir, "active-auth.json")
	if err := os.WriteFile(active, []byte(`{"email":"first@example.com","token":"one"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := testConfig(dir, active)
	if _, err := CaptureAccount(cfg, "codex", "first", ""); err != nil {
		t.Fatal(err)
	}

	if err := RemoveAccount(cfg, "codex", "first", false); err == nil {
		t.Fatal("expected active account removal to fail without force")
	}
	if err := RemoveAccount(cfg, "codex", "first", true); err != nil {
		t.Fatal(err)
	}
	state, err := LoadState(cfg.StatePath)
	if err != nil {
		t.Fatal(err)
	}
	if state.Service("codex").ActiveAccount != "" {
		t.Fatalf("expected active account to be cleared, got %q", state.Service("codex").ActiveAccount)
	}
}

func TestSafeNameAvoidsCollisionsForUnsafeNames(t *testing.T) {
	if safeName("a/b") == safeName("a_b") {
		t.Fatal("expected unsafe names to include a collision-resistant suffix")
	}
}

func captureWithUsage(t *testing.T, cfg Config, serviceName, activePath, content, account string, fiveHourPct, weeklyPct float64) {
	t.Helper()
	captureWithUsageSnapshot(t, cfg, serviceName, activePath, content, account, usageForTest(fiveHourPct, weeklyPct))
}

func captureWithUsageSnapshot(t *testing.T, cfg Config, serviceName, activePath, content, account string, usage UsageSnapshot) {
	t.Helper()
	if err := os.WriteFile(activePath, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := CaptureAccount(cfg, serviceName, account, account+"@example.com"); err != nil {
		t.Fatal(err)
	}
	state, err := LoadState(cfg.StatePath)
	if err != nil {
		t.Fatal(err)
	}
	service := state.Service(serviceName)
	updated := service.Accounts[account]
	updated.Usage = usage
	service.Accounts[account] = updated
	if err := SaveState(cfg.StatePath, state); err != nil {
		t.Fatal(err)
	}
}

func setServiceLastSwitchedAt(t *testing.T, cfg Config, serviceName string, when time.Time) {
	t.Helper()
	state, err := LoadState(cfg.StatePath)
	if err != nil {
		t.Fatal(err)
	}
	state.Service(serviceName).LastSwitchedAt = when.UTC()
	if err := SaveState(cfg.StatePath, state); err != nil {
		t.Fatal(err)
	}
}

func testConfig(dir, active string) Config {
	cfg := Config{
		BackupRoot: filepath.Join(dir, "backups"),
		StatePath:  filepath.Join(dir, "state.json"),
		Services: []ServiceConfig{
			testService("codex", active),
		},
	}
	cfg.ApplyDefaults()
	return cfg
}

func testService(name, active string) ServiceConfig {
	return ServiceConfig{
		Name: name,
		Kind: "custom",
		Files: []ManagedFile{
			requiredFile(active, "auth.json"),
		},
	}
}

func usageForTest(fiveHourPct, weeklyPct float64) UsageSnapshot {
	return UsageSnapshot{
		FiveHour: LimitWindow{
			Pct: PtrFloat64(fiveHourPct),
		},
		Weekly: LimitWindow{
			Pct: PtrFloat64(weeklyPct),
		},
	}
}

func assertFileContent(t *testing.T, path, want string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != want {
		t.Fatalf("expected %q, got %q", want, string(data))
	}
}
