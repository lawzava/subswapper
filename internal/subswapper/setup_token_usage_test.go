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

func TestClaudeSetupTokenInactiveProxyUsageRefresh(t *testing.T) {
	for _, test := range []struct {
		name     string
		age      time.Duration
		invalid  bool
		wantGETs int
	}{
		{name: "fresh", age: time.Minute},
		{name: "TTL reached", age: inactiveUsageTTL, wantGETs: 1},
		{name: "seven days old", age: 7 * 24 * time.Hour, wantGETs: 1},
		{name: "future", age: -time.Hour, wantGETs: 1},
		{name: "wrong revision", age: time.Minute, invalid: true, wantGETs: 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			cfg, name := setupTokenUsageAccount(t)
			upstream := newProxyUpstream(t)
			upstream.respond("setup-token-work", setupTokenMetadataResponse(t, http.StatusOK))
			useClaudeUsageServer(t, upstream.server.URL+"/api/oauth/usage")
			state, err := LoadState(cfg.StatePath)
			if err != nil {
				t.Fatal(err)
			}
			account := state.Service("claude").Accounts[name]
			account.ProxyUsage = setupTokenProxySample(account.SetupTokenRevision, time.Now().UTC().Add(-test.age))
			if test.invalid {
				account.ProxyUsage.TokenRevision = "previous-token"
			}
			state.Service("claude").Accounts[name] = account
			if err := SaveState(cfg.StatePath, state); err != nil {
				t.Fatal(err)
			}

			status := collectSetupTokenAccount(t, cfg, name)
			if calls := upstream.recorded(); len(calls) != test.wantGETs {
				t.Fatalf("metadata GETs = %d, want %d", len(calls), test.wantGETs)
			}
			if !status.Selectable || status.Reason != "ready" {
				t.Fatalf("selectable = %v, reason = %q", status.Selectable, status.Reason)
			}
			if test.wantGETs == 1 {
				if status.Account.Usage.Source != claudeUsageSourceSetupTokenDirect ||
					*status.Account.Usage.Weekly.Pct != 34 ||
					(test.age >= 0 && !status.Account.Usage.ObservedAt.After(account.ProxyUsage.ObservedAt)) {
					t.Fatalf("usage was not refreshed: %#v", status.Account.Usage)
				}
			} else if !status.Account.Usage.ObservedAt.Equal(account.ProxyUsage.ObservedAt) {
				t.Fatal("cached observation timestamp changed")
			}
		})
	}
}

func TestClaudeSetupTokenInactiveDirectAndCommandCacheStillSkipsProbe(t *testing.T) {
	for _, source := range []string{claudeUsageSourceSetupTokenDirect, claudeUsageSourceCommand} {
		t.Run(source, func(t *testing.T) {
			cfg, name := setupTokenUsageAccount(t)
			upstream := newProxyUpstream(t)
			upstream.respond("setup-token-work", setupTokenMetadataResponse(t, http.StatusUnauthorized))
			useClaudeUsageServer(t, upstream.server.URL+"/api/oauth/usage")
			state, err := LoadState(cfg.StatePath)
			if err != nil {
				t.Fatal(err)
			}
			account := state.Service("claude").Accounts[name]
			account.Usage = setupTokenProxySample(account.SetupTokenRevision, time.Now().UTC().Add(-time.Minute))
			account.Usage.Source = source
			account.ProxyUsage = setupTokenProxySample(account.SetupTokenRevision, time.Now().UTC().Add(-time.Hour))
			state.Service("claude").Accounts[name] = account
			if err := SaveState(cfg.StatePath, state); err != nil {
				t.Fatal(err)
			}
			status := collectSetupTokenAccount(t, cfg, name)
			if !status.Selectable || status.Reason != "ready" || status.Account.Usage.Source != source ||
				!status.Account.Usage.ObservedAt.Equal(account.Usage.ObservedAt) {
				t.Fatalf("cached %s usage was not retained", source)
			}
			if calls := upstream.recorded(); len(calls) != 0 {
				t.Fatalf("fresh %s cache made %d metadata GETs", source, len(calls))
			}
		})
	}
}

