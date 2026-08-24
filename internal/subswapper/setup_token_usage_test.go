package subswapper

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestClaudeSetupTokenDirectUsageIsSelectable(t *testing.T) {
	cfg, account := setupTokenUsageAccount(t)
	server := setupTokenUsageServer(t, http.StatusOK, `{
		"five_hour":{"utilization":12,"resets_at":"2030-01-01T00:00:00Z"},
		"seven_day":{"utilization":34,"resets_at":"2030-01-02T00:00:00Z"}
	}`)
	useClaudeUsageServer(t, server.URL)

	status := collectSetupTokenAccount(t, cfg, account)
	if !status.Selectable || status.Reason != "ready" {
		t.Fatalf("status = %#v", status)
	}
	if status.Account.Usage.Source != claudeUsageSourceSetupTokenDirect {
		t.Fatalf("usage source = %q", status.Account.Usage.Source)
	}
}

func TestClaudeSetupTokenUsageFallsBackOnlyToFreshStatusLineData(t *testing.T) {
	for _, test := range []struct {
		name       string
		observedAt time.Time
		wantReady  bool
	}{
		{name: "fresh", observedAt: time.Now().UTC().Add(-time.Minute), wantReady: true},
		{name: "stale", observedAt: time.Now().UTC().Add(-inactiveUsageTTL - time.Second)},
	} {
		t.Run(test.name, func(t *testing.T) {
			cfg, account := setupTokenUsageAccount(t)
			server := setupTokenUsageServer(t, http.StatusForbidden, `{"error":"missing user:profile"}`)
			useClaudeUsageServer(t, server.URL)

			state, err := LoadState(cfg.StatePath)
			if err != nil {
				t.Fatal(err)
			}
			stored := state.Service("claude").Accounts[account]
			stored.Usage = UsageSnapshot{
				FiveHour:      LimitWindow{Pct: PtrFloat64(25), ResetsAt: time.Now().Add(time.Hour)},
				Weekly:        LimitWindow{Pct: PtrFloat64(35), ResetsAt: time.Now().Add(24 * time.Hour)},
				ObservedAt:    test.observedAt,
				Source:        claudeUsageSourceStatusLine,
				TokenRevision: stored.SetupTokenRevision,
			}
			state.Service("claude").Accounts[account] = stored
			if err := SaveState(cfg.StatePath, state); err != nil {
				t.Fatal(err)
			}

			status := collectSetupTokenAccount(t, cfg, account)
			if status.Selectable != test.wantReady {
				t.Fatalf("selectable = %v, reason = %q", status.Selectable, status.Reason)
			}
			if !test.wantReady && !strings.Contains(status.Reason, "usage unavailable") {
				t.Fatalf("reason = %q", status.Reason)
			}
		})
	}
}

func TestClaudeSetupTokenUnavailableRejectedAndMissingAreUnselectable(t *testing.T) {
	for _, test := range []struct {
		name        string
		removeToken bool
		status      int
		wantReason  string
	}{
		{name: "unavailable", status: http.StatusForbidden, wantReason: "usage unavailable"},
		{name: "rejected", status: http.StatusUnauthorized, wantReason: "authentication rejected"},
		{name: "missing", removeToken: true, status: http.StatusOK, wantReason: "setup token missing"},
	} {
		t.Run(test.name, func(t *testing.T) {
			cfg, account := setupTokenUsageAccount(t)
			if test.removeToken {
				if err := RemoveClaudeSetupToken(context.Background(), cfg, "claude", account); err != nil {
					t.Fatal(err)
				}
			}
			server := setupTokenUsageServer(t, test.status, `{}`)
			useClaudeUsageServer(t, server.URL)

			status := collectSetupTokenAccount(t, cfg, account)
			if status.Selectable || !strings.Contains(status.Reason, test.wantReason) {
				t.Fatalf("status = %#v", status)
			}
		})
	}
}

