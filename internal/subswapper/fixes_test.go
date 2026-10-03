package subswapper

import (
	"context"
	"errors"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestUsageSnapshotScoresAvailableWindows(t *testing.T) {
	tests := []struct {
		name  string
		usage UsageSnapshot
		want  float64
	}{
		{name: "weekly", usage: UsageSnapshot{Weekly: LimitWindow{Pct: PtrFloat64(30)}}, want: 0.30},
		{name: "five hour", usage: UsageSnapshot{FiveHour: LimitWindow{Pct: PtrFloat64(20)}}, want: 0.20},
		{name: "fable", usage: UsageSnapshot{FableWeekly: LimitWindow{Pct: PtrFloat64(40)}}, want: 0.40},
		{name: "empty", usage: UsageSnapshot{}, want: math.Inf(1)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.usage.Score(); got != tt.want {
				t.Fatalf("Score() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestCustomUsageCommandStillRequiresCoreWindows(t *testing.T) {
	dir := t.TempDir()
	cfg := Config{BackupRoot: filepath.Join(dir, "backups"), StatePath: filepath.Join(dir, "state.json")}
	service := ServiceConfig{
		Name:         "svc",
		Kind:         "custom",
		UsageCommand: []string{"sh", "-c", `echo '{"weekly":{"pct":20}}'`},
	}
	_, err := runUsageCommand(testContext(t), cfg, service, AccountState{Name: "a"})
	if err == nil || !strings.Contains(err.Error(), "missing limits") {
		t.Fatalf("expected missing core limits error, got %v", err)
	}
}

func TestProbeErrorsAreSanitizedAndBackedOff(t *testing.T) {
	dir := t.TempDir()
	live := filepath.Join(dir, "active.json")
	cfg := Config{
		BackupRoot: filepath.Join(dir, "backups"),
		StatePath:  filepath.Join(dir, "state.json"),
		Monitor:    MonitorConfig{Interval: Duration{Duration: 5 * time.Minute}},
		Services: []ServiceConfig{
			{
				Name: "svc",
				Kind: "codex",
				UsageCommand: []string{"sh", "-c",
					`{ printf '\033[31mboom\n  repeated   whitespace '; i=0; while [ "$i" -lt 600 ]; do printf x; i=$((i+1)); done; printf '\033[0m'; } >&2; exit 1`},
			},
		},
	}
	captureWithUsage(t, cfg, "svc", live, "credential", "a", 10, 20)
	state, err := LoadState(cfg.StatePath)
	if err != nil {
		t.Fatal(err)
	}
	status := CollectService(testContext(t), cfg, state, cfg.Services[0]).Accounts[0]
	if status.Account.LastProbeError == "" {
		t.Fatal("expected persisted probe summary")
	}
	if strings.ContainsAny(status.Account.LastProbeError, "\n\r\x1b") {
		t.Fatalf("probe summary contains control characters: %q", status.Account.LastProbeError)
	}
	if strings.Contains(status.Account.LastProbeError, "  ") {
		t.Fatalf("probe summary contains repeated whitespace: %q", status.Account.LastProbeError)
	}
	if len(status.Account.LastProbeError) > 512 {
		t.Fatalf("probe summary has %d bytes, want at most 512", len(status.Account.LastProbeError))
	}
	if !status.Account.FetchBackoffUntil.After(time.Now().Add(4 * time.Minute)) {
		t.Fatalf("expected transient backoff, got %s", status.Account.FetchBackoffUntil)
	}
}

func TestStatusProbeDoesNotHoldStateLock(t *testing.T) {
	dir := t.TempDir()
	live := filepath.Join(dir, "active.json")
	started := filepath.Join(dir, "started")
	release := filepath.Join(dir, "release")
	cfg := Config{
		BackupRoot: filepath.Join(dir, "backups"),
		StatePath:  filepath.Join(dir, "state.json"),
		Services: []ServiceConfig{
			{
				Name:         "svc",
				Kind:         "codex",
				UsageCommand: []string{"sh", "-c", `touch "$1"; while [ ! -f "$2" ]; do sleep 0.01; done; echo '{"five_hour":{"pct":10},"weekly":{"pct":20}}'`, "probe", started, release},
			},
		},
	}
	captureWithUsage(t, cfg, "svc", live, "credential", "a", 10, 20)
	statusDone := make(chan error, 1)
	go func() {
		_, err := StatusOnce(context.Background(), cfg)
		statusDone <- err
	}()
	waitForFile(t, started)
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	lock, err := AcquireStateLock(ctx, cfg)
	if err != nil {
		_ = os.WriteFile(release, nil, 0o600)
		<-statusDone
		t.Fatalf("state lock remained held during provider probe: %v", err)
	}
	lock.Release()
	if err := os.WriteFile(release, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := <-statusDone; err != nil {
		t.Fatal(err)
	}
}

func TestMergeProbeStateSkipsRecapturedAccount(t *testing.T) {
	oldAdded := time.Now().Add(-time.Hour).UTC()
	newAdded := time.Now().UTC()
	probed := NewState()
	probed.Service("svc").Accounts["a"] = AccountState{
		Name:           "a",
		AddedAt:        oldAdded,
		Usage:          usageForTest(99, 99),
		LastProbeError: "stale result",
	}
	current := NewState()
	current.Service("svc").Accounts["a"] = AccountState{
		Name:    "a",
		AddedAt: newAdded,
		Usage:   usageForTest(10, 20),
	}

	mergeProbeState(current, probed)
	account := current.Service("svc").Accounts["a"]
	if account.LastProbeError != "" {
		t.Fatalf("stale probe error was merged: %q", account.LastProbeError)
	}
	if score := account.Usage.Score(); score != 0.20 {
		t.Fatalf("stale usage was merged: %v", score)
	}
}

func TestConcurrentActiveChangeSuppressesAutoSwitch(t *testing.T) {
	dir := t.TempDir()
	live := filepath.Join(dir, "active.json")
	started := filepath.Join(dir, "started")
	release := filepath.Join(dir, "release")
	command := `if [ "$SUBSWAPPER_ACCOUNT" = a ]; then touch "$1"; while [ ! -f "$2" ]; do sleep 0.01; done; pct=95; else pct=10; fi; printf '{"five_hour":{"pct":%s},"weekly":{"pct":%s}}\n' "$pct" "$pct"`
	cfg := Config{
		BackupRoot: filepath.Join(dir, "backups"),
		StatePath:  filepath.Join(dir, "state.json"),
		Services: []ServiceConfig{
			{
				Name:         "svc",
				Kind:         "codex",
				UsageCommand: []string{"sh", "-c", command, "probe", started, release},
			},
		},
	}
	captureWithUsage(t, cfg, "svc", live, "account-a", "a", 95, 95)
	captureWithUsage(t, cfg, "svc", live, "account-b", "b", 10, 10)
	if err := SwitchAccount(cfg, "svc", "a"); err != nil {
		t.Fatal(err)
	}
	setServiceLastSwitchedAt(t, cfg, "svc", time.Now().Add(-defaultAutoSwitchCooldown-time.Minute))

	done := make(chan CycleResult, 1)
	go func() { done <- MonitorOnce(context.Background(), cfg, true) }()
	waitForFile(t, started)
	if err := SwitchAccount(cfg, "svc", "b"); err != nil {
		_ = os.WriteFile(release, nil, 0o600)
		<-done
		t.Fatal(err)
	}
	if err := os.WriteFile(release, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	result := <-done
	if len(result.Switches) != 0 {
		t.Fatalf("stale probe performed switches: %v", result.Switches)
	}
	if len(result.Errors) == 0 || !strings.Contains(result.Errors[0].Error(), "changed during usage probe") {
		t.Fatalf("expected concurrent change diagnostic, got %v", result.Errors)
	}
	state, err := LoadState(cfg.StatePath)
	if err != nil {
		t.Fatal(err)
	}
	if got := state.Service("svc").ActiveAccount; got != "b" {
		t.Fatalf("active account = %q, want b", got)
	}
}

func TestOlderProbeCannotOverwriteNewerProbe(t *testing.T) {
	base := time.Now().UTC()
	current := NewState()
	current.Service("svc").Accounts["a"] = AccountState{Name: "a", AddedAt: base, Usage: usageForTest(50, 50)}
	newer := NewState()
	newer.Service("svc").Accounts["a"] = AccountState{Name: "a", AddedAt: base, Usage: usageForTest(10, 10), LastProbeStartedAt: base.Add(2 * time.Second)}
	older := NewState()
	older.Service("svc").Accounts["a"] = AccountState{Name: "a", AddedAt: base, Usage: usageForTest(95, 95), LastProbeStartedAt: base.Add(time.Second)}

	mergeProbeState(current, newer)
	mergeProbeState(current, older)
	account := current.Service("svc").Accounts["a"]
	if score := account.Usage.Score(); score != 0.10 {
		t.Fatalf("older probe overwrote newer usage: %v", score)
	}
	if serviceStateMatchesSnapshot(current.Service("svc"), older.Service("svc")) {
		t.Fatal("older probe remained eligible for automatic switching")
	}
}

func TestActiveAccountABAChangeSuppressesAutoSwitch(t *testing.T) {
	base := time.Now().UTC()
	snapshot := &ServiceState{
		ActiveAccount:  "a",
		LastSwitchedAt: base,
		Accounts: map[string]AccountState{
			"a": {Name: "a", AddedAt: base, LastProbeStartedAt: base},
			"b": {Name: "b", AddedAt: base, LastProbeStartedAt: base},
		},
	}
	current := &ServiceState{
		ActiveAccount:  "a",
		LastSwitchedAt: base.Add(time.Second),
		Accounts: map[string]AccountState{
			"a": {Name: "a", AddedAt: base, LastProbeStartedAt: base},
			"b": {Name: "b", AddedAt: base, LastProbeStartedAt: base},
		},
	}
	if serviceStateMatchesSnapshot(current, snapshot) {
		t.Fatal("A to B to A switch during probe remained eligible for automatic switching")
	}
}

func TestSanitizeProbeErrorRedactsCredentials(t *testing.T) {
	err := errors.New(`request failed: Authorization: Bearer bearer-secret refresh_token=refresh-secret "access_token":"access-secret" OPENAI_API_KEY=key-secret`)
	summary := sanitizeProbeError(err)
	for _, secret := range []string{"bearer-secret", "refresh-secret", "access-secret", "key-secret"} {
		if strings.Contains(summary, secret) {
			t.Fatalf("summary leaked %q: %q", secret, summary)
		}
	}
}

func TestCodexStderrIsNotPersisted(t *testing.T) {
	dir := t.TempDir()
	fakeCodex := filepath.Join(dir, "codex")
	script := "#!/bin/sh\nIFS= read -r line\nprintf '%s\\n' 'diagnostic opaque-codex-secret' >&2\nexit 1\n"
	if err := os.WriteFile(fakeCodex, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	oldCommand := codexCommand
	codexCommand = fakeCodex
	t.Cleanup(func() { codexCommand = oldCommand })

	cfg := Config{BackupRoot: filepath.Join(dir, "backups"), StatePath: filepath.Join(dir, "state.json"), Services: []ServiceConfig{{Name: "codex", Kind: "codex"}}}
	cfg.ApplyDefaults()
	service := cfg.Services[0]
	registerHomeAccount(t, cfg, "codex", "a", `{"auth_mode":"chatgpt","tokens":{"access_token":"test-token","refresh_token":"r","account_id":"account-a"}}`)
	state, err := LoadState(cfg.StatePath)
	if err != nil {
		t.Fatal(err)
	}
	CollectService(context.Background(), cfg, state, service)
	if got := state.Service("codex").Accounts["a"].LastProbeError; strings.Contains(got, "opaque-codex-secret") {
		t.Fatalf("app-server stderr persisted: %q", got)
	}
}

func TestCustomCommandDiagnosticsAreNotPersisted(t *testing.T) {
	dir := t.TempDir()
	cfg := Config{BackupRoot: filepath.Join(dir, "backups"), StatePath: filepath.Join(dir, "state.json"), Services: []ServiceConfig{{
		Name:         "custom",
		Kind:         "codex",
		UsageCommand: []string{"sh", "-c", "printf '%s\\n' 'diagnostic opaque-command-secret' >&2; exit 1"},
	}}}
	cfg.ApplyDefaults()
	service := cfg.Services[0]
	registerHomeAccount(t, cfg, "custom", "a", "credential")
	state, err := LoadState(cfg.StatePath)
	if err != nil {
		t.Fatal(err)
	}
	CollectService(context.Background(), cfg, state, service)
	if got := state.Service("custom").Accounts["a"].LastProbeError; strings.Contains(got, "opaque-command-secret") {
		t.Fatalf("custom command diagnostic persisted: %q", got)
	}
}

func TestLoadStateSanitizesLegacyProbeErrors(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	state := NewState()
	state.Service("codex").Accounts["a"] = AccountState{
		Name:             "a",
		CredentialsError: "stored credentials unusable; body={\n\"error\":\"opaque-legacy-secret\"}",
		LastProbeError:   "\x1b[31mclaude usage API returned 500 Internal Server Error: diagnostic opaque-probe-secret\x1b[0m",
	}
	if err := SaveState(path, state); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadState(path)
	if err != nil {
		t.Fatal(err)
	}
	account := loaded.Service("codex").Accounts["a"]
	for name, value := range map[string]string{
		"credentials": account.CredentialsError,
		"probe":       account.LastProbeError,
	} {
		if strings.ContainsAny(value, "\n\r\x1b") || len(value) > maxProbeErrorBytes {
			t.Fatalf("legacy %s error was not bounded and terminal-safe: %q", name, value)
		}
		if strings.Contains(value, "opaque-") {
			t.Fatalf("legacy %s provider detail survived sanitization: %q", name, value)
		}
	}
	rendered := RenderStatus([]ServiceStatus{{
		Service: ServiceConfig{Name: "codex"},
		Accounts: []AccountStatus{{
			Service: "codex",
			Account: account,
			Reason:  account.LastProbeError,
		}},
	}}, nil, time.Now())
	if strings.Contains(rendered, "opaque-") || strings.Contains(rendered, "\x1b") {
		t.Fatalf("legacy provider detail reached status output: %q", rendered)
	}
}

func TestRenderMonitorEventsReportsTransitionsOnce(t *testing.T) {
	ready := []ServiceStatus{{
		Service:  ServiceConfig{Name: "claude"},
		Accounts: []AccountStatus{{Service: "claude", Account: AccountState{Name: "a"}, Reason: "ready", Selectable: true}},
	}}
	failing := []ServiceStatus{{
		Service:  ServiceConfig{Name: "claude"},
		Accounts: []AccountStatus{{Service: "claude", Account: AccountState{Name: "a"}, Reason: "ready (stale usage from Jul13 00:00: probe failed)", Selectable: true}},
	}}
	first := RenderMonitorEvents(ready, failing, nil)
	if !strings.Contains(first, "claude/a") || !strings.Contains(first, "probe failed") {
		t.Fatalf("missing failure event: %q", first)
	}
	if repeated := RenderMonitorEvents(failing, failing, nil); repeated != "" {
		t.Fatalf("unchanged failure repeated: %q", repeated)
	}
	if recovered := RenderMonitorEvents(failing, ready, nil); !strings.Contains(recovered, "recovered claude/a") {
		t.Fatalf("missing recovery event: %q", recovered)
	}
	if switched := RenderMonitorEvents(ready, ready, []SwitchEvent{{Service: "claude", Account: "b"}}); !strings.Contains(switched, "switched claude to b") {
		t.Fatalf("missing switch event: %q", switched)
	}
}

func waitForFile(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", path)
}

func TestSafeNameNeverEscapesBackupRoot(t *testing.T) {
	for _, name := range []string{".", "..", "..."} {
		got := safeName(name)
		if got == name {
			t.Fatalf("expected %q to be rewritten, got %q", name, got)
		}
		if !strings.HasPrefix(got, "account-") {
			t.Fatalf("expected hashed fallback for %q, got %q", name, got)
		}
	}
	cfg := Config{BackupRoot: "/backups"}
	dir := filepath.Clean(AccountDir(cfg, "codex", ".."))
	if !strings.HasPrefix(dir, filepath.Clean("/backups/codex")+string(os.PathSeparator)) {
		t.Fatalf("AccountDir escaped the service directory: %s", dir)
	}
}

func TestSwitchAccountRefusesDisabledService(t *testing.T) {
	dir := t.TempDir()
	active := filepath.Join(dir, "active-auth.json")
	cfg := testConfig(dir, active)
	captureWithUsage(t, cfg, "codex", active, "a1", "a", 10, 10)
	cfg.Services[0].Disabled = true
	if err := SwitchAccount(cfg, "codex", "a"); err == nil {
		t.Fatal("expected switch on disabled service to fail")
	}
}

func TestSwitchBestSkipsDisabledAndEmptyServices(t *testing.T) {
	dir := t.TempDir()
	alphaActive := filepath.Join(dir, "alpha-active.json")
	betaActive := filepath.Join(dir, "beta-active.json")
	cfg := Config{
		BackupRoot: filepath.Join(dir, "backups"),
		StatePath:  filepath.Join(dir, "state.json"),
		Services: []ServiceConfig{
			testService("alpha", alphaActive),
			testService("beta", betaActive),
			{Name: "off", Kind: "codex", Disabled: true},
		},
	}
	cfg.ApplyDefaults()
	captureWithUsage(t, cfg, "alpha", alphaActive, "a1", "a", 90, 90)
	captureWithUsage(t, cfg, "alpha", alphaActive, "b1", "b", 10, 10)
	if err := SwitchAccount(cfg, "alpha", "a"); err != nil {
		t.Fatal(err)
	}

	switches, err := SwitchBest(testContext(t), cfg, "all")
	if err != nil {
		t.Fatalf("expected empty and disabled services to be skipped, got %v", err)
	}
	if len(switches) != 1 || switches[0].Service != "alpha" || switches[0].Account != "b" {
		t.Fatalf("unexpected switches %#v", switches)
	}
	assertActive(t, cfg, "alpha", "b")
}

func TestSwitchBestPersistsEarlierSwitchWhenLaterServiceFails(t *testing.T) {
	dir := t.TempDir()
	alphaActive := filepath.Join(dir, "alpha-active.json")
	betaActive := filepath.Join(dir, "beta-active.json")
	cfg := Config{
		BackupRoot: filepath.Join(dir, "backups"),
		StatePath:  filepath.Join(dir, "state.json"),
		Services: []ServiceConfig{
			testService("alpha", alphaActive),
			testService("beta", betaActive),
		},
	}
	cfg.ApplyDefaults()
	captureWithUsage(t, cfg, "alpha", alphaActive, "a1", "a", 90, 90)
	captureWithUsage(t, cfg, "alpha", alphaActive, "b1", "b", 10, 10)
	if err := SwitchAccount(cfg, "alpha", "a"); err != nil {
		t.Fatal(err)
	}
	// beta has a captured account but no usage data: nothing is selectable.
	registerHomeAccount(t, cfg, "beta", "c", "c1")

	switches, err := SwitchBest(testContext(t), cfg, "all")
	if err == nil {
		t.Fatal("expected error from beta")
	}
	if len(switches) != 1 || switches[0].Service != "alpha" {
		t.Fatalf("unexpected switches %#v", switches)
	}
	state, loadErr := LoadState(cfg.StatePath)
	if loadErr != nil {
		t.Fatal(loadErr)
	}
	if got := state.Service("alpha").ActiveAccount; got != "b" {
		t.Fatalf("alpha switch was not persisted, active is %q", got)
	}
}

func TestSwitchBestDoesNotRestoreWhenBestAlreadyActive(t *testing.T) {
	dir := t.TempDir()
	active := filepath.Join(dir, "active-auth.json")
	cfg := testConfig(dir, active)
	captureWithUsage(t, cfg, "codex", active, "a1", "a", 10, 10)
	captureWithUsage(t, cfg, "codex", active, "b1", "b", 90, 90)
	if err := SwitchAccount(cfg, "codex", "a"); err != nil {
		t.Fatal(err)
	}

	switches, err := SwitchBest(testContext(t), cfg, "codex")
	if err != nil {
		t.Fatal(err)
	}
	if len(switches) != 0 {
		t.Fatalf("expected no switch, got %#v", switches)
	}
	assertActive(t, cfg, "codex", "a")
}

func TestMonitorOnceSkipsDisabledAndEmptyServices(t *testing.T) {
	dir := t.TempDir()
	active := filepath.Join(dir, "active-auth.json")
	cfg := Config{
		BackupRoot: filepath.Join(dir, "backups"),
		StatePath:  filepath.Join(dir, "state.json"),
		Services: []ServiceConfig{
			testService("alpha", active),
			testService("empty", filepath.Join(dir, "empty.json")),
			{Name: "off", Kind: "codex", Disabled: true},
		},
	}
	cfg.ApplyDefaults()
	captureWithUsage(t, cfg, "alpha", active, "a1", "a", 10, 10)

	result := MonitorOnce(testContext(t), cfg, true)
	if err := errors.Join(result.Errors...); err != nil {
		t.Fatalf("expected no errors for disabled/empty services, got %v", err)
	}
}

func TestShouldAutoSwitchEscapesUnselectableActiveImmediately(t *testing.T) {
	best := statusForTest("b", 10, 10)
	result := ServiceStatus{Accounts: []AccountStatus{
		{Account: AccountState{Name: "a"}, Active: true, Selectable: false},
		best,
	}}
	now := time.Now().UTC()
	// An exhausted or credential-dead active account burns the user every
	// minute; the cooldown must not delay the escape.
	if !shouldAutoSwitch(MonitorConfig{}, result, best, now.Add(-time.Minute), now) {
		t.Fatal("expected immediate switch away from an unselectable active account")
	}
}

func TestShouldAutoSwitchHonorsConfiguredKnobs(t *testing.T) {
	active := statusForTest("a", 85, 85)
	active.Active = true
	best := statusForTest("b", 20, 20)
	result := ServiceStatus{Accounts: []AccountStatus{active, best}}
	now := time.Now().UTC()
	aged := now.Add(-defaultAutoSwitchCooldown - time.Minute)

	// 85% is below the default 90% threshold...
	if shouldAutoSwitch(MonitorConfig{}, result, best, aged, now) {
		t.Fatal("expected no switch below the default threshold")
	}
	// ...but above a configured 80% threshold.
	lower := MonitorConfig{SwitchThreshold: PtrFloat64(0.80)}
	if !shouldAutoSwitch(lower, result, best, aged, now) {
		t.Fatal("expected switch above the configured threshold")
	}
	// A configured short cooldown unblocks a recent switch.
	short := MonitorConfig{SwitchThreshold: PtrFloat64(0.80), Cooldown: &Duration{Duration: time.Minute}}
	if !shouldAutoSwitch(short, result, best, now.Add(-2*time.Minute), now) {
		t.Fatal("expected switch after the configured cooldown")
	}
	// A configured improvement margin larger than the gap blocks the switch.
	strict := MonitorConfig{SwitchThreshold: PtrFloat64(0.80), MinImprovement: PtrFloat64(0.90)}
	if shouldAutoSwitch(strict, result, best, aged, now) {
		t.Fatal("expected no switch below the configured improvement margin")
	}
}

func TestMonitorOnceEscapesExhaustedActiveDespiteCooldown(t *testing.T) {
	dir := t.TempDir()
	active := filepath.Join(dir, "claude-active.json")
	cfg := Config{
		BackupRoot: filepath.Join(dir, "backups"),
		StatePath:  filepath.Join(dir, "state.json"),
		Services: []ServiceConfig{
			testService("claude", active),
		},
	}
	cfg.ApplyDefaults()
	// a is exhausted (100%); b is only 5 points better and the last switch
	// was seconds ago — both the improvement margin and the cooldown must be
	// bypassed because staying on a dead account has no value.
	captureWithUsage(t, cfg, "claude", active, "claude-a", "a", 100, 60)
	captureWithUsage(t, cfg, "claude", active, "claude-b", "b", 95, 60)
	if err := SwitchAccount(cfg, "claude", "a"); err != nil {
		t.Fatal(err)
	}

	result := MonitorOnce(testContext(t), cfg, true)
	if err := errors.Join(result.Errors...); err != nil {
		t.Fatal(err)
	}
	if len(result.Switches) != 1 {
		t.Fatalf("expected immediate escape from exhausted account, got %d switches", len(result.Switches))
	}
	assertActive(t, cfg, "claude", "b")
}

func TestCollectServiceMarksAccountWithMissingBackupUnselectable(t *testing.T) {
	dir := t.TempDir()
	active := filepath.Join(dir, "active-auth.json")
	cfg := testConfig(dir, active)
	captureWithUsage(t, cfg, "codex", active, "a1", "a", 10, 10)
	if err := os.Remove(filepath.Join(AccountDir(cfg, "codex", "a"), "auth.json")); err != nil {
		t.Fatal(err)
	}

	state, err := LoadState(cfg.StatePath)
	if err != nil {
		t.Fatal(err)
	}
	result := CollectService(testContext(t), cfg, state, cfg.Services[0])
	if len(result.Accounts) != 1 {
		t.Fatalf("expected one account, got %d", len(result.Accounts))
	}
	status := result.Accounts[0]
	if status.Selectable {
		t.Fatalf("expected account with missing backup to be unselectable despite cached usage, reason %q", status.Reason)
	}
	if !strings.Contains(status.Reason, "missing backup files") {
		t.Fatalf("unexpected reason %q", status.Reason)
	}
}

// The codex app-server surfaces dead stored tokens as an RPC error; that must
// classify as credentials-invalid so the cached snapshot cannot keep the
// account selectable.
func TestCodexAuthRPCErrorMarksCredentialsInvalid(t *testing.T) {
	dir := t.TempDir()
	fakeCodex := filepath.Join(dir, "codex")
	script := `#!/bin/sh
while IFS= read -r line; do
	case "$line" in
		*'"id":1'*)
			printf '%s\n' '{"id":1,"result":{"userAgent":"test"}}'
			;;
		*'"id":2'*)
			printf '%s\n' '{"id":2,"error":{"code":-32000,"message":"failed to refresh token: 401 Unauthorized"}}'
			exit 0
			;;
	esac
done
`
	if err := os.WriteFile(fakeCodex, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	oldCommand := codexCommand
	codexCommand = fakeCodex
	t.Cleanup(func() { codexCommand = oldCommand })

	live := filepath.Join(dir, "auth.json")
	cfg := Config{
		BackupRoot: filepath.Join(dir, "backups"),
		StatePath:  filepath.Join(dir, "state.json"),
		Services: []ServiceConfig{
			{Name: "codex", Kind: "codex", Files: []ManagedFile{requiredFile(live, "auth.json")}},
		},
	}
	cfg.ApplyDefaults()
	accountDir := AccountDir(cfg, "codex", "main")
	if err := os.MkdirAll(accountDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(accountDir, "auth.json"), []byte(`{"auth_mode":"chatgpt","tokens":{"access_token":"rotated-out","refresh_token":"rotated-out"}}`), 0o600); err != nil {
		t.Fatal(err)
	}

	_, err := fetchCodexUsage(testContext(t), cfg, cfg.Services[0], AccountState{Name: "main"})
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "unusable") {
		t.Fatalf("expected credentials-invalid classification, got %v", err)
	}
}

func TestValidateRejectsOutOfRangeMonitorKnobs(t *testing.T) {
	base := func() Config {
		cfg := Config{Services: []ServiceConfig{{Name: "svc", Kind: "codex"}}}
		cfg.ApplyDefaults()
		return cfg
	}
	cfg := base()
	cfg.Monitor.SwitchThreshold = PtrFloat64(90) // percent instead of ratio
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected threshold validation error")
	}
	cfg = base()
	cfg.Monitor.MinImprovement = PtrFloat64(-0.1)
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected improvement validation error")
	}
	cfg = base()
	cfg.Monitor.Cooldown = &Duration{Duration: -time.Minute}
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected cooldown validation error")
	}
}

func TestLimitWindowRatioExpiresAfterReset(t *testing.T) {
	window := LimitWindow{Pct: PtrFloat64(95), ResetsAt: time.Now().Add(-time.Hour)}
	ratio, ok := window.Ratio()
	if !ok || ratio != 0 {
		t.Fatalf("expected expired window to read 0, got %v %v", ratio, ok)
	}
	window.ResetsAt = time.Now().Add(time.Hour)
	ratio, ok = window.Ratio()
	if !ok || ratio != 0.95 {
		t.Fatalf("expected live window at 0.95, got %v %v", ratio, ok)
	}
}

func TestLoadStateToleratesNullService(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	if err := os.WriteFile(path, []byte(`{"version":1,"services":{"claude":null}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	state, err := LoadState(path)
	if err != nil {
		t.Fatal(err)
	}
	if state.Service("claude").Accounts == nil {
		t.Fatal("expected normalized service state")
	}
}

func TestValidateRejectsDuplicateBackupNameAfterCleaning(t *testing.T) {
	cfg := Config{
		Services: []ServiceConfig{
			{
				Name:        "svc",
				Kind:        "codex",
				AccountMode: AccountModeHome,
				Files: []ManagedFile{
					{Path: "/tmp/a", BackupName: "auth.json"},
					{Path: "/tmp/b", BackupName: "./auth.json"},
				},
			},
		},
	}
	cfg.ApplyDefaults()
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected duplicate backup_name error")
	}
}

func TestValidateRejectsCaseInsensitiveDuplicateServices(t *testing.T) {
	cfg := Config{
		Services: []ServiceConfig{
			{Name: "Claude", Kind: "custom", Files: []ManagedFile{{Path: "/tmp/a", BackupName: "a"}}},
			{Name: "claude", Kind: "custom", Files: []ManagedFile{{Path: "/tmp/b", BackupName: "b"}}},
		},
	}
	cfg.ApplyDefaults()
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected case-insensitive duplicate service error")
	}
}

func TestCollectServiceKeepsCachedUsageWhenProbeFails(t *testing.T) {
	dir := t.TempDir()
	active := filepath.Join(dir, "active-auth.json")
	cfg := Config{
		BackupRoot: filepath.Join(dir, "backups"),
		StatePath:  filepath.Join(dir, "state.json"),
		Services: []ServiceConfig{
			{
				Name:         "svc",
				Kind:         "codex",
				UsageCommand: []string{"sh", "-c", "echo boom >&2; exit 1"},
			},
		},
	}
	cfg.ApplyDefaults()
	captureWithUsage(t, cfg, "svc", active, "a1", "a", 10, 10)

	state, err := LoadState(cfg.StatePath)
	if err != nil {
		t.Fatal(err)
	}
	result := CollectService(testContext(t), cfg, state, cfg.Services[0])
	if len(result.Accounts) != 1 {
		t.Fatalf("expected one account, got %d", len(result.Accounts))
	}
	status := result.Accounts[0]
	if !status.Selectable {
		t.Fatalf("expected cached usage to keep account selectable, reason %q", status.Reason)
	}
	if !strings.Contains(status.Reason, "stale usage") {
		t.Fatalf("expected stale-usage annotation, got %q", status.Reason)
	}
}

func TestRunUsageCommandIgnoresStderr(t *testing.T) {
	dir := t.TempDir()
	cfg := Config{BackupRoot: filepath.Join(dir, "backups"), StatePath: filepath.Join(dir, "state.json")}
	service := ServiceConfig{
		Name:         "svc",
		Kind:         "custom",
		UsageCommand: []string{"sh", "-c", `echo "warning: noise" >&2; echo '{"five_hour":{"pct":10},"weekly":{"pct":20}}'`},
	}
	usage, err := runUsageCommand(testContext(t), cfg, service, AccountState{Name: "a"})
	if err != nil {
		t.Fatal(err)
	}
	ratio, ok := usage.Weekly.Ratio()
	if !ok || ratio != 0.2 {
		t.Fatalf("unexpected weekly ratio %v %v", ratio, ok)
	}
}

func TestCollectServiceOrdersAccountsByName(t *testing.T) {
	dir := t.TempDir()
	active := filepath.Join(dir, "active-auth.json")
	cfg := testConfig(dir, active)
	for _, name := range []string{"charlie", "alpha", "bravo"} {
		captureWithUsage(t, cfg, "codex", active, name+"-auth", name, 10, 10)
	}
	state, err := LoadState(cfg.StatePath)
	if err != nil {
		t.Fatal(err)
	}
	result := CollectService(testContext(t), cfg, state, cfg.Services[0])
	var names []string
	for _, account := range result.Accounts {
		names = append(names, account.Account.Name)
	}
	if strings.Join(names, ",") != "alpha,bravo,charlie" {
		t.Fatalf("expected sorted account order, got %v", names)
	}
}

func TestSwitchBestErrorsWhenNothingCapturedAnywhere(t *testing.T) {
	dir := t.TempDir()
	cfg := Config{
		BackupRoot: filepath.Join(dir, "backups"),
		StatePath:  filepath.Join(dir, "state.json"),
		Services: []ServiceConfig{
			testService("alpha", filepath.Join(dir, "alpha.json")),
			testService("beta", filepath.Join(dir, "beta.json")),
		},
	}
	cfg.ApplyDefaults()
	if _, err := SwitchBest(testContext(t), cfg, "all"); err == nil {
		t.Fatal("expected error when no service has captured accounts")
	}
}

func TestShouldAutoSwitchLeavesHealthyActiveForEarlierWeeklyReset(t *testing.T) {
	now := time.Now().UTC()
	active := weeklyResetStatusForTest("a", 14, 36, now.Add(72*time.Hour))
	active.Active = true
	soon := weeklyResetStatusForTest("b", 0, 49, now.Add(9*time.Hour))
	idle := weeklyResetStatusForTest("c", 0, 0, time.Time{})
	aged := now.Add(-defaultAutoSwitchCooldown - time.Minute)

	result := ServiceStatus{Accounts: []AccountStatus{active, soon}}
	if !shouldAutoSwitch(MonitorConfig{}, result, soon, aged, now) {
		t.Fatal("expected a switch to the account whose weekly window resets first")
	}
	if shouldAutoSwitch(MonitorConfig{}, result, soon, now.Add(-time.Minute), now) {
		t.Fatal("expected the cooldown to pace an optimization switch")
	}
	result = ServiceStatus{Accounts: []AccountStatus{active, idle}}
	if shouldAutoSwitch(MonitorConfig{}, result, idle, aged, now) {
		t.Fatal("expected no switch from a running weekly window to an idle account")
	}
}

const codexLimitsResponse = `{"id":2,"result":{"rateLimits":{"limitId":"codex","primary":{"usedPercent":12,"windowDurationMins":300,"resetsAt":1909954910},"secondary":{"usedPercent":34,"windowDurationMins":10080,"resetsAt":1910414767},"planType":"pro","rateLimitReachedType":null}}}`

// fakeCodexAppServer installs a codex that answers the rate-limit request
// with response and counts its launches.
func fakeCodexAppServer(t *testing.T, response string) func() int {
	t.Helper()
	dir := t.TempDir()
	calls := filepath.Join(dir, "calls")
	script := "#!/bin/sh\necho call >> '" + calls + "'\n" + `while IFS= read -r line; do
	case "$line" in
		*'"id":1'*) printf '%s\n' '{"id":1,"result":{"userAgent":"test"}}' ;;
		*'"id":2'*) printf '%s\n' '` + response + `'; exit 0 ;;
	esac
done
`
	path := filepath.Join(dir, "codex")
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	oldCommand := codexCommand
	codexCommand = path
	t.Cleanup(func() { codexCommand = oldCommand })
	return func() int {
		data, _ := os.ReadFile(calls)
		return strings.Count(string(data), "call")
	}
}

func codexHomeConfig(dir string) Config {
	cfg := Config{
		BackupRoot: filepath.Join(dir, "backups"),
		StatePath:  filepath.Join(dir, "state.json"),
		Services:   []ServiceConfig{{Name: "codex", Kind: "codex"}},
	}
	cfg.ApplyDefaults()
	return cfg
}

const codexTestLoginJSON = `{"auth_mode":"chatgpt","tokens":{"access_token":"tok","refresh_token":"ref"}}`

// An account whose login was rotated out (every probe fails to refresh) but
// whose cached usage is the lowest must never become the route.
func TestMonitorNeverSwitchesToAccountWithDeadCredentials(t *testing.T) {
	fakeCodexAppServer(t, `{"id":2,"error":{"code":-32000,"message":"failed to refresh token: 401 Unauthorized"}}`)
	cfg := codexHomeConfig(t.TempDir())
	captureWithUsage(t, cfg, "codex", "", codexTestLoginJSON, "dead", 5, 5)
	captureWithUsage(t, cfg, "codex", "", codexTestLoginJSON, "busy", 100, 60)
	setServiceLastSwitchedAt(t, cfg, "codex", time.Now().Add(-defaultAutoSwitchCooldown-time.Minute))

	result := MonitorOnce(testContext(t), cfg, true)
	if len(result.Switches) != 0 {
		t.Fatalf("expected no switch to the dead account, got %#v", result.Switches)
	}
	assertActive(t, cfg, "codex", "busy")
	for _, status := range result.Results[0].Accounts {
		if status.Account.Name == "dead" && status.Selectable {
			t.Fatalf("dead account must not be selectable, reason %q", status.Reason)
		}
	}
}

// A login file that parses but has no tokens (written mid-logout) must be
// credentials-invalid, not a plain error that keeps the cached usage.
func TestCodexLoginWithoutTokensIsUnselectable(t *testing.T) {
	calls := fakeCodexAppServer(t, codexLimitsResponse)
	cfg := codexHomeConfig(t.TempDir())
	captureWithUsage(t, cfg, "codex", "", `{"auth_mode":"chatgpt","tokens":{}}`, "dead", 5, 5)
	state, err := LoadState(cfg.StatePath)
	if err != nil {
		t.Fatal(err)
	}
	status := CollectService(testContext(t), cfg, state, cfg.Services[0]).Accounts[0]
	if status.Selectable || !strings.Contains(status.Reason, "unusable") {
		t.Fatalf("expected an unselectable tokenless account, got %v %q", status.Selectable, status.Reason)
	}
	if calls() != 0 {
		t.Fatal("a tokenless login reached codex app-server")
	}
}

func TestCollectServiceBacksOffOnRateLimit(t *testing.T) {
	calls := fakeCodexAppServer(t, `{"id":2,"error":{"code":-32000,"message":"429 Too Many Requests"}}`)
	cfg := codexHomeConfig(t.TempDir())
	captureWithUsage(t, cfg, "codex", "", codexTestLoginJSON, "a", 10, 10)
	state, err := LoadState(cfg.StatePath)
	if err != nil {
		t.Fatal(err)
	}
	first := CollectService(testContext(t), cfg, state, cfg.Services[0]).Accounts[0]
	if got := calls(); got != 1 {
		t.Fatalf("expected one probe, got %d", got)
	}
	if !first.Selectable || !strings.Contains(first.Reason, "stale usage") {
		t.Fatalf("expected selectable with stale annotation, got %v %q", first.Selectable, first.Reason)
	}
	if remaining := time.Until(first.Account.FetchBackoffUntil); remaining < rateLimitBackoffMin-time.Minute || remaining > rateLimitBackoffMin+time.Minute {
		t.Fatalf("expected ~%s backoff, got %s", rateLimitBackoffMin, remaining)
	}

	second := CollectService(testContext(t), cfg, state, cfg.Services[0]).Accounts[0]
	if got := calls(); got != 1 {
		t.Fatalf("expected no probe during backoff, got %d", got)
	}
	if !second.Selectable || !strings.Contains(second.Reason, "paused") {
		t.Fatalf("expected selectable with paused annotation, got %v %q", second.Selectable, second.Reason)
	}
}

func TestDeadCredentialsBackOffAndStayUnselectable(t *testing.T) {
	calls := fakeCodexAppServer(t, `{"id":2,"error":{"code":-32000,"message":"failed to refresh token: invalid_grant"}}`)
	cfg := codexHomeConfig(t.TempDir())
	captureWithUsage(t, cfg, "codex", "", codexTestLoginJSON, "dead", 5, 5)
	state, err := LoadState(cfg.StatePath)
	if err != nil {
		t.Fatal(err)
	}
	first := CollectService(testContext(t), cfg, state, cfg.Services[0]).Accounts[0]
	afterFirst := calls()
	if afterFirst == 0 {
		t.Fatal("expected the first cycle to probe the login")
	}
	if first.Selectable || !strings.Contains(first.Reason, "unusable") {
		t.Fatalf("expected unselectable dead account, got %v %q", first.Selectable, first.Reason)
	}
	second := CollectService(testContext(t), cfg, state, cfg.Services[0]).Accounts[0]
	if got := calls(); got != afterFirst {
		t.Fatalf("expected no probes during credentials backoff, got %d after %d", got, afterFirst)
	}
	if second.Selectable || !strings.Contains(second.Reason, "retry at") {
		t.Fatalf("expected an unselectable account with retry-at, got %v %q", second.Selectable, second.Reason)
	}
}

func TestInactiveAccountsUseCachedUsageWithinTTL(t *testing.T) {
	calls := fakeCodexAppServer(t, codexLimitsResponse)
	cfg := codexHomeConfig(t.TempDir())
	fresh := usageForTest(10, 10)
	fresh.ObservedAt = time.Now().UTC()
	captureWithUsageSnapshot(t, cfg, "codex", "", codexTestLoginJSON, "b", fresh)
	captureWithUsageSnapshot(t, cfg, "codex", "", codexTestLoginJSON, "a", fresh)
	state, err := LoadState(cfg.StatePath)
	if err != nil {
		t.Fatal(err)
	}
	CollectService(testContext(t), cfg, state, cfg.Services[0])
	if got := calls(); got != 1 {
		t.Fatalf("expected only the active account to be probed, got %d", got)
	}
	// Age the inactive account's snapshot past the TTL: it is probed again.
	stale := state.Service("codex").Accounts["b"]
	stale.Usage.ObservedAt = time.Now().UTC().Add(-inactiveUsageTTL - time.Minute)
	state.Service("codex").Accounts["b"] = stale
	CollectService(testContext(t), cfg, state, cfg.Services[0])
	if got := calls(); got != 3 {
		t.Fatalf("expected active + aged inactive probes, got %d", got)
	}
}