func TestClaudeSetupTokenStaleProxyFallbackRetriesAfterBackoff(t *testing.T) {
	cfg, name := setupTokenUsageAccount(t)
	upstream := newProxyUpstream(t)
	upstream.respond("setup-token-work", setupTokenMetadataResponse(t, http.StatusForbidden))
	useClaudeUsageServer(t, upstream.server.URL+"/api/oauth/usage")
	state, err := LoadState(cfg.StatePath)
	if err != nil {
		t.Fatal(err)
	}
	account := state.Service("claude").Accounts[name]
	observed := time.Now().UTC().Add(-7 * 24 * time.Hour)
	account.ProxyUsage = setupTokenProxySample(account.SetupTokenRevision, observed)
	state.Service("claude").Accounts[name] = account
	if err := SaveState(cfg.StatePath, state); err != nil {
		t.Fatal(err)
	}
	for cycleNumber := 0; cycleNumber < 3; cycleNumber++ {
		if cycleNumber == 2 {
			state, err = LoadState(cfg.StatePath)
			if err != nil {
				t.Fatal(err)
			}
			account = state.Service("claude").Accounts[name]
			account.FetchBackoffUntil = time.Now().UTC().Add(-time.Second)
			state.Service("claude").Accounts[name] = account
			if err := SaveState(cfg.StatePath, state); err != nil {
				t.Fatal(err)
			}
		}
		cycle := MonitorOnce(context.Background(), cfg, false)
		if len(cycle.Errors) != 0 || len(cycle.Results) != 1 || len(cycle.Results[0].Accounts) != 1 {
			t.Fatalf("cycle = %#v", cycle)
		}
		status := cycle.Results[0].Accounts[0]
		if !status.Selectable || status.Reason != "ready (stale Claude proxy usage; setup-token usage unavailable)" ||
			!status.Account.Usage.ObservedAt.Equal(observed) || !status.Account.ProxyUsage.ObservedAt.Equal(observed) {
			t.Fatalf("cycle %d: selectable=%v reason=%q observed=%v", cycleNumber, status.Selectable, status.Reason, status.Account.Usage.ObservedAt)
		}
		if remaining := time.Until(status.Account.FetchBackoffUntil); remaining <= 0 || remaining > cfg.Monitor.Interval.Duration {
			t.Fatalf("backoff = %v", remaining)
		}
		wantGETs := 1
		if cycleNumber == 2 {
			wantGETs = 2
		}
		if calls := upstream.recorded(); len(calls) != wantGETs {
			t.Fatalf("cycle %d: metadata GETs = %d, want %d", cycleNumber, len(calls), wantGETs)
		}
	}
}

