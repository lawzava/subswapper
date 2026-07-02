package subswapper

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

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

func TestCaptureRejectsReservedAccountNames(t *testing.T) {
	dir := t.TempDir()
	active := filepath.Join(dir, "active-auth.json")
	if err := os.WriteFile(active, []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := testConfig(dir, active)
	for _, name := range []string{"auto", ".", "..", "  "} {
		if _, err := CaptureAccount(cfg, "codex", name, ""); err == nil {
			t.Fatalf("expected capture of account %q to fail", name)
		}
	}
}

func TestCaptureRejectsCaseInsensitiveDuplicate(t *testing.T) {
	dir := t.TempDir()
	active := filepath.Join(dir, "active-auth.json")
	if err := os.WriteFile(active, []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := testConfig(dir, active)
	if _, err := CaptureAccount(cfg, "codex", "Work", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := CaptureAccount(cfg, "codex", "work", ""); err == nil {
		t.Fatal("expected case-colliding capture to fail")
	}
	if _, err := CaptureAccount(cfg, "codex", "Work", ""); err != nil {
		t.Fatalf("recapturing the same name must stay allowed: %v", err)
	}
}

func TestSwitchAccountSyncsOutgoingAccountFiles(t *testing.T) {
	dir := t.TempDir()
	active := filepath.Join(dir, "active-auth.json")
	cfg := testConfig(dir, active)

	captureWithUsage(t, cfg, "codex", active, "a1", "a", 10, 10)
	captureWithUsage(t, cfg, "codex", active, "b1", "b", 10, 10)
	// Simulate the live client rotating b's credentials after capture.
	if err := os.WriteFile(active, []byte("b2"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := SwitchAccount(cfg, "codex", "a"); err != nil {
		t.Fatal(err)
	}
	assertFileContent(t, active, "a1")
	if err := SwitchAccount(cfg, "codex", "b"); err != nil {
		t.Fatal(err)
	}
	assertFileContent(t, active, "b2")
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

func TestRestoreRemovesLiveOptionalFileWhenBackupMissing(t *testing.T) {
	dir := t.TempDir()
	required := filepath.Join(dir, "required.json")
	optional := filepath.Join(dir, "optional.json")
	cfg := Config{
		BackupRoot: filepath.Join(dir, "backups"),
		StatePath:  filepath.Join(dir, "state.json"),
		Services: []ServiceConfig{
			{
				Name: "svc",
				Kind: "custom",
				Files: []ManagedFile{
					requiredFile(required, "required.json"),
					optionalFile(optional, "optional.json"),
				},
			},
		},
	}
	cfg.ApplyDefaults()

	if err := os.WriteFile(required, []byte("one"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(optional, []byte("one-optional"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := CaptureAccount(cfg, "svc", "one", ""); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(required, []byte("two"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(optional); err != nil {
		t.Fatal(err)
	}
	if _, err := CaptureAccount(cfg, "svc", "two", ""); err != nil {
		t.Fatal(err)
	}

	if err := SwitchAccount(cfg, "svc", "one"); err != nil {
		t.Fatal(err)
	}
	assertFileContent(t, optional, "one-optional")
	if err := SwitchAccount(cfg, "svc", "two"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(optional); !os.IsNotExist(err) {
		t.Fatalf("expected live optional file to be removed for account without it, got %v", err)
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
			{Name: "off", Kind: "custom", Disabled: true, Files: []ManagedFile{requiredFile(filepath.Join(dir, "off.json"), "off.json")}},
		},
	}
	cfg.ApplyDefaults()
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
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
	assertFileContent(t, alphaActive, "b1")
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
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	captureWithUsage(t, cfg, "alpha", alphaActive, "a1", "a", 90, 90)
	captureWithUsage(t, cfg, "alpha", alphaActive, "b1", "b", 10, 10)
	if err := SwitchAccount(cfg, "alpha", "a"); err != nil {
		t.Fatal(err)
	}
	// beta has a captured account but no usage data: nothing is selectable.
	if err := os.WriteFile(betaActive, []byte("c1"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := CaptureAccount(cfg, "beta", "c", ""); err != nil {
		t.Fatal(err)
	}

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
	assertFileContent(t, alphaActive, "b1")
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
	// Live rotation after the switch: a no-op "switch" must not clobber it.
	if err := os.WriteFile(active, []byte("a-rotated"), 0o600); err != nil {
		t.Fatal(err)
	}

	switches, err := SwitchBest(testContext(t), cfg, "codex")
	if err != nil {
		t.Fatal(err)
	}
	if len(switches) != 0 {
		t.Fatalf("expected no switch, got %#v", switches)
	}
	assertFileContent(t, active, "a-rotated")
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
			{Name: "off", Kind: "custom", Disabled: true, Files: []ManagedFile{requiredFile(filepath.Join(dir, "off.json"), "off.json")}},
		},
	}
	cfg.ApplyDefaults()
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
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
	assertFileContent(t, active, "claude-b")
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

func TestSyncNeverOverwritesGoodBackupWithCorruptLiveFile(t *testing.T) {
	dir := t.TempDir()
	live := filepath.Join(dir, "credentials.json")
	service := ServiceConfig{
		Name: "claude", Kind: "claude",
		Files: []ManagedFile{requiredFile(live, "credentials.json")},
	}
	cfg := Config{
		BackupRoot: filepath.Join(dir, "backups"),
		StatePath:  filepath.Join(dir, "state.json"),
		Services:   []ServiceConfig{service},
	}
	cfg.ApplyDefaults()
	goodBackup := `{"claudeAiOauth":{"accessToken":"good-token"}}`
	accountDir := AccountDir(cfg, "claude", "a")
	if err := os.MkdirAll(accountDir, 0o700); err != nil {
		t.Fatal(err)
	}
	backupPath := filepath.Join(accountDir, "credentials.json")
	if err := os.WriteFile(backupPath, []byte(goodBackup), 0o600); err != nil {
		t.Fatal(err)
	}

	// A logged-out/truncated live file must not poison the backup...
	for _, corrupt := range []string{"", "   ", "{}", `{"claudeAiOauth":{}}`, "not-json"} {
		if err := os.WriteFile(live, []byte(corrupt), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := syncAccountFiles(cfg, service, "a"); err != nil {
			t.Fatal(err)
		}
		assertFileContent(t, backupPath, goodBackup)
	}
	// ...while genuinely rotated credentials still sync.
	rotated := `{"claudeAiOauth":{"accessToken":"rotated-token"}}`
	if err := os.WriteFile(live, []byte(rotated), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := syncAccountFiles(cfg, service, "a"); err != nil {
		t.Fatal(err)
	}
	assertFileContent(t, backupPath, rotated)
}

// Replays the 2026-07-02 incident: an account whose stored refresh token was
// rotated out (every fetch 401s, refresh rejected) but whose cached usage is
// the lowest in the store. The monitor must never switch to it.
func TestMonitorNeverSwitchesToAccountWithDeadCredentials(t *testing.T) {
	dir := t.TempDir()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	t.Cleanup(server.Close)
	oldUsageURL, oldTokenURL := claudeUsageURL, claudeTokenURL
	claudeUsageURL, claudeTokenURL = server.URL, server.URL
	t.Cleanup(func() { claudeUsageURL, claudeTokenURL = oldUsageURL, oldTokenURL })

	live := filepath.Join(dir, "credentials.json")
	cfg := Config{
		BackupRoot: filepath.Join(dir, "backups"),
		StatePath:  filepath.Join(dir, "state.json"),
		Services: []ServiceConfig{
			{Name: "claude", Kind: "claude", Files: []ManagedFile{requiredFile(live, "credentials.json")}},
		},
	}
	cfg.ApplyDefaults()
	// Active account "busy" at 100%, dead account "dead" cached at 5%.
	captureWithUsage(t, cfg, "claude", live, `{"claudeAiOauth":{"accessToken":"dead","refreshToken":"dead"}}`, "dead", 5, 5)
	captureWithUsage(t, cfg, "claude", live, `{"claudeAiOauth":{"accessToken":"busy","refreshToken":"busy"}}`, "busy", 100, 60)
	if err := SwitchAccount(cfg, "claude", "busy"); err != nil {
		t.Fatal(err)
	}
	setServiceLastSwitchedAt(t, cfg, "claude", time.Now().Add(-defaultAutoSwitchCooldown-time.Minute))

	result := MonitorOnce(testContext(t), cfg, true)
	if len(result.Switches) != 0 {
		t.Fatalf("expected no switch to the dead account, got %#v", result.Switches)
	}
	assertFileContent(t, live, `{"claudeAiOauth":{"accessToken":"busy","refreshToken":"busy"}}`)
	for _, status := range result.Results[0].Accounts {
		if status.Account.Name == "dead" && status.Selectable {
			t.Fatalf("dead account must not be selectable, reason %q", status.Reason)
		}
	}
}

// A credentials file that parses but has no tokens at all (captured
// mid-logout) must be credentials-invalid, not a plain error that falls back
// to the cached usage snapshot.
func TestClaudeAccountWithoutAnyTokensIsUnselectable(t *testing.T) {
	dir := t.TempDir()
	live := filepath.Join(dir, "credentials.json")
	cfg := Config{
		BackupRoot: filepath.Join(dir, "backups"),
		StatePath:  filepath.Join(dir, "state.json"),
		Services: []ServiceConfig{
			{Name: "claude", Kind: "claude", Files: []ManagedFile{requiredFile(live, "credentials.json")}},
		},
	}
	cfg.ApplyDefaults()
	captureWithUsage(t, cfg, "claude", live, `{"claudeAiOauth":{}}`, "dead", 5, 5)

	state, err := LoadState(cfg.StatePath)
	if err != nil {
		t.Fatal(err)
	}
	result := CollectService(testContext(t), cfg, state, cfg.Services[0])
	status := result.Accounts[0]
	if status.Selectable {
		t.Fatalf("expected tokenless account to be unselectable, reason %q", status.Reason)
	}
	if !strings.Contains(status.Reason, "unusable") {
		t.Fatalf("expected credentials-invalid reason, got %q", status.Reason)
	}
}

// A missing access token with a refresh token present is recoverable: the
// fetch must refresh instead of failing.
func TestClaudeFetchRefreshesWhenAccessTokenMissing(t *testing.T) {
	dir := t.TempDir()
	mux := http.NewServeMux()
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"fresh","expires_in":3600,"refresh_token":"fresh-refresh"}`))
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer fresh" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"five_hour":{"utilization":12},"seven_day":{"utilization":34}}`))
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	oldUsageURL, oldTokenURL := claudeUsageURL, claudeTokenURL
	claudeUsageURL, claudeTokenURL = server.URL, server.URL+"/token"
	t.Cleanup(func() { claudeUsageURL, claudeTokenURL = oldUsageURL, oldTokenURL })

	live := filepath.Join(dir, "credentials.json")
	cfg := Config{
		BackupRoot: filepath.Join(dir, "backups"),
		StatePath:  filepath.Join(dir, "state.json"),
		Services: []ServiceConfig{
			{Name: "claude", Kind: "claude", Files: []ManagedFile{requiredFile(live, "credentials.json")}},
		},
	}
	cfg.ApplyDefaults()
	accountDir := AccountDir(cfg, "claude", "main")
	if err := os.MkdirAll(accountDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(accountDir, "credentials.json"), []byte(`{"claudeAiOauth":{"refreshToken":"r"}}`), 0o600); err != nil {
		t.Fatal(err)
	}

	usage, err := fetchClaudeUsage(testContext(t), cfg, cfg.Services[0], AccountState{Name: "main"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if ratio, ok := usage.FiveHour.Ratio(); !ok || ratio != 0.12 {
		t.Fatalf("unexpected five-hour ratio %v %v", ratio, ok)
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

	_, err := fetchCodexUsage(testContext(t), cfg, cfg.Services[0], AccountState{Name: "main"}, false)
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "unusable") {
		t.Fatalf("expected credentials-invalid classification, got %v", err)
	}
}

// A 429 must pause fetches for that account (honoring Retry-After) instead of
// retrying at full rate every cycle, while cached usage keeps it selectable.
func TestCollectServiceBacksOffOnRateLimit(t *testing.T) {
	dir := t.TempDir()
	var requests atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.Header().Set("Retry-After", "300")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	t.Cleanup(server.Close)
	oldURL := claudeUsageURL
	claudeUsageURL = server.URL
	t.Cleanup(func() { claudeUsageURL = oldURL })

	live := filepath.Join(dir, "credentials.json")
	cfg := Config{
		BackupRoot: filepath.Join(dir, "backups"),
		StatePath:  filepath.Join(dir, "state.json"),
		Services: []ServiceConfig{
			{Name: "claude", Kind: "claude", Files: []ManagedFile{requiredFile(live, "credentials.json")}},
		},
	}
	cfg.ApplyDefaults()
	captureWithUsage(t, cfg, "claude", live, `{"claudeAiOauth":{"accessToken":"tok"}}`, "a", 10, 10)

	state, err := LoadState(cfg.StatePath)
	if err != nil {
		t.Fatal(err)
	}
	first := CollectService(testContext(t), cfg, state, cfg.Services[0]).Accounts[0]
	if got := requests.Load(); got != 1 {
		t.Fatalf("expected one request, got %d", got)
	}
	if !first.Selectable || !strings.Contains(first.Reason, "stale usage") {
		t.Fatalf("expected selectable with stale annotation, got %v %q", first.Selectable, first.Reason)
	}
	until := first.Account.FetchBackoffUntil
	if remaining := time.Until(until); remaining < 4*time.Minute || remaining > 6*time.Minute {
		t.Fatalf("expected ~5m backoff from Retry-After, got %s", remaining)
	}

	second := CollectService(testContext(t), cfg, state, cfg.Services[0]).Accounts[0]
	if got := requests.Load(); got != 1 {
		t.Fatalf("expected no request during backoff, got %d", got)
	}
	if !second.Selectable || !strings.Contains(second.Reason, "paused") {
		t.Fatalf("expected selectable with paused annotation, got %v %q", second.Selectable, second.Reason)
	}
}

// Dead credentials must stop being probed every cycle: one failing round sets
// a long backoff, and the account stays unselectable throughout it.
func TestDeadCredentialsBackOffAndStayUnselectable(t *testing.T) {
	dir := t.TempDir()
	var requests atomic.Int64
	mux := http.NewServeMux()
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"invalid_grant"}`))
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusUnauthorized)
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	oldUsageURL, oldTokenURL := claudeUsageURL, claudeTokenURL
	claudeUsageURL, claudeTokenURL = server.URL, server.URL+"/token"
	t.Cleanup(func() { claudeUsageURL, claudeTokenURL = oldUsageURL, oldTokenURL })

	live := filepath.Join(dir, "credentials.json")
	cfg := Config{
		BackupRoot: filepath.Join(dir, "backups"),
		StatePath:  filepath.Join(dir, "state.json"),
		Services: []ServiceConfig{
			{Name: "claude", Kind: "claude", Files: []ManagedFile{requiredFile(live, "credentials.json")}},
		},
	}
	cfg.ApplyDefaults()
	captureWithUsage(t, cfg, "claude", live, `{"claudeAiOauth":{"accessToken":"x","refreshToken":"rotated-out"}}`, "dead", 5, 5)

	state, err := LoadState(cfg.StatePath)
	if err != nil {
		t.Fatal(err)
	}
	first := CollectService(testContext(t), cfg, state, cfg.Services[0]).Accounts[0]
	afterFirst := requests.Load()
	if afterFirst == 0 {
		t.Fatal("expected the first cycle to probe the credentials")
	}
	if first.Selectable || !strings.Contains(first.Reason, "unusable") {
		t.Fatalf("expected unselectable dead account, got %v %q", first.Selectable, first.Reason)
	}

	second := CollectService(testContext(t), cfg, state, cfg.Services[0]).Accounts[0]
	if got := requests.Load(); got != afterFirst {
		t.Fatalf("expected no requests during credentials backoff, got %d after %d", got, afterFirst)
	}
	if second.Selectable {
		t.Fatal("dead account must stay unselectable during backoff")
	}
	if !strings.Contains(second.Reason, "retry at") {
		t.Fatalf("expected retry-at annotation, got %q", second.Reason)
	}
}

// Inactive accounts with a fresh cached snapshot skip the network fetch; the
// active account is always fetched.
func TestInactiveAccountsUseCachedUsageWithinTTL(t *testing.T) {
	dir := t.TempDir()
	var requests atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"five_hour":{"utilization":12},"seven_day":{"utilization":34}}`))
	}))
	t.Cleanup(server.Close)
	oldURL := claudeUsageURL
	claudeUsageURL = server.URL
	t.Cleanup(func() { claudeUsageURL = oldURL })

	live := filepath.Join(dir, "credentials.json")
	cfg := Config{
		BackupRoot: filepath.Join(dir, "backups"),
		StatePath:  filepath.Join(dir, "state.json"),
		Services: []ServiceConfig{
			{Name: "claude", Kind: "claude", Files: []ManagedFile{requiredFile(live, "credentials.json")}},
		},
	}
	cfg.ApplyDefaults()
	fresh := usageForTest(10, 10)
	fresh.ObservedAt = time.Now().UTC()
	captureWithUsageSnapshot(t, cfg, "claude", live, `{"claudeAiOauth":{"accessToken":"tok-b"}}`, "b", fresh)
	captureWithUsageSnapshot(t, cfg, "claude", live, `{"claudeAiOauth":{"accessToken":"tok-a"}}`, "a", fresh)

	state, err := LoadState(cfg.StatePath)
	if err != nil {
		t.Fatal(err)
	}
	CollectService(testContext(t), cfg, state, cfg.Services[0])
	if got := requests.Load(); got != 1 {
		t.Fatalf("expected only the active account to be fetched, got %d requests", got)
	}

	// Age the inactive account's snapshot past the TTL: it gets fetched again.
	stale := state.Service("claude").Accounts["b"]
	stale.Usage.ObservedAt = time.Now().UTC().Add(-inactiveUsageTTL - time.Minute)
	state.Service("claude").Accounts["b"] = stale
	CollectService(testContext(t), cfg, state, cfg.Services[0])
	if got := requests.Load(); got != 3 {
		t.Fatalf("expected active + aged inactive fetches, got %d requests", got)
	}
}

func TestValidateRejectsOutOfRangeMonitorKnobs(t *testing.T) {
	base := func() Config {
		cfg := Config{Services: []ServiceConfig{{Name: "svc", Kind: "custom", Files: []ManagedFile{{Path: "/tmp/a", BackupName: "a"}}}}}
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
				Name: "svc",
				Kind: "custom",
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
				Kind:         "custom",
				Files:        []ManagedFile{requiredFile(active, "auth.json")},
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

func TestFetchClaudeUsageActiveReadsLiveAndSyncsBackup(t *testing.T) {
	dir := t.TempDir()
	live := filepath.Join(dir, "credentials.json")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer live-token" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"five_hour": {"utilization": 12, "resets_at": "2030-07-02T01:49:59Z"},
			"seven_day": {"utilization": 34, "resets_at": "2030-07-05T03:59:59Z"}
		}`))
	}))
	t.Cleanup(server.Close)
	oldURL := claudeUsageURL
	claudeUsageURL = server.URL
	t.Cleanup(func() { claudeUsageURL = oldURL })

	cfg := Config{
		BackupRoot: filepath.Join(dir, "backups"),
		StatePath:  filepath.Join(dir, "state.json"),
		Services: []ServiceConfig{
			{Name: "claude", Kind: "claude", Files: []ManagedFile{requiredFile(live, "credentials.json")}},
		},
	}
	liveCredentials := `{"claudeAiOauth":{"accessToken":"live-token"}}`
	if err := os.WriteFile(live, []byte(liveCredentials), 0o600); err != nil {
		t.Fatal(err)
	}
	accountDir := AccountDir(cfg, "claude", "main")
	if err := os.MkdirAll(accountDir, 0o700); err != nil {
		t.Fatal(err)
	}
	backupPath := filepath.Join(accountDir, "credentials.json")
	if err := os.WriteFile(backupPath, []byte(`{"claudeAiOauth":{"accessToken":"stale-token"}}`), 0o600); err != nil {
		t.Fatal(err)
	}

	usage, err := fetchClaudeUsage(testContext(t), cfg, cfg.Services[0], AccountState{Name: "main"}, true)
	if err != nil {
		t.Fatal(err)
	}
	if ratio, ok := usage.FiveHour.Ratio(); !ok || ratio != 0.12 {
		t.Fatalf("unexpected five-hour ratio %v %v", ratio, ok)
	}
	assertFileContent(t, backupPath, liveCredentials)
}

func TestFetchCodexUsageActiveSyncsRefreshedAuthToLiveAndBackup(t *testing.T) {
	dir := t.TempDir()
	live := filepath.Join(dir, "auth.json")
	refreshed := `{"auth_mode":"chatgpt","tokens":{"access_token":"refreshed","refresh_token":"r2"}}`
	fakeCodex := filepath.Join(dir, "codex")
	script := `#!/bin/sh
while IFS= read -r line; do
	case "$line" in
		*'"id":1'*)
			printf '%s\n' '{"id":1,"result":{"userAgent":"test"}}'
			;;
		*'"id":2'*)
			printf '%s' '` + refreshed + `' > "$CODEX_HOME/auth.json"
			printf '%s\n' '{"id":2,"result":{"rateLimits":{"limitId":"codex","primary":{"usedPercent":12,"windowDurationMins":300},"secondary":{"usedPercent":6,"windowDurationMins":10080}}}}'
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

	cfg := Config{
		BackupRoot: filepath.Join(dir, "backups"),
		StatePath:  filepath.Join(dir, "state.json"),
		Services: []ServiceConfig{
			{Name: "codex", Kind: "codex", Files: []ManagedFile{requiredFile(live, "auth.json")}},
		},
	}
	if err := os.WriteFile(live, []byte(`{"auth_mode":"chatgpt","tokens":{"access_token":"live","refresh_token":"r1"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	accountDir := AccountDir(cfg, "codex", "main")
	if err := os.MkdirAll(accountDir, 0o700); err != nil {
		t.Fatal(err)
	}
	backupPath := filepath.Join(accountDir, "auth.json")
	if err := os.WriteFile(backupPath, []byte(`{"auth_mode":"chatgpt","tokens":{"access_token":"stale","refresh_token":"r0"}}`), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := fetchCodexUsage(testContext(t), cfg, cfg.Services[0], AccountState{Name: "main"}, true); err != nil {
		t.Fatal(err)
	}
	assertFileContent(t, backupPath, refreshed)
	assertFileContent(t, live, refreshed)
}

func TestImportClaudeSwapSkipsExistingAccountsAndKeepsActive(t *testing.T) {
	dir := t.TempDir()
	root := buildClaudeSwapFixture(t, dir)
	cfg := testClaudeImportConfig(dir)
	// Live credentials match slot 2, cswap's active slot.
	if err := os.WriteFile(filepath.Join(dir, "live-credentials.json"), []byte(`{"claudeAiOauth":{"accessToken":"two"}}`), 0o600); err != nil {
		t.Fatal(err)
	}

	first, err := ImportClaudeSwap(cfg, root)
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Imported) != 2 {
		t.Fatalf("expected 2 imported, got %d", len(first.Imported))
	}
	// A later refresh updates the stored credentials; re-import must not undo it.
	fresh := `{"claudeAiOauth":{"accessToken":"fresh"}}`
	credentialsPath := filepath.Join(AccountDir(cfg, "claude", "cswap-1"), "credentials.json")
	if err := os.WriteFile(credentialsPath, []byte(fresh), 0o600); err != nil {
		t.Fatal(err)
	}

	second, err := ImportClaudeSwap(cfg, root)
	if err != nil {
		t.Fatal(err)
	}
	if len(second.Imported) != 0 || len(second.Skipped) != 2 {
		t.Fatalf("expected everything skipped, got %#v", second)
	}
	assertFileContent(t, credentialsPath, fresh)
	state, err := LoadState(cfg.StatePath)
	if err != nil {
		t.Fatal(err)
	}
	if got := state.Service("claude").ActiveAccount; got != "cswap-2" {
		t.Fatalf("expected active account preserved, got %q", got)
	}
}

func TestImportClaudeSwapSkipsBrokenSlots(t *testing.T) {
	dir := t.TempDir()
	root := buildClaudeSwapFixture(t, dir)
	if err := os.Remove(filepath.Join(root, "configs", ".claude-config-1-one@example.com.json")); err != nil {
		t.Fatal(err)
	}
	cfg := testClaudeImportConfig(dir)

	result, err := ImportClaudeSwap(cfg, root)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Imported) != 1 || result.Imported[0].Name != "cswap-2" {
		t.Fatalf("expected only cswap-2 imported, got %#v", result.Imported)
	}
	if len(result.Errors) != 1 {
		t.Fatalf("expected one slot error, got %v", result.Errors)
	}
	if _, err := os.Stat(AccountDir(cfg, "claude", "cswap-1")); !os.IsNotExist(err) {
		t.Fatalf("expected no orphaned directory for broken slot, got %v", err)
	}
}

func TestImportClaudeSwapDoesNotMarkActiveWhenLiveFilesDiffer(t *testing.T) {
	dir := t.TempDir()
	root := buildClaudeSwapFixture(t, dir)
	cfg := testClaudeImportConfig(dir)
	// Live credentials belong to some other login, not cswap slot 2. Marking
	// cswap-2 active would make the next switch sync a foreign login into
	// cswap-2's backup.
	if err := os.WriteFile(filepath.Join(dir, "live-credentials.json"), []byte(`{"claudeAiOauth":{"accessToken":"someone-else"}}`), 0o600); err != nil {
		t.Fatal(err)
	}

	result, err := ImportClaudeSwap(cfg, root)
	if err != nil {
		t.Fatal(err)
	}
	if result.Active != "" {
		t.Fatalf("expected no active account for mismatched live files, got %q", result.Active)
	}
	state, err := LoadState(cfg.StatePath)
	if err != nil {
		t.Fatal(err)
	}
	if got := state.Service("claude").ActiveAccount; got != "" {
		t.Fatalf("expected no active account in state, got %q", got)
	}
}

func TestSwitchAccountToActiveAccountIsNoop(t *testing.T) {
	dir := t.TempDir()
	active := filepath.Join(dir, "active-auth.json")
	cfg := testConfig(dir, active)
	captureWithUsage(t, cfg, "codex", active, "a1", "a", 10, 10)
	// Live rotation after capture: an explicit re-switch to the same account
	// must not clobber it with the stale backup.
	if err := os.WriteFile(active, []byte("a-rotated"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := SwitchAccount(cfg, "codex", "a"); err != nil {
		t.Fatal(err)
	}
	assertFileContent(t, active, "a-rotated")
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

func buildClaudeSwapFixture(t *testing.T, dir string) string {
	t.Helper()
	root := filepath.Join(dir, "claude-swap")
	for _, sub := range []string{"configs", "credentials", "cache"} {
		if err := os.MkdirAll(filepath.Join(root, sub), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	sequence := `{
		"activeAccountNumber": 2,
		"sequence": [1, 2],
		"accounts": {
			"1": {"email": "one@example.com", "added": "2026-07-01T20:00:00Z"},
			"2": {"email": "two@example.com", "added": "2026-07-01T21:00:00Z"}
		}
	}`
	if err := os.WriteFile(filepath.Join(root, "sequence.json"), []byte(sequence), 0o600); err != nil {
		t.Fatal(err)
	}
	writeClaudeSwapFixtureAccount(t, root, "1", "one@example.com", `{"claudeAiOauth":{"accessToken":"one"}}`, `{"account":"one"}`)
	writeClaudeSwapFixtureAccount(t, root, "2", "two@example.com", `{"claudeAiOauth":{"accessToken":"two"}}`, `{"account":"two"}`)
	return root
}

// An active account whose live credentials file is unusable must fall back to
// its backup copy instead of failing, and must not overwrite the live file.
func TestFetchClaudeUsageActiveFallsBackToBackupWhenLiveUnusable(t *testing.T) {
	dir := t.TempDir()
	live := filepath.Join(dir, "credentials.json")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer backup-token" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"five_hour":{"utilization":12},"seven_day":{"utilization":34}}`))
	}))
	t.Cleanup(server.Close)
	oldURL := claudeUsageURL
	claudeUsageURL = server.URL
	t.Cleanup(func() { claudeUsageURL = oldURL })

	cfg := Config{
		BackupRoot: filepath.Join(dir, "backups"),
		StatePath:  filepath.Join(dir, "state.json"),
		Services: []ServiceConfig{
			{Name: "claude", Kind: "claude", Files: []ManagedFile{requiredFile(live, "credentials.json")}},
		},
	}
	if err := os.WriteFile(live, []byte("not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	accountDir := AccountDir(cfg, "claude", "main")
	if err := os.MkdirAll(accountDir, 0o700); err != nil {
		t.Fatal(err)
	}
	backup := `{"claudeAiOauth":{"accessToken":"backup-token"}}`
	if err := os.WriteFile(filepath.Join(accountDir, "credentials.json"), []byte(backup), 0o600); err != nil {
		t.Fatal(err)
	}

	usage, err := fetchClaudeUsage(testContext(t), cfg, cfg.Services[0], AccountState{Name: "main"}, true)
	if err != nil {
		t.Fatal(err)
	}
	if ratio, ok := usage.FiveHour.Ratio(); !ok || ratio != 0.12 {
		t.Fatalf("unexpected five-hour ratio %v %v", ratio, ok)
	}
	assertFileContent(t, live, "not json")
}

// An inactive account must never read (or touch) the live credentials file,
// even when one exists with valid credentials.
func TestFetchClaudeUsageInactiveIgnoresLiveFile(t *testing.T) {
	dir := t.TempDir()
	live := filepath.Join(dir, "credentials.json")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer backup-token" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"five_hour":{"utilization":12},"seven_day":{"utilization":34}}`))
	}))
	t.Cleanup(server.Close)
	oldURL := claudeUsageURL
	claudeUsageURL = server.URL
	t.Cleanup(func() { claudeUsageURL = oldURL })

	cfg := Config{
		BackupRoot: filepath.Join(dir, "backups"),
		StatePath:  filepath.Join(dir, "state.json"),
		Services: []ServiceConfig{
			{Name: "claude", Kind: "claude", Files: []ManagedFile{requiredFile(live, "credentials.json")}},
		},
	}
	liveCredentials := `{"claudeAiOauth":{"accessToken":"live-token"}}`
	if err := os.WriteFile(live, []byte(liveCredentials), 0o600); err != nil {
		t.Fatal(err)
	}
	accountDir := AccountDir(cfg, "claude", "other")
	if err := os.MkdirAll(accountDir, 0o700); err != nil {
		t.Fatal(err)
	}
	backup := `{"claudeAiOauth":{"accessToken":"backup-token"}}`
	if err := os.WriteFile(filepath.Join(accountDir, "credentials.json"), []byte(backup), 0o600); err != nil {
		t.Fatal(err)
	}

	usage, err := fetchClaudeUsage(testContext(t), cfg, cfg.Services[0], AccountState{Name: "other"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if ratio, ok := usage.FiveHour.Ratio(); !ok || ratio != 0.12 {
		t.Fatalf("unexpected five-hour ratio %v %v", ratio, ok)
	}
	assertFileContent(t, live, liveCredentials)
}

// Restores and state writes must always land with owner-only permissions,
// even over a pre-existing looser-mode target.
func TestSwitchRestoresFilesWithOwnerOnlyPermissions(t *testing.T) {
	dir := t.TempDir()
	active := filepath.Join(dir, "active-auth.json")
	cfg := testConfig(dir, active)
	if err := os.WriteFile(active, []byte(`{"token":"one"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := CaptureAccount(cfg, "codex", "first", ""); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(active, []byte(`{"token":"two"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := CaptureAccount(cfg, "codex", "second", ""); err != nil {
		t.Fatal(err)
	}
	// Loosen the live file; the restore must tighten it back to 0600.
	if err := os.Chmod(active, 0o644); err != nil {
		t.Fatal(err)
	}

	if err := SwitchAccount(cfg, "codex", "first"); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{active, cfg.StatePath} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if perm := info.Mode().Perm(); perm != 0o600 {
			t.Fatalf("expected %s to have 0600 permissions, got %o", path, perm)
		}
	}
}
