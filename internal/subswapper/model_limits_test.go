package subswapper

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"
)

func modelLimitTestUsage(weekly, fable float64, now time.Time) UsageSnapshot {
	return UsageSnapshot{
		Weekly:      LimitWindow{Pct: PtrFloat64(weekly), ResetsAt: now.Add(48 * time.Hour)},
		FableWeekly: LimitWindow{Pct: PtrFloat64(fable), ResetsAt: now.Add(72 * time.Hour)},
		ObservedAt:  now,
	}
}

func TestFullFableWindowKeepsAccountSelectable(t *testing.T) {
	now := time.Now().UTC()
	usage := modelLimitTestUsage(53, 100, now)
	if usage.Exhausted() {
		t.Fatal("a full Fable window exhausted the core windows")
	}
	if !usage.ModelExhausted("claude-Fable-5-2") || usage.ModelExhausted("claude-opus-5-5") || usage.ModelExhausted("") {
		t.Fatal("the Fable window must be full for Fable models only")
	}
	if models := usage.ExhaustedModels(); len(models) != 1 || models[0] != "fable" {
		t.Fatalf("exhausted models = %v", models)
	}
	for name, mark := range map[string]func(*AccountStatus){
		"setup token": markClaudeSetupTokenSelectable,
		"generic":     markAccountSelectable,
	} {
		status := AccountStatus{Service: "claude", Account: AccountState{Name: "a", Usage: usage}}
		mark(&status)
		if !status.Selectable || status.Reason != "ready (fable limit reached)" || status.Score != 1 {
			t.Fatalf("%s: selectable=%v reason=%q score=%v", name, status.Selectable, status.Reason, status.Score)
		}
		best, ok := BestAccount([]AccountStatus{status}, 0.9, defaultStaleUsageAfter, now)
		if !ok || best.Account.Name != "a" {
			t.Fatalf("%s: BestAccount = %q, %v", name, best.Account.Name, ok)
		}
		if state := historyState(status); state != "ready" {
			t.Fatalf("%s: history state = %q", name, state)
		}

		full := AccountStatus{Service: "claude", Account: AccountState{Name: "a", Usage: modelLimitTestUsage(100, 10, now)}}
		mark(&full)
		if full.Selectable || full.Reason != "limit reached" {
			t.Fatalf("%s: weekly-full selectable=%v reason=%q", name, full.Selectable, full.Reason)
		}
	}
}

func TestBestAccountFindsCandidateWhenEveryFableWindowIsFull(t *testing.T) {
	now := time.Now().UTC()
	var accounts []AccountStatus
	for name, weekly := range map[string]float64{"a": 80, "b": 53, "c": 20} {
		status := AccountStatus{Service: "claude", Account: AccountState{Name: name, Usage: modelLimitTestUsage(weekly, 100, now)}}
		markClaudeSetupTokenSelectable(&status)
		accounts = append(accounts, status)
	}
	best, ok := BestAccount(accounts, 0.9, defaultStaleUsageAfter, now)
	if !ok {
		t.Fatal("no selectable account although only the Fable windows are full")
	}
	// Every worst window is 100%, so the lowest average decides.
	if best.Account.Name != "c" {
		t.Fatalf("best = %q, want c", best.Account.Name)
	}

	// A cooler account without a full window still wins.
	cool := AccountStatus{Service: "claude", Account: AccountState{Name: "d", Usage: modelLimitTestUsage(85, 10, now)}}
	markClaudeSetupTokenSelectable(&cool)
	if best, _ := BestAccount(append(accounts, cool), 0.9, defaultStaleUsageAfter, now); best.Account.Name != "d" {
		t.Fatalf("best = %q, want d", best.Account.Name)
	}
}