func TestClaudeSetupTokenProxyCacheCannotHideAuthenticationRejection(t *testing.T) {
	for _, test := range []struct {
		name     string
		source   string
		age      time.Duration
		rejected bool
		backoff  time.Duration
		wantGETs int
	}{
		{name: "stale proxy GET rejects", source: claudeUsageSourceProxy, age: time.Hour, wantGETs: 1},
		{name: "stale proxy rejection backoff", source: claudeUsageSourceProxy, age: time.Hour, rejected: true, backoff: time.Hour},
		{name: "fresh proxy rejection backoff", source: claudeUsageSourceProxy, age: time.Minute, rejected: true, backoff: time.Hour},
		{name: "fresh direct rejection backoff", source: claudeUsageSourceSetupTokenDirect, age: time.Minute, rejected: true, backoff: time.Hour},
		{name: "fresh command rejection backoff", source: claudeUsageSourceCommand, age: time.Minute, rejected: true, backoff: time.Hour},
		{name: "fresh proxy rejection retry", source: claudeUsageSourceProxy, age: time.Minute, rejected: true, backoff: -time.Second, wantGETs: 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			cfg, name := setupTokenUsageAccount(t)
			upstream := newProxyUpstream(t)
			upstream.respond("setup-token-work", setupTokenMetadataResponse(t, http.StatusUnauthorized))
			useClaudeUsageServer(t, upstream.server.URL+"/api/oauth/usage")
			state, err := LoadState(cfg.StatePath)
			if err != nil {
				t.Fatal(err)
			}
			account := state.Service("claude").Accounts[name]
			account.ProxyUsage = setupTokenProxySample(account.SetupTokenRevision, time.Now().UTC().Add(-test.age))
			account.Usage = account.ProxyUsage
			account.Usage.Source = test.source
			if test.rejected {
				account.CredentialsError = "setup token authentication rejected"
				account.FetchBackoffUntil = time.Now().UTC().Add(test.backoff)
			}
			state.Service("claude").Accounts[name] = account
			if err := SaveState(cfg.StatePath, state); err != nil {
				t.Fatal(err)
			}
			status := collectSetupTokenAccount(t, cfg, name)
			if status.Selectable || status.Reason != "setup token authentication rejected" {
				t.Fatalf("selectable=%v reason=%q", status.Selectable, status.Reason)
			}
			if calls := upstream.recorded(); len(calls) != test.wantGETs {
				t.Fatalf("metadata GETs = %d, want %d", len(calls), test.wantGETs)
			}
			if test.wantGETs != 0 && status.Account.Usage.HasAnyLimit() {
				t.Fatal("401 retained selectable usage")
			}
		})
	}
}

func TestClaudeSetupTokenMonitorRefreshesAllAccountsWithoutSwitchOrRotation(t *testing.T) {
	upstream := newProxyUpstream(t)
	cfg, _ := setupProxyAccounts(t, upstream.server.URL)
	registerSetupTokenTestAccount(t, cfg, "c")
	if _, err := ReplaceClaudeSetupToken(context.Background(), cfg, "claude", "c", "setup-token-c", nil); err != nil {
		t.Fatal(err)
	}
	useClaudeUsageServer(t, upstream.server.URL+"/api/oauth/usage")
	state, err := LoadState(cfg.StatePath)
	if err != nil {
		t.Fatal(err)
	}
	revisions := make(map[string]string)
	for _, name := range []string{"a", "b", "c"} {
		upstream.respond("setup-token-"+name, setupTokenMetadataResponse(t, http.StatusOK))
		account := state.Service("claude").Accounts[name]
		revisions[name] = account.SetupTokenRevision
		account.ProxyUsage = setupTokenProxySample(account.SetupTokenRevision, time.Now().UTC().Add(-7*24*time.Hour))
		state.Service("claude").Accounts[name] = account
	}
	if err := SaveState(cfg.StatePath, state); err != nil {
		t.Fatal(err)
	}
	// Keep a selected account exhausted while alternatives have headroom, so
	// an accidental automatic switch would be observable.
	upstream.respond("setup-token-a", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.RequestURI() != "/api/oauth/usage" {
			t.Errorf("unexpected metadata request: %s %s", r.Method, r.URL.RequestURI())
		}
		_, _ = w.Write([]byte(`{"seven_day":{"utilization":100,"resets_at":"2030-01-02T00:00:00Z"}}`))
	})
	cycle := MonitorOnce(context.Background(), cfg, false)
	if len(cycle.Errors) != 0 || len(cycle.Switches) != 0 {
		t.Fatalf("errors=%v switches=%v", cycle.Errors, cycle.Switches)
	}
	calls := upstream.recorded()
	if len(calls) != 3 {
		t.Fatalf("metadata GETs = %d, want 3", len(calls))
	}
	for i, name := range []string{"a", "b", "c"} {
		if calls[i].Authorization != "Bearer setup-token-"+name || calls[i].Path != "/api/oauth/usage" || calls[i].Body != "" {
			t.Fatalf("account %s did not make its metadata-only GET", name)
		}
	}
	after, err := LoadState(cfg.StatePath)
	if err != nil {
		t.Fatal(err)
	}
	if after.Service("claude").ActiveAccount != "a" || !after.Service("claude").LastSwitchedAt.Equal(state.Service("claude").LastSwitchedAt) {
		t.Fatal("monitor with auto-switch disabled changed selection")
	}
	for name, revision := range revisions {
		account := after.Service("claude").Accounts[name]
		token, tokenStatus, err := LoadClaudeSetupTokenWithStatus(cfg, "claude", name)
		if err != nil {
			t.Fatal(err)
		}
		if account.SetupTokenRevision != revision || tokenStatus.Revision != revision || token != "setup-token-"+name {
			t.Fatalf("account %s token or revision changed", name)
		}
		wantPct := 34.0
		if name == "a" {
			wantPct = 100
		}
		if account.Usage.Source != claudeUsageSourceSetupTokenDirect || *account.Usage.Weekly.Pct != wantPct {
			t.Fatalf("account %s was not refreshed", name)
		}
	}
}