func TestClaudeSetupTokenUsageCommandOverrideRunsAfterAuthenticationProbe(t *testing.T) {
	cfg, account := setupTokenUsageAccount(t)
	marker := filepath.Join(t.TempDir(), "usage-command-ran")
	cfg.Services[0].UsageCommand = []string{"sh", "-c", `touch "$1"; printf '{"five_hour":{"pct":17},"weekly":{"pct":29}}\n'`, "usage", marker}
	server := setupTokenUsageServer(t, http.StatusForbidden, `{"error":"missing user:profile"}`)
	useClaudeUsageServer(t, server.URL)

	status := collectSetupTokenAccount(t, cfg, account)
	if !status.Selectable || status.Account.Usage.Source != claudeUsageSourceCommand {
		t.Fatalf("status = %#v", status)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("usage command did not run: %v", err)
	}

	if err := os.Remove(marker); err != nil {
		t.Fatal(err)
	}
	if err := RemoveClaudeSetupToken(context.Background(), cfg, "claude", account); err != nil {
		t.Fatal(err)
	}
	status = collectSetupTokenAccount(t, cfg, account)
	_, markerErr := os.Stat(marker)
	if status.Selectable || !errors.Is(markerErr, os.ErrNotExist) {
		t.Fatalf("missing token ran usage override: status=%#v markerErr=%v", status, markerErr)
	}
}

func TestRecordClaudeStatusLineUsageRejectsWrongRevisionAndStoresFreshUsage(t *testing.T) {
	cfg, account := setupTokenUsageAccount(t)
	state, err := LoadState(cfg.StatePath)
	if err != nil {
		t.Fatal(err)
	}
	revision := state.Service("claude").Accounts[account].SetupTokenRevision
	now := time.Now().UTC()
	payload := []byte(`{"rate_limits":{"five_hour":{"used_percentage":21,"resets_at":4102444800},"seven_day":{"used_percentage":32,"resets_at":4102531200}}}`)

	wrong, err := ParseClaudeStatusLine(payload, now, "wrong-revision")
	if err != nil {
		t.Fatal(err)
	}
	if err := RecordClaudeStatusLineUsage(context.Background(), cfg, "claude", account, wrong); err == nil {
		t.Fatal("wrong token revision was accepted")
	}

	sample, err := ParseClaudeStatusLine(payload, now, revision)
	if err != nil {
		t.Fatal(err)
	}
	if err := RecordClaudeStatusLineUsage(context.Background(), cfg, "claude", account, sample); err != nil {
		t.Fatal(err)
	}
	state, err = LoadState(cfg.StatePath)
	if err != nil {
		t.Fatal(err)
	}
	usage := state.Service("claude").Accounts[account].Usage
	if usage.Source != claudeUsageSourceStatusLine || usage.TokenRevision != revision || !usage.HasCoreLimits() {
		t.Fatalf("stored usage = %#v", usage)
	}
	if !state.Service("claude").Accounts[account].LastProbeStartedAt.Equal(now) {
		t.Fatalf("status-line observation did not order concurrent probes: %#v", state.Service("claude").Accounts[account])
	}
	if usage.FableWeekly.Pct != nil {
		t.Fatalf("unsupported Fable usage was stored: %#v", usage.FableWeekly)
	}
	olderProbe := NewState()
	olderAccount := state.Service("claude").Accounts[account]
	olderAccount.LastProbeStartedAt = now.Add(-time.Second)
	olderAccount.Usage = UsageSnapshot{ObservedAt: now.Add(time.Second)}
	olderProbe.Service("claude").Accounts[account] = olderAccount
	mergeProbeState(state, olderProbe)
	if got := state.Service("claude").Accounts[account].Usage.Source; got != claudeUsageSourceStatusLine {
		t.Fatalf("older provider probe overwrote status-line usage: %q", got)
	}
}