func TestStaleProxyUsageSuffixComposesWithModelLimitReason(t *testing.T) {
	now := time.Now().UTC()
	usage := modelLimitTestUsage(53, 100, now)
	usage.Source = claudeUsageSourceProxy
	usage.TokenRevision = "rev"
	usage.ObservedAt = now.Add(-2 * inactiveUsageTTL)
	state := &ServiceState{Accounts: map[string]AccountState{}}
	status := AccountStatus{Service: "claude", Account: AccountState{Name: "a", ProxyUsage: usage}}
	if !adoptClaudeProxyUsage(state, &status, now, "rev") {
		t.Fatal("proxy usage not adopted")
	}
	want := "ready (fable limit reached) (stale Claude proxy usage; setup-token usage unavailable)"
	if !status.Selectable || status.Reason != want {
		t.Fatalf("selectable=%v reason=%q", status.Selectable, status.Reason)
	}
}

func TestRenderMonitorEventsTreatsModelLimitAsReady(t *testing.T) {
	statuses := func(reason string) []ServiceStatus {
		return []ServiceStatus{{
			Service:  ServiceConfig{Name: "claude"},
			Accounts: []AccountStatus{{Service: "claude", Account: AccountState{Name: "a"}, Reason: reason, Selectable: true}},
		}}
	}
	ready, modelFull := statuses("ready"), statuses("ready (fable limit reached)")
	for name, events := range map[string]string{
		"reached": RenderMonitorEvents(ready, modelFull, nil),
		"cleared": RenderMonitorEvents(modelFull, ready, nil),
	} {
		if events != "" {
			t.Fatalf("%s: model limit emitted %q", name, events)
		}
	}
	stale := statuses("ready (fable limit reached) (stale Claude proxy usage; setup-token usage unavailable)")
	if events := RenderMonitorEvents(modelFull, stale, nil); !strings.Contains(events, "error claude/a") {
		t.Fatalf("stale usage on a model-limited account was not reported: %q", events)
	}
	if events := RenderMonitorEvents(ready, statuses("limit reached"), nil); !strings.Contains(events, "error claude/a: limit reached") {
		t.Fatalf("core limit was not reported: %q", events)
	}
}

func TestClaudeRequestModel(t *testing.T) {
	for body, want := range map[string]string{
		`{"model":"claude-fable-5-2","messages":[{"model":"nested"}]}`: "claude-fable-5-2",
		`{"messages":[]}`: "",
		`{"model":7}`:     "",
		`[1,2]`:           "",
		`not json`:        "",
		``:                "",
	} {
		if got := claudeRequestModel([]byte(body)); got != want {
			t.Fatalf("claudeRequestModel(%q) = %q, want %q", body, got, want)
		}
	}
}

func TestClaudeProxyRoutesFableRequestsAwayFromFullFableWindow(t *testing.T) {
	for _, tc := range []struct {
		name, model, wantToken, wantActive string
	}{
		{"fable request", "claude-fable-5-2", "setup-token-b", "b"},
		{"other model", "claude-opus-5-5", "setup-token-a", "a"},
		{"no model", "", "setup-token-a", "a"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			upstream := newProxyUpstream(t)
			cfg, proxy := setupProxyAccounts(t, upstream.server.URL)
			state, err := LoadState(cfg.StatePath)
			if err != nil {
				t.Fatal(err)
			}
			now := time.Now().UTC()
			for name, fable := range map[string]float64{"a": 100, "b": 40} {
				account := state.Service("claude").Accounts[name]
				account.ProxyUsage = UsageSnapshot{
					FiveHour:      LimitWindow{Pct: PtrFloat64(10), ResetsAt: now.Add(time.Hour)},
					Weekly:        LimitWindow{Pct: PtrFloat64(53), ResetsAt: now.Add(24 * time.Hour)},
					FableWeekly:   LimitWindow{Pct: PtrFloat64(fable), ResetsAt: now.Add(48 * time.Hour)},
					ObservedAt:    now,
					Source:        claudeUsageSourceProxy,
					TokenRevision: account.SetupTokenRevision,
				}
				state.Service("claude").Accounts[name] = account
			}
			if err := SaveState(cfg.StatePath, state); err != nil {
				t.Fatal(err)
			}
			for _, name := range []string{"a", "b"} {
				upstream.respond("setup-token-"+name, func(w http.ResponseWriter, r *http.Request) {
					rateLimitHeaders(w, 0.1, 0.53, "allowed")
					w.WriteHeader(http.StatusOK)
				})
			}

			body := `{}`
			if tc.model != "" {
				body = `{"model":"` + tc.model + `"}`
			}
			if recorder := proxyRequest(t, proxy, proxy.secret, body); recorder.Code != http.StatusOK {
				t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
			}
			calls := upstream.recorded()
			if len(calls) != 1 || calls[0].Authorization != "Bearer "+tc.wantToken {
				t.Fatalf("upstream calls = %#v", calls)
			}
			state, err = LoadState(cfg.StatePath)
			if err != nil {
				t.Fatal(err)
			}
			if active := state.Service("claude").ActiveAccount; active != tc.wantActive {
				t.Fatalf("active account = %q, want %q", active, tc.wantActive)
			}
		})
	}
}

