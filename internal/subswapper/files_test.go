package subswapper

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestAcquireStateLockHonorsContext(t *testing.T) {
	dir := t.TempDir()
	cfg := Config{StatePath: filepath.Join(dir, "state.json")}
	first, err := AcquireStateLock(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Release()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	_, err = AcquireStateLock(ctx, cfg)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected deadline exceeded, got %v", err)
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

	assertActive(t, cfg, "claude", "b")
	assertActive(t, cfg, "codex", "b")
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
	assertActive(t, cfg, "claude", "a")
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
	assertActive(t, cfg, "claude", "a")
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
	assertActive(t, cfg, "claude", "a")
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
	assertActive(t, cfg, "claude", "b")
}

func TestRemoveAccountDeletesStateAndBackup(t *testing.T) {
	dir := t.TempDir()
	active := filepath.Join(dir, "active-auth.json")
	if err := os.WriteFile(active, []byte(`{"email":"first@example.com","token":"one"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := testConfig(dir, active)
	registerHomeAccount(t, cfg, "codex", "first", "one")
	registerHomeAccount(t, cfg, "codex", "second", "two")
	registerHomeAccount(t, cfg, "codex", "third", "three")

	// Unregistering keeps the home; -delete-home erases it.
	if err := RemoveAccount(cfg, "codex", "first", false); err != nil {
		t.Fatal(err)
	}
	if err := RemoveAccountWithOptions(cfg, "codex", "second", false, true); err != nil {
		t.Fatal(err)
	}
	state, err := LoadState(cfg.StatePath)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := state.Service("codex").Accounts["first"]; ok {
		t.Fatal("expected first account to be removed from state")
	}
	if _, ok := state.Service("codex").Accounts["second"]; ok {
		t.Fatal("expected second account to be removed from state")
	}
	if _, err := os.Stat(AccountDir(cfg, "codex", "first")); err != nil {
		t.Fatalf("unregistering deleted the home: %v", err)
	}
	if _, err := os.Stat(AccountDir(cfg, "codex", "second")); !os.IsNotExist(err) {
		t.Fatalf("expected home removal, got %v", err)
	}
}

func TestRemoveActiveAccountRequiresForce(t *testing.T) {
	dir := t.TempDir()
	active := filepath.Join(dir, "active-auth.json")
	if err := os.WriteFile(active, []byte(`{"email":"first@example.com","token":"one"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := testConfig(dir, active)
	registerHomeAccount(t, cfg, "codex", "first", "one")

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
	registerHomeAccount(t, cfg, serviceName, account, content)
	home := AccountDir(cfg, serviceName, account)
	data, err := json.Marshal(usage)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "usage.json"), data, 0o600); err != nil {
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

// registerHomeAccount registers account with login content in its home and
// selects it, as the last registration used to.
func registerHomeAccount(t *testing.T, cfg Config, serviceName, account, content string) {
	t.Helper()
	// Tests that never validate their config still need home-mode defaults.
	local := cfg
	local.Services = append([]ServiceConfig(nil), cfg.Services...)
	local.ApplyDefaults()
	if _, _, err := CreateAccountHome(local, serviceName, account, account+"@example.com"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(AccountDir(cfg, serviceName, account), "auth.json"), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	state, err := LoadState(cfg.StatePath)
	if err != nil {
		t.Fatal(err)
	}
	state.Service(serviceName).ActiveAccount = account
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

// testService is a Codex-kind account-home service whose usage comes from
// a usage.json each fixture account keeps in its home, so monitor tests stay
// deterministic without a codex binary. active is accepted for older
// callers; account homes have no live credential file.
func testService(name, _ string) ServiceConfig {
	return ServiceConfig{
		Name:         name,
		Kind:         "codex",
		UsageCommand: []string{"sh", "-c", `cat "$SUBSWAPPER_ACCOUNT_DIR/usage.json"`},
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

func assertActive(t *testing.T, cfg Config, service, want string) {
	t.Helper()
	state, err := LoadState(cfg.StatePath)
	if err != nil {
		t.Fatal(err)
	}
	if got := state.Service(service).ActiveAccount; got != want {
		t.Fatalf("%s selects %q, want %q", service, got, want)
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