func TestRecordClaudeStatusLineUsageCannotReviveRejectedAuthentication(t *testing.T) {
	cfg, account := setupTokenUsageAccount(t)
	state, err := LoadState(cfg.StatePath)
	if err != nil {
		t.Fatal(err)
	}
	stored := state.Service("claude").Accounts[account]
	stored.CredentialsError = "setup token authentication rejected"
	stored.FetchBackoffUntil = time.Now().UTC().Add(time.Hour)
	state.Service("claude").Accounts[account] = stored
	if err := SaveState(cfg.StatePath, state); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	payload := []byte(`{"rate_limits":{"five_hour":{"used_percentage":21,"resets_at":4102444800},"seven_day":{"used_percentage":32,"resets_at":4102531200}}}`)
	sample, err := ParseClaudeStatusLine(payload, now, stored.SetupTokenRevision)
	if err != nil {
		t.Fatal(err)
	}
	if err := RecordClaudeStatusLineUsage(context.Background(), cfg, "claude", account, sample); err == nil {
		t.Fatal("status-line usage revived rejected authentication")
	}
	state, err = LoadState(cfg.StatePath)
	if err != nil {
		t.Fatal(err)
	}
	stored = state.Service("claude").Accounts[account]
	if stored.CredentialsError == "" || stored.Usage.HasAnyLimit() {
		t.Fatalf("rejected authentication was cleared: %#v", stored)
	}
}

func TestClaudeSetupTokenExpiredAccountIsUnselectable(t *testing.T) {
	fixed := time.Date(2026, time.August, 24, 12, 0, 0, 0, time.UTC)
	originalNow := claudeSetupTokenNow
	claudeSetupTokenNow = func() time.Time { return fixed }
	t.Cleanup(func() { claudeSetupTokenNow = originalNow })
	cfg, account := setupTokenUsageAccount(t)
	claudeSetupTokenNow = func() time.Time { return fixed.Add(claudeSetupTokenLifetime) }
	status := collectSetupTokenAccount(t, cfg, account)
	if status.Selectable || !strings.Contains(status.Reason, "setup token expired") {
		t.Fatalf("status = %#v", status)
	}
}

func TestProbeFromReplacedSetupTokenCannotMergeOrSwitch(t *testing.T) {
	addedAt := time.Now().UTC()
	current := NewState()
	current.Service("claude").Accounts["work"] = AccountState{
		Name:               "work",
		AddedAt:            addedAt,
		SetupTokenRevision: "new-revision",
		Usage:              UsageSnapshot{ObservedAt: addedAt},
	}
	probed := NewState()
	probed.Service("claude").Accounts["work"] = AccountState{
		Name:               "work",
		AddedAt:            addedAt,
		SetupTokenRevision: "old-revision",
		Usage:              UsageSnapshot{ObservedAt: addedAt.Add(time.Second)},
	}

	mergeProbeState(current, probed)
	account := current.Service("claude").Accounts["work"]
	if account.SetupTokenRevision != "new-revision" || !account.Usage.ObservedAt.Equal(addedAt) {
		t.Fatalf("old-token probe merged into current state: %#v", account)
	}
	if serviceStateMatchesSnapshot(current.Service("claude"), probed.Service("claude")) {
		t.Fatal("old-token snapshot remained eligible for switching")
	}
}

func setupTokenUsageAccount(t *testing.T) (Config, string) {
	t.Helper()
	dir := t.TempDir()
	cfg := setupTokenTestConfig(dir)
	cfg.ApplyDefaults()
	registerSetupTokenTestAccount(t, cfg, "work")
	if _, err := ReplaceClaudeSetupToken(context.Background(), cfg, "claude", "work", "setup-token-work", nil); err != nil {
		t.Fatal(err)
	}
	return cfg, "work"
}

func setupTokenUsageServer(t *testing.T, status int, body string) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer setup-token-work" {
			t.Fatalf("authorization = %q", got)
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(server.Close)
	return server
}

func useClaudeUsageServer(t *testing.T, url string) {
	t.Helper()
	oldURL := claudeUsageURL
	claudeUsageURL = url
	t.Cleanup(func() { claudeUsageURL = oldURL })
}

func collectSetupTokenAccount(t *testing.T, cfg Config, account string) AccountStatus {
	t.Helper()
	state, err := LoadState(cfg.StatePath)
	if err != nil {
		t.Fatal(err)
	}
	result := CollectService(context.Background(), cfg, state, cfg.Services[0])
	if len(result.Accounts) != 1 || result.Accounts[0].Account.Name != account {
		t.Fatalf("accounts = %#v", result.Accounts)
	}
	return result.Accounts[0]
}