func setupTokenProxySample(revision string, observed time.Time) UsageSnapshot {
	return UsageSnapshot{
		Weekly:        LimitWindow{Pct: PtrFloat64(35), ResetsAt: time.Now().UTC().Add(24 * time.Hour)},
		ObservedAt:    observed,
		Source:        claudeUsageSourceProxy,
		TokenRevision: revision,
	}
}

func setupTokenMetadataResponse(t *testing.T, status int) func(http.ResponseWriter, *http.Request) {
	t.Helper()
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.RequestURI() != "/api/oauth/usage" || r.ContentLength > 0 {
			t.Errorf("unexpected metadata request: %s %s length=%d", r.Method, r.URL.RequestURI(), r.ContentLength)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.WriteHeader(status)
		if status == http.StatusOK {
			_, _ = w.Write([]byte(`{"seven_day":{"utilization":34,"resets_at":"2030-01-02T00:00:00Z"}}`))
		} else {
			_, _ = w.Write([]byte(`{"error":"private provider detail must not appear in status"}`))
		}
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

func TestClaudeSetupTokenDirectUsageWithoutFiveHourWindowIsSelectable(t *testing.T) {
	for _, body := range []string{
		`{"seven_day":{"utilization":34,"resets_at":"2030-01-02T00:00:00Z"}}`,
		`{"five_hour":null,"seven_day":{"utilization":34,"resets_at":"2030-01-02T00:00:00Z"}}`,
	} {
		cfg, account := setupTokenUsageAccount(t)
		server := setupTokenUsageServer(t, http.StatusOK, body)
		useClaudeUsageServer(t, server.URL)

		status := collectSetupTokenAccount(t, cfg, account)
		if !status.Selectable || status.Reason != "ready" || status.Score != 0.34 {
			t.Fatalf("body %s: status = %#v", body, status)
		}
		if status.Account.Usage.FiveHour != (LimitWindow{}) {
			t.Fatalf("body %s: five-hour = %#v", body, status.Account.Usage.FiveHour)
		}
	}
}

func TestClaudeSetupTokenStatusLineUsageWithoutFiveHourWindowIsTrusted(t *testing.T) {
	cfg, account := setupTokenUsageAccount(t)
	server := setupTokenUsageServer(t, http.StatusForbidden, `{"error":"missing user:profile"}`)
	useClaudeUsageServer(t, server.URL)

	state, err := LoadState(cfg.StatePath)
	if err != nil {
		t.Fatal(err)
	}
	stored := state.Service("claude").Accounts[account]
	stored.Usage = UsageSnapshot{
		Weekly:        LimitWindow{Pct: PtrFloat64(35), ResetsAt: time.Now().Add(24 * time.Hour)},
		ObservedAt:    time.Now().UTC().Add(-time.Minute),
		Source:        claudeUsageSourceStatusLine,
		TokenRevision: stored.SetupTokenRevision,
	}
	state.Service("claude").Accounts[account] = stored
	if err := SaveState(cfg.StatePath, state); err != nil {
		t.Fatal(err)
	}
	if status := collectSetupTokenAccount(t, cfg, account); !status.Selectable {
		t.Fatalf("status = %#v", status)
	}
}