func TestClaudeProxyRelaysFableRejectionWithoutRetrying(t *testing.T) {
	upstream := newProxyUpstream(t)
	cfg, proxy := setupProxyAccounts(t, upstream.server.URL)
	fableReset := time.Now().Add(48 * time.Hour).Unix()
	upstream.respond("setup-token-a", func(w http.ResponseWriter, r *http.Request) {
		rateLimitHeaders(w, 0.1, 0.53, "rejected")
		w.Header().Set("anthropic-ratelimit-unified-representative-claim", "seven_day_overage_included")
		w.Header().Set("anthropic-ratelimit-unified-7d_oi-utilization", "1")
		w.Header().Set("anthropic-ratelimit-unified-7d_oi-reset", strconv.FormatInt(fableReset, 10))
		w.Header().Set("anthropic-ratelimit-unified-7d_oi-status", "rejected")
		w.WriteHeader(http.StatusTooManyRequests)
	})
	if recorder := proxyRequest(t, proxy, proxy.secret, `{"model":"claude-fable-5-2"}`); recorder.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d", recorder.Code)
	}
	if calls := upstream.recorded(); len(calls) != 1 {
		t.Fatalf("a Fable rejection was replayed: %#v", calls)
	}
	state, err := LoadState(cfg.StatePath)
	if err != nil {
		t.Fatal(err)
	}
	usage := state.Service("claude").Accounts["a"].ProxyUsage
	if usage.Exhausted() || !usage.ModelExhausted("claude-fable-5-2") {
		t.Fatalf("recorded usage = %#v", usage)
	}
	if active := state.Service("claude").ActiveAccount; active != "a" {
		t.Fatalf("active account = %q, want a", active)
	}
}

func TestPassedResetMakesFullAccountSelectableAndHidesReset(t *testing.T) {
	now := time.Now().UTC()
	past := now.Add(-time.Hour)
	usage := UsageSnapshot{
		FiveHour:   LimitWindow{Pct: PtrFloat64(100), ResetsAt: past},
		Weekly:     LimitWindow{Pct: PtrFloat64(100), ResetsAt: past},
		ObservedAt: now.Add(-2 * time.Hour),
	}
	status := AccountStatus{Service: "claude", Active: true, Account: AccountState{Name: "a", Usage: usage}}
	markClaudeSetupTokenSelectable(&status)
	if !status.Selectable || status.Score != 0 || status.Reason != "ready" {
		t.Fatalf("selectable=%v score=%v reason=%q", status.Selectable, status.Score, status.Reason)
	}
	if best, ok := BestAccount([]AccountStatus{status}, 0.9, defaultStaleUsageAfter, now); !ok || best.Account.Name != "a" {
		t.Fatalf("BestAccount = %q, %v", best.Account.Name, ok)
	}

	results := []ServiceStatus{{Service: ServiceConfig{Name: "claude", Kind: "claude"}, Accounts: []AccountStatus{status}}}
	rendered := RenderStatus(results, nil, now, defaultStaleUsageAfter)
	var row string
	for _, line := range strings.Split(rendered, "\n") {
		if strings.HasPrefix(line, "claude") {
			row = line
		}
	}
	if !strings.Contains(row, "0%") || strings.Contains(row, "100%") || strings.Contains(row, "reset") {
		t.Fatalf("status row shows a passed window: %q", row)
	}

	account := BuildStatusReport(results, now).Services[0].Accounts[0]
	if account.Weekly == nil || account.Weekly.UsedPercent != 0 || !account.Weekly.ResetsAt.IsZero() ||
		account.FiveHour == nil || account.FiveHour.UsedPercent != 0 || !account.FiveHour.ResetsAt.IsZero() {
		t.Fatalf("report windows = %#v %#v", account.FiveHour, account.Weekly)
	}
	encoded, err := json.Marshal(account)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "resets_at") {
		t.Fatalf("report JSON keeps a passed reset: %s", encoded)
	}

	// A running window keeps its reset in both outputs.
	running := LimitWindow{Pct: PtrFloat64(40), ResetsAt: now.Add(time.Hour)}
	if got := formatWindow(running); !strings.Contains(got, "40% reset ") {
		t.Fatalf("formatWindow(running) = %q", got)
	}
	if got := statusWindow(running); got == nil || !got.ResetsAt.Equal(running.ResetsAt) {
		t.Fatalf("statusWindow(running) = %#v", got)
	}
}

// A Fable limit known only from a metadata probe must survive the first
// proxied response, which carries no Fable window for other models.
func TestClaudeProxyKeepsProbedFableLimitAcrossOtherModelResponse(t *testing.T) {
	reset := time.Now().UTC().Add(48 * time.Hour)
	for name, fable := range map[string]LimitWindow{
		"percentage":     {Pct: PtrFloat64(100), ResetsAt: reset},
		"used and limit": {Used: 100, Limit: 100, ResetsAt: reset},
	} {
		t.Run(name, func(t *testing.T) {
			upstream := newProxyUpstream(t)
			cfg, proxy := setupProxyAccounts(t, upstream.server.URL)
			state, err := LoadState(cfg.StatePath)
			if err != nil {
				t.Fatal(err)
			}
			now := time.Now().UTC()
			account := state.Service("claude").Accounts["a"]
			account.Usage = UsageSnapshot{
				Weekly:        LimitWindow{Pct: PtrFloat64(53), ResetsAt: now.Add(24 * time.Hour)},
				FableWeekly:   fable,
				ObservedAt:    now,
				Source:        claudeUsageSourceCommand,
				TokenRevision: account.SetupTokenRevision,
			}
			state.Service("claude").Accounts["a"] = account
			if err := SaveState(cfg.StatePath, state); err != nil {
				t.Fatal(err)
			}
			for _, name := range []string{"a", "b"} {
				upstream.respond("setup-token-"+name, func(w http.ResponseWriter, r *http.Request) {
					rateLimitHeaders(w, 0.1, 0.53, "allowed")
					w.WriteHeader(http.StatusOK)
				})
			}
			for _, model := range []string{"claude-opus-5-5", "claude-fable-5-2"} {
				if recorder := proxyRequest(t, proxy, proxy.secret, `{"model":"`+model+`"}`); recorder.Code != http.StatusOK {
					t.Fatalf("%s status = %d", model, recorder.Code)
				}
			}
			calls := upstream.recorded()
			if len(calls) != 2 || calls[0].Authorization != "Bearer setup-token-a" || calls[1].Authorization != "Bearer setup-token-b" {
				t.Fatalf("upstream calls = %#v", calls)
			}
		})
	}
}
