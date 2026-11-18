package subswapper

import (
	"context"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"
)

// All account homes and tokens in this fixture are synthetic and local to t.TempDir.
func routeTestConfig(t *testing.T, kind string) Config {
	t.Helper()
	var cfg Config
	if kind == "claude" {
		cfg, _ = setupProxyAccounts(t, "http://127.0.0.1:1")
		registerSetupTokenTestAccount(t, cfg, "c")
		if _, err := ReplaceClaudeSetupToken(context.Background(), cfg, kind, "c", "setup-token-c", nil); err != nil {
			t.Fatal(err)
		}
	} else {
		cfg, _ = setupCodexProxyAccounts(t, "http://127.0.0.1:1")
		writeCodexTestLogin(t, cfg, "c", "chatgpt-token-c", "acct-c")
		state := routeTestState(t, cfg)
		state.Service(kind).Accounts["c"] = AccountState{Name: "c", AddedAt: time.Now().UTC()}
		routeTestSave(t, cfg, state)
	}
	return cfg
}

func routeTestState(t *testing.T, cfg Config) *State {
	t.Helper()
	state, err := LoadState(cfg.StatePath)
	if err != nil {
		t.Fatal(err)
	}
	return state
}

func routeTestSave(t *testing.T, cfg Config, state *State) {
	t.Helper()
	if err := SaveState(cfg.StatePath, state); err != nil {
		t.Fatal(err)
	}
}

func TestClaudeFallbackKeepsNewerSwitch(t *testing.T) {
	cfg := routeTestConfig(t, "claude")
	routes, err := claudeProxyRoutes(cfg, cfg.Services[0], "")
	if err != nil {
		t.Fatal(err)
	}
	for _, route := range routes {
		if route.Account != "b" {
			continue
		}
		if err := SwitchAccount(cfg, "claude", "c"); err != nil {
			t.Fatal(err)
		}
		if err := recordClaudeProxyObservation(cfg, cfg.Services[0], route, claudeRateLimitObservation{}, false, true); err != nil {
			t.Fatal(err)
		}
		if got := routeTestState(t, cfg).Service("claude").ActiveAccount; got != "c" {
			t.Fatalf("stale fallback completion undid newer switch: active = %q, want c", got)
		}
		return
	}
	t.Fatal("no fallback b")
}

func TestCodexFallbackKeepsNewerSwitch(t *testing.T) {
	cfg := routeTestConfig(t, "codex")
	routes, err := codexProxyRoutes(cfg, cfg.Services[0])
	if err != nil {
		t.Fatal(err)
	}
	for _, route := range routes {
		if route.Account != "b" {
			continue
		}
		if err := SwitchAccount(cfg, "codex", "c"); err != nil {
			t.Fatal(err)
		}
		if err := recordCodexProxyObservation(cfg, cfg.Services[0], route, nil, false, true); err != nil {
			t.Fatal(err)
		}
		if got := routeTestState(t, cfg).Service("codex").ActiveAccount; got != "c" {
			t.Fatalf("stale fallback completion undid newer switch: active = %q, want c", got)
		}
		return
	}
	t.Fatal("no fallback b")
}

func TestCodexProxyRejectsReaddedAccountObservation(t *testing.T) {
	for _, unauthorized := range []bool{false, true} {
		t.Run(map[bool]string{false: "usage", true: "401"}[unauthorized], func(t *testing.T) {
			cfg := routeTestConfig(t, "codex")
			routes, err := codexProxyRoutes(cfg, cfg.Services[0])
			if err != nil {
				t.Fatal(err)
			}
			route := routes[0]
			state := routeTestState(t, cfg)
			previous := state.Service("codex").Accounts[route.Account]
			replacement := AccountState{Name: route.Account, AddedAt: previous.AddedAt.Add(time.Second)}
			state.Service("codex").Accounts[route.Account] = replacement
			routeTestSave(t, cfg, state)
			usage := UsageSnapshot{ObservedAt: time.Now().UTC(), Source: claudeUsageSourceProxy}
			err = recordCodexProxyObservation(cfg, cfg.Services[0], route, &usage, unauthorized, false)
			if err == nil {
				t.Error("accepted an observation for the previous account identity")
			}
			got := routeTestState(t, cfg).Service("codex").Accounts[route.Account]
			if got.ProxyUsage != replacement.ProxyUsage || got.CredentialsError != "" || !got.FetchBackoffUntil.IsZero() {
				t.Fatalf("stale observation modified replacement: usage source=%q credentials error=%q backoff=%s",
					got.ProxyUsage.Source, got.CredentialsError, got.FetchBackoffUntil)
			}
		})
	}
}

func TestClaudeProxyOlderSuccessKeepsNewerTokenRejection(t *testing.T) {
	cfg := routeTestConfig(t, "claude")
	now := time.Now().UTC()
	oldClock := claudeProxyNow
	claudeProxyNow = func() time.Time { return now }
	t.Cleanup(func() { claudeProxyNow = oldClock })
	routes, err := claudeProxyRoutes(cfg, cfg.Services[0], "")
	if err != nil {
		t.Fatal(err)
	}
	route := routes[0]
	now = now.Add(time.Second)
	if err := recordClaudeProxyObservation(cfg, cfg.Services[0], route, claudeRateLimitObservation{}, true, false); err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Second)
	// Headers arrive after the rejection, but belong to a request already in flight.
	observation := claudeRateLimitObservation{HasUsage: true, Usage: UsageSnapshot{ObservedAt: now}}
	if err := recordClaudeProxyObservation(cfg, cfg.Services[0], route, observation, false, false); err != nil {
		t.Fatal(err)
	}
	account := routeTestState(t, cfg).Service("claude").Accounts[route.Account]
	if account.CredentialsError == "" || account.FetchBackoffUntil.IsZero() {
		t.Fatal("an older request's success cleared the newer token rejection")
	}
	// A request dispatched after the rejection can prove recovery.
	// Backoff excludes the route until its retry time.
	now = account.FetchBackoffUntil.Add(time.Second)
	observation.Usage.ObservedAt = now
	routes, err = claudeProxyRoutes(cfg, cfg.Services[0], "")
	if err != nil {
		t.Fatal(err)
	}
	for _, fresh := range routes {
		if fresh.Account == route.Account {
			if err := recordClaudeProxyObservation(cfg, cfg.Services[0], fresh, observation, false, false); err != nil {
				t.Fatal(err)
			}
			account = routeTestState(t, cfg).Service("claude").Accounts[route.Account]
			if account.CredentialsError != "" || !account.FetchBackoffUntil.IsZero() {
				t.Fatal("a newer request's success did not clear the token rejection")
			}
			return
		}
	}
	t.Fatal("no recovered route")
}

// routeTestClock pins both proxy clocks to a time the test advances.
func routeTestClock(t *testing.T) *time.Time {
	t.Helper()
	now := time.Now().UTC()
	oldClaude, oldCodex := claudeProxyNow, codexProxyNow
	claudeProxyNow = func() time.Time { return now }
	codexProxyNow = func() time.Time { return now }
	t.Cleanup(func() { claudeProxyNow, codexProxyNow = oldClaude, oldCodex })
	return &now
}

// routeTestRequest dispatches one request to account from a fresh route
// snapshot. The returned function records its outcome: "success" (taking
// over the route when account was a fallback), "401" or "quota".
func routeTestRequest(t *testing.T, cfg Config, kind, account string) func(outcome string) error {
	t.Helper()
	return routeTestModelRequest(t, cfg, kind, account, "")
}

const routeTestFableModel = "claude-fable-5-2"

// routeTestOneWindowFull is a sample with only the five-hour ("five_quota")
// or only the weekly ("weekly_quota") window full.
func routeTestOneWindowFull(now time.Time, revision, outcome string) UsageSnapshot {
	usage := routeSimUsage(now, 10, revision)
	if outcome == "five_quota" {
		usage.FiveHour.Pct = PtrFloat64(100)
	} else {
		usage.Weekly.Pct = PtrFloat64(100)
	}
	return usage
}

// routeTestModelRequest is routeTestRequest for a Claude request to model.
// Its "fable_quota" outcome fills only the Fable window, and a success
// for a Fable model reports that window below its limit.
func routeTestModelRequest(t *testing.T, cfg Config, kind, account, model string) func(outcome string) error {
	t.Helper()
	service := cfg.Services[0]
	if kind == "claude" {
		routes, err := claudeProxyRoutes(cfg, service, model)
		if err != nil {
			t.Fatal(err)
		}
		for _, route := range routes {
			if route.Account != account {
				continue
			}
			return func(outcome string) error {
				now := claudeProxyNow().UTC()
				fable := func(pct float64) LimitWindow {
					return LimitWindow{Pct: PtrFloat64(pct), ResetsAt: now.Add(48 * time.Hour)}
				}
				switch outcome {
				case "401":
					return recordClaudeProxyObservation(cfg, service, route, claudeRateLimitObservation{}, true, false)
				case "five_quota", "weekly_quota":
					observation := claudeRateLimitObservation{HasUsage: true, Rejected: true, Usage: routeTestOneWindowFull(now, route.Revision, outcome)}
					return recordClaudeProxyObservation(cfg, service, route, observation, false, false)
				case "quota":
					observation := claudeRateLimitObservation{HasUsage: true, Rejected: true, Usage: routeSimUsage(now, 100, route.Revision)}
					return recordClaudeProxyObservation(cfg, service, route, observation, false, false)
				case "fable_quota":
					observation := claudeRateLimitObservation{HasUsage: true, Rejected: true, Usage: routeSimUsage(now, 10, route.Revision)}
					observation.Usage.FableWeekly = fable(100)
					return recordClaudeProxyObservation(cfg, service, route, observation, false, false)
				}
				observation := claudeRateLimitObservation{HasUsage: true, Usage: routeSimUsage(now, 10, route.Revision)}
				if strings.Contains(model, "fable") {
					observation.Usage.FableWeekly = fable(10)
				}
				return recordClaudeProxyObservation(cfg, service, route, observation, false, !route.Active)
			}
		}
	} else {
		routes, err := codexProxyRoutes(cfg, service)
		if err != nil {
			t.Fatal(err)
		}
		for _, route := range routes {
			if route.Account != account {
				continue
			}
			return func(outcome string) error {
				switch outcome {
				case "401":
					return recordCodexProxyObservation(cfg, service, route, nil, true, false)
				case "quota":
					usage := routeSimUsage(codexProxyNow().UTC(), 100, "")
					return recordCodexProxyObservation(cfg, service, route, &usage, false, false)
				case "five_quota", "weekly_quota":
					// The usage fetch after the 429 starts now.
					usage := routeTestOneWindowFull(codexProxyNow().UTC(), "", outcome)
					return recordCodexProxyObservation(cfg, service, route, &usage, false, false)
				}
				return recordCodexProxyObservation(cfg, service, route, nil, false, !route.Active)
			}
		}
	}
	t.Fatalf("no %s route for %s", kind, account)
	return nil
}

func TestProxyStaleSuccessKeepsNewerRejection(t *testing.T) {
	for _, kind := range []string{"claude", "codex"} {
		for _, rejection := range []string{"401", "quota"} {
			t.Run(kind+"/"+rejection, func(t *testing.T) {
				cfg := routeTestConfig(t, kind)
				now := routeTestClock(t)
				stale := routeTestRequest(t, cfg, kind, "b")
				*now = now.Add(time.Second)
				if err := routeTestRequest(t, cfg, kind, "b")(rejection); err != nil {
					t.Fatal(err)
				}
				before := routeTestState(t, cfg).Service(kind).LastSwitchedAt
				*now = now.Add(time.Second)
				// The fallback was dispatched before the rejection and succeeds after it.
				if err := stale("success"); err != nil {
					t.Fatal(err)
				}
				got := routeTestState(t, cfg).Service(kind)
				if got.ActiveAccount != "a" || !got.LastSwitchedAt.Equal(before) {
					t.Fatalf("stale success selected an account with a newer %s rejection: active=%s", rejection, got.ActiveAccount)
				}
			})
		}
		t.Run(kind+"/quota_before_dispatch", func(t *testing.T) {
			cfg := routeTestConfig(t, kind)
			now := routeTestClock(t)
			if err := routeTestRequest(t, cfg, kind, "b")("quota"); err != nil {
				t.Fatal(err)
			}
			*now = now.Add(time.Second)
			fresh := routeTestRequest(t, cfg, kind, "b")
			*now = now.Add(time.Second)
			if err := fresh("success"); err != nil {
				t.Fatal(err)
			}
			if got := routeTestState(t, cfg).Service(kind).ActiveAccount; got != "b" {
				t.Fatalf("a success dispatched after the rejection did not take over: active=%s", got)
			}
		})
	}
}

// Two fallbacks are in flight when the account is rejected. The first stale
// success must not erase the rejection, or the second would take over.
func TestProxyTwoStaleSuccessesKeepNewerRejection(t *testing.T) {
	for _, kind := range []string{"claude", "codex"} {
		for _, rejection := range []string{"401", "quota"} {
			t.Run(kind+"/"+rejection, func(t *testing.T) {
				cfg := routeTestConfig(t, kind)
				now := routeTestClock(t)
				first := routeTestRequest(t, cfg, kind, "b")
				second := routeTestRequest(t, cfg, kind, "b")
				*now = now.Add(time.Second)
				if err := routeTestRequest(t, cfg, kind, "b")(rejection); err != nil {
					t.Fatal(err)
				}
				for _, stale := range []func(string) error{first, second} {
					*now = now.Add(time.Second)
					if err := stale("success"); err != nil {
						t.Fatal(err)
					}
				}
				got := routeTestState(t, cfg).Service(kind)
				account := got.Accounts["b"]
				if got.ActiveAccount != "a" {
					t.Fatalf("second stale success selected b after a newer %s rejection", rejection)
				}
				if rejection == "quota" && !account.ProxyUsage.Exhausted() || rejection == "401" && account.CredentialsError == "" {
					t.Fatalf("a stale success erased the newer %s rejection: %+v", rejection, account)
				}
			})
		}
	}
}

// Without a newer rejection, headers from an older in-flight request are an
// ordinary update and replace the stored sample.
func TestClaudeProxyStaleSuccessUpdatesUsageWithoutNewerRejection(t *testing.T) {
	cfg := routeTestConfig(t, "claude")
	now := routeTestClock(t)
	routes, err := claudeProxyRoutes(cfg, cfg.Services[0], "")
	if err != nil {
		t.Fatal(err)
	}
	stale := routes[0]
	*now = now.Add(time.Second)
	if err := routeTestRequest(t, cfg, "claude", stale.Account)("success"); err != nil {
		t.Fatal(err)
	}
	*now = now.Add(time.Second)
	observation := claudeRateLimitObservation{HasUsage: true, Usage: routeSimUsage(*now, 30, stale.Revision)}
	if err := recordClaudeProxyObservation(cfg, cfg.Services[0], stale, observation, false, false); err != nil {
		t.Fatal(err)
	}
	usage := routeTestState(t, cfg).Service("claude").Accounts[stale.Account].ProxyUsage
	if usage.Weekly.Pct == nil || *usage.Weekly.Pct != 30 || !usage.ObservedAt.Equal(*now) {
		t.Fatalf("older request's headers were dropped without a newer rejection: %+v", usage.Weekly)
	}
}

// A full model window rejects only requests for that model, so it fences a
// stale success for that model and nothing else.
func TestClaudeProxyModelRejectionFencesOnlyThatModel(t *testing.T) {
	for _, staleModel := range []string{routeTestFableModel, "claude-opus-5-5"} {
		t.Run(staleModel, func(t *testing.T) {
			cfg := routeTestConfig(t, "claude")
			now := routeTestClock(t)
			stale := routeTestModelRequest(t, cfg, "claude", "b", staleModel)
			*now = now.Add(time.Second)
			if err := routeTestModelRequest(t, cfg, "claude", "b", routeTestFableModel)("fable_quota"); err != nil {
				t.Fatal(err)
			}
			*now = now.Add(time.Second)
			if err := stale("success"); err != nil {
				t.Fatal(err)
			}
			got := routeTestState(t, cfg).Service("claude")
			if usage := got.Accounts["b"].ProxyUsage; !usage.ModelExhausted(routeTestFableModel) || usage.Exhausted() {
				t.Fatalf("stale %s success changed the newer Fable rejection: fable=%v core=%v", staleModel, usage.ModelExhausted(routeTestFableModel), usage.Exhausted())
			}
			want := "a"
			if staleModel != routeTestFableModel {
				// A full Fable window does not reject other models.
				want = "b"
			}
			if got.ActiveAccount != want {
				t.Fatalf("stale %s success after a newer Fable rejection: active=%s, want %s", staleModel, got.ActiveAccount, want)
			}
		})
	}
}

// Two 429s name different binding windows. The later-recorded one comes from
// a request dispatched first, yet its full window still binds and must be
// kept beside the newer five-hour rejection.
func TestClaudeProxyStaleRejectionAddsItsFullWindow(t *testing.T) {
	cfg := routeTestConfig(t, "claude")
	now := routeTestClock(t)
	routes, err := claudeProxyRoutes(cfg, cfg.Services[0], "")
	if err != nil {
		t.Fatal(err)
	}
	var stale claudeProxyRoute
	for _, route := range routes {
		if route.Account == "b" {
			stale = route
		}
	}
	fiveReset, weekReset := now.Add(2*time.Hour), now.Add(5*24*time.Hour)
	window := func(pct float64, reset time.Time) LimitWindow {
		return LimitWindow{Pct: PtrFloat64(pct), ResetsAt: reset}
	}
	reject := func(route claudeProxyRoute, fiveHour, weekly LimitWindow) {
		t.Helper()
		usage := UsageSnapshot{FiveHour: fiveHour, Weekly: weekly, ObservedAt: *now, Source: claudeUsageSourceProxy}
		if err := recordClaudeProxyObservation(cfg, cfg.Services[0], route, claudeRateLimitObservation{HasUsage: true, Rejected: true, Usage: usage}, false, false); err != nil {
			t.Fatal(err)
		}
	}
	*now = now.Add(time.Second)
	fresh, err := claudeProxyRoutes(cfg, cfg.Services[0], "")
	if err != nil {
		t.Fatal(err)
	}
	for _, route := range fresh {
		if route.Account == "b" {
			// claim five_hour
			reject(route, window(100, fiveReset), window(30, weekReset))
		}
	}
	*now = now.Add(time.Second)
	// claim seven_day, from the request dispatched first
	reject(stale, window(40, fiveReset), window(100, weekReset))
	got := routeTestState(t, cfg).Service("claude")
	usage := got.Accounts["b"].ProxyUsage
	if ratio, _ := usage.FiveHour.Ratio(); ratio < 1 {
		t.Errorf("the stale rejection lowered the newer five-hour rejection: %v", ratio)
	}
	if ratio, _ := usage.Weekly.Ratio(); ratio < 1 || !usage.Weekly.ResetsAt.Equal(weekReset) {
		t.Errorf("the stale rejection's binding weekly window was dropped: %v reset=%s", ratio, usage.Weekly.ResetsAt)
	}
	if got.ActiveAccount != "a" {
		t.Errorf("a rejection changed the selection: active=%s", got.ActiveAccount)
	}
	// A success dispatched between the two rejections is older than the
	// weekly one and must not erase it.
	*now = now.Add(time.Second)
	between := stale
	between.DispatchedAt = now.Add(-1500 * time.Millisecond)
	observation := claudeRateLimitObservation{HasUsage: true, Usage: UsageSnapshot{FiveHour: window(10, fiveReset), Weekly: window(10, weekReset), ObservedAt: *now}}
	if err := recordClaudeProxyObservation(cfg, cfg.Services[0], between, observation, false, true); err != nil {
		t.Fatal(err)
	}
	got = routeTestState(t, cfg).Service("claude")
	if !got.Accounts["b"].ProxyUsage.Exhausted() || got.ActiveAccount != "a" {
		t.Fatalf("a success dispatched before the weekly rejection cleared it: active=%s", got.ActiveAccount)
	}
}

// A has the route throughout. Two requests to B dispatched at T0 record a
// weekly rejection at T1 and a repeated (or other-window) rejection at T3.
// A success to B dispatched before both, between them or after both then
// completes. Only one dispatched after both may take over.
func TestProxyRepeatedRejectionAdvancesItsTime(t *testing.T) {
	for _, kind := range []string{"claude", "codex"} {
		for _, repeat := range []string{"weekly_quota", "five_quota"} {
			for _, dispatch := range []string{"before_both", "between", "after_both"} {
				t.Run(kind+"/"+repeat+"/"+dispatch, func(t *testing.T) {
					cfg := routeTestConfig(t, kind)
					now := routeTestClock(t)
					first := routeTestRequest(t, cfg, kind, "b")
					second := routeTestRequest(t, cfg, kind, "b")
					var success func(string) error
					// step dispatches the success when its slot comes, then
					// advances the clock, so every event has its own time.
					step := func(at string) {
						t.Helper()
						if dispatch == at {
							success = routeTestRequest(t, cfg, kind, "b")
						}
						*now = now.Add(time.Second)
					}
					step("before_both") // T0
					if err := first("weekly_quota"); err != nil {
						t.Fatal(err)
					}
					*now = now.Add(time.Second)
					step("between") // T2
					if err := second(repeat); err != nil {
						t.Fatal(err)
					}
					*now = now.Add(time.Second)
					step("after_both") // T4
					if err := success("success"); err != nil {
						t.Fatal(err)
					}
					got := routeTestState(t, cfg).Service(kind)
					if dispatch == "after_both" {
						if got.ActiveAccount != "b" {
							t.Fatalf("a success dispatched after both rejections did not take over: active=%s", got.ActiveAccount)
						}
						return
					}
					if got.ActiveAccount != "a" || !got.Accounts["b"].ProxyUsage.Exhausted() {
						t.Fatalf("a success dispatched %s cleared the T3 rejection: active=%s exhausted=%v",
							dispatch, got.ActiveAccount, got.Accounts["b"].ProxyUsage.Exhausted())
					}
				})
			}
		}
	}
}

// The repeated-rejection schedule for a model window: the second Fable 429
// dates the window, so a Fable success dispatched between them stays fenced.
func TestClaudeProxyRepeatedModelRejectionAdvancesItsTime(t *testing.T) {
	cfg := routeTestConfig(t, "claude")
	now := routeTestClock(t)
	first := routeTestModelRequest(t, cfg, "claude", "b", routeTestFableModel)
	second := routeTestModelRequest(t, cfg, "claude", "b", routeTestFableModel)
	*now = now.Add(time.Second)
	if err := first("fable_quota"); err != nil {
		t.Fatal(err)
	}
	*now = now.Add(time.Second)
	between := routeTestModelRequest(t, cfg, "claude", "b", routeTestFableModel)
	*now = now.Add(time.Second)
	if err := second("fable_quota"); err != nil {
		t.Fatal(err)
	}
	*now = now.Add(time.Second)
	if err := between("success"); err != nil {
		t.Fatal(err)
	}
	got := routeTestState(t, cfg).Service("claude")
	if got.ActiveAccount != "a" || !got.Accounts["b"].ProxyUsage.ModelExhausted(routeTestFableModel) {
		t.Fatalf("a Fable success dispatched between two Fable rejections cleared the second: active=%s", got.ActiveAccount)
	}
}

func TestKeepOutlastingFullWindows(t *testing.T) {
	now := time.Now().UTC()
	full := func(reset time.Time) LimitWindow { return LimitWindow{Pct: PtrFloat64(100), ResetsAt: reset} }
	marker := func() UsageSnapshot {
		return UsageSnapshot{FiveHour: full(now.Add(5 * time.Minute)), ObservedAt: now, Source: claudeUsageSourceProxy}
	}
	for _, tc := range []struct {
		name           string
		stored         UsageSnapshot
		fiveHour, week time.Time // expected resets; zero means no window
	}{
		{"weekly exhaustion kept", UsageSnapshot{Weekly: full(now.Add(72 * time.Hour))}, now.Add(5 * time.Minute), now.Add(72 * time.Hour)},
		{"longer five-hour kept", UsageSnapshot{FiveHour: full(now.Add(2 * time.Hour))}, now.Add(2 * time.Hour), time.Time{}},
		{"shorter five-hour yields", UsageSnapshot{FiveHour: full(now.Add(time.Minute))}, now.Add(5 * time.Minute), time.Time{}},
		{"passed window dropped", UsageSnapshot{Weekly: full(now.Add(-time.Minute))}, now.Add(5 * time.Minute), time.Time{}},
		{"partial window dropped", UsageSnapshot{Weekly: LimitWindow{Pct: PtrFloat64(50), ResetsAt: now.Add(72 * time.Hour)}}, now.Add(5 * time.Minute), time.Time{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := marker()
			keepOutlastingFullWindows(&got, tc.stored)
			if !got.FiveHour.ResetsAt.Equal(tc.fiveHour) || !got.Weekly.ResetsAt.Equal(tc.week) || !got.ObservedAt.Equal(now) {
				t.Fatalf("five-hour reset=%s weekly reset=%s observed=%s", got.FiveHour.ResetsAt, got.Weekly.ResetsAt, got.ObservedAt)
			}
		})
	}
}

// An Opus response inherits the full Fable window into a newer sample. That
// must not make a Fable request dispatched after the rejection look stale.
func TestClaudeProxyInheritedModelWindowKeepsItsObservationTime(t *testing.T) {
	cfg := routeTestConfig(t, "claude")
	now := routeTestClock(t)
	*now = now.Add(time.Second)
	if err := routeTestModelRequest(t, cfg, "claude", "b", routeTestFableModel)("fable_quota"); err != nil {
		t.Fatal(err)
	}
	*now = now.Add(time.Second)
	// The Fable window's reset is still ahead, so the route is ranked last
	// but stays usable.
	fresh := routeTestModelRequest(t, cfg, "claude", "b", routeTestFableModel)
	*now = now.Add(time.Second)
	routes, err := claudeProxyRoutes(cfg, cfg.Services[0], "claude-opus-5-5")
	if err != nil {
		t.Fatal(err)
	}
	for _, route := range routes {
		if route.Account == "b" {
			// The Opus response reports no Fable window; it does not switch here.
			observation := claudeRateLimitObservation{HasUsage: true, Usage: routeSimUsage(*now, 10, route.Revision)}
			if err := recordClaudeProxyObservation(cfg, cfg.Services[0], route, observation, false, false); err != nil {
				t.Fatal(err)
			}
		}
	}
	if !routeTestState(t, cfg).Service("claude").Accounts["b"].ProxyUsage.ModelExhausted(routeTestFableModel) {
		t.Fatal("the Opus response dropped the full Fable window")
	}
	*now = now.Add(time.Second)
	if err := fresh("success"); err != nil {
		t.Fatal(err)
	}
	got := routeTestState(t, cfg).Service("claude")
	if got.Accounts["b"].ProxyUsage.ModelExhausted(routeTestFableModel) {
		t.Error("a Fable success dispatched after the rejection did not clear it")
	}
	if got.ActiveAccount != "b" || !got.LastSwitchedAt.Equal(*now) {
		t.Errorf("a Fable success dispatched after the rejection was fenced: active=%s", got.ActiveAccount)
	}
}

// A Codex quota 429 whose usage fetch fails must still mark the account, or
// an older fallback success could select it and routing would not rank it last.
// codexQuotaWithoutUsageUpstream rejects a's token, answers b with a quota
// 429 and fails b's usage fetch.
func codexQuotaWithoutUsageUpstream(t *testing.T) *httptest.Server {
	t.Helper()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Header.Get("Authorization") == "Bearer chatgpt-token-a":
			w.WriteHeader(http.StatusUnauthorized)
		case r.URL.Path == codexProxyUsagePath:
			w.WriteHeader(http.StatusInternalServerError)
		default:
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"error":{"type":"usage_limit_reached","message":"You've hit your usage limit."}}`))
		}
	}))
	t.Cleanup(upstream.Close)
	return upstream
}

// A provisional marker must not hide a weekly exhaustion already known.
func TestCodexProxyQuotaWithoutUsageKeepsKnownExhaustion(t *testing.T) {
	cfg, proxy := setupCodexProxyAccounts(t, codexQuotaWithoutUsageUpstream(t).URL)
	weekReset := time.Now().UTC().Add(6 * 24 * time.Hour).Truncate(time.Second)
	state := routeTestState(t, cfg)
	account := state.Service("codex").Accounts["b"]
	account.ProxyUsage = UsageSnapshot{Weekly: LimitWindow{Pct: PtrFloat64(100), ResetsAt: weekReset},
		ObservedAt: time.Now().UTC().Add(-time.Minute), Source: claudeUsageSourceProxy}
	state.Service("codex").Accounts["b"] = account
	routeTestSave(t, cfg, state)
	stale := routeTestRequest(t, cfg, "codex", "b")
	if recorder := codexProxyRequest(t, proxy, proxy.placeholder.Token, `{}`); recorder.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d", recorder.Code)
	}
	if err := stale("success"); err != nil {
		t.Fatal(err)
	}
	state = routeTestState(t, cfg)
	if active := state.Service("codex").ActiveAccount; active != "a" {
		t.Errorf("an older fallback success selected the quota-rejected account: active=%s", active)
	}
	account = state.Service("codex").Accounts["b"]
	if ratio, _ := account.ProxyUsage.Weekly.Ratio(); ratio < 1 || !account.ProxyUsage.Weekly.ResetsAt.Equal(weekReset) {
		t.Fatalf("the provisional marker dropped the known weekly exhaustion: ratio=%v reset=%s", ratio, account.ProxyUsage.Weekly.ResetsAt)
	}
	// Let the provisional five-minute window pass.
	account.ProxyUsage.FiveHour.ResetsAt = time.Now().UTC().Add(-time.Second)
	state.Service("codex").Accounts["b"] = account
	routeTestSave(t, cfg, state)
	routes, err := codexProxyRoutes(cfg, cfg.Services[0])
	if err != nil {
		t.Fatal(err)
	}
	for _, route := range routes {
		if route.Account == "b" && !route.Exhausted {
			t.Error("b ranks as usable once the provisional window passes, despite the weekly exhaustion")
		}
	}
}

func TestCodexProxyQuotaWithoutUsageFencesOlderFallback(t *testing.T) {
	cfg, proxy := setupCodexProxyAccounts(t, codexQuotaWithoutUsageUpstream(t).URL)
	stale := routeTestRequest(t, cfg, "codex", "b")
	// a's token is rejected, so the request falls over to b, which is out of quota.
	if recorder := codexProxyRequest(t, proxy, proxy.placeholder.Token, `{}`); recorder.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d", recorder.Code)
	}
	if err := stale("success"); err != nil {
		t.Fatal(err)
	}
	if active := routeTestState(t, cfg).Service("codex").ActiveAccount; active != "a" {
		t.Errorf("an older fallback success selected the quota-rejected account: active=%s", active)
	}
	routes, err := codexProxyRoutes(cfg, cfg.Services[0])
	if err != nil {
		t.Fatal(err)
	}
	for _, route := range routes {
		if route.Account == "b" && !route.Exhausted {
			t.Error("routing does not rank the quota-rejected account as exhausted")
		}
	}
}

// routeTestReadd force-removes account and adds it again through the
// production paths. Adding selects it when the service has no selection.
func routeTestReadd(t *testing.T, cfg Config, kind, account string) {
	t.Helper()
	// CreateAccountHome links shared Claude configuration from the native home.
	t.Setenv("HOME", t.TempDir())
	if err := UnregisterAccount(context.Background(), cfg, kind, account, true, false); err != nil {
		t.Fatal(err)
	}
	if _, _, err := CreateAccountHome(cfg, kind, account, ""); err != nil {
		t.Fatal(err)
	}
	if kind == "claude" {
		// Reuse the same token: only the revision can tell the identities apart.
		if _, err := ReplaceClaudeSetupToken(context.Background(), cfg, kind, account, "setup-token-"+account, nil); err != nil {
			t.Fatal(err)
		}
	}
}

func TestProxyFallbackKeepsReaddedSelection(t *testing.T) {
	for _, kind := range []string{"claude", "codex"} {
		t.Run(kind, func(t *testing.T) {
			cfg := routeTestConfig(t, kind)
			stale := routeTestRequest(t, cfg, kind, "b")
			routeTestReadd(t, cfg, kind, "a")
			before := routeTestState(t, cfg).Service(kind)
			if before.ActiveAccount != "a" {
				t.Fatalf("re-adding the removed selection did not select it: active=%q", before.ActiveAccount)
			}
			if err := stale("success"); err != nil {
				t.Fatal(err)
			}
			got := routeTestState(t, cfg).Service(kind)
			if got.ActiveAccount != "a" || !got.LastSwitchedAt.Equal(before.LastSwitchedAt) {
				t.Fatalf("fallback dispatched before a remove and re-add replaced the new selection: active=%s", got.ActiveAccount)
			}
		})
	}
}

func TestClaudeProxyRejectsReaddedAccountObservation(t *testing.T) {
	for _, outcome := range []string{"success", "401", "quota"} {
		t.Run(outcome, func(t *testing.T) {
			cfg := routeTestConfig(t, "claude")
			stale := routeTestRequest(t, cfg, "claude", "a")
			routeTestReadd(t, cfg, "claude", "a")
			before := routeTestState(t, cfg).Service("claude")
			if err := stale(outcome); err == nil {
				t.Error("accepted an observation for the previous account identity")
			}
			got := routeTestState(t, cfg).Service("claude")
			if !reflect.DeepEqual(got.Accounts["a"], before.Accounts["a"]) || got.ActiveAccount != before.ActiveAccount {
				t.Fatalf("stale observation modified the re-added account: %+v", got.Accounts["a"])
			}
		})
	}
}

func TestProxyFallbackSelectionFence(t *testing.T) {
	for _, kind := range []string{"claude", "codex"} {
		for _, changed := range []bool{false, true} {
			t.Run(kind+map[bool]string{false: "/normal_failover", true: "/ABA"}[changed], func(t *testing.T) {
				cfg := routeTestConfig(t, kind)
				service := cfg.Services[0]
				var complete func() error
				if kind == "claude" {
					routes, err := claudeProxyRoutes(cfg, service, "")
					if err != nil {
						t.Fatal(err)
					}
					for _, route := range routes {
						if route.Account == "b" {
							complete = func() error {
								return recordClaudeProxyObservation(cfg, service, route, claudeRateLimitObservation{}, false, true)
							}
						}
					}
				} else {
					routes, err := codexProxyRoutes(cfg, service)
					if err != nil {
						t.Fatal(err)
					}
					for _, route := range routes {
						if route.Account == "b" {
							complete = func() error { return recordCodexProxyObservation(cfg, service, route, nil, false, true) }
						}
					}
				}
				if complete == nil {
					t.Fatal("no fallback b")
				}
				want := "b"
				if changed {
					for _, name := range []string{"c", "a"} {
						if err := SwitchAccount(cfg, kind, name); err != nil {
							t.Fatal(err)
						}
					}
					want = "a"
				}
				before := routeTestState(t, cfg).Service(kind).LastSwitchedAt
				if err := complete(); err != nil {
					t.Fatal(err)
				}
				got := routeTestState(t, cfg).Service(kind)
				if got.ActiveAccount != want || changed && !got.LastSwitchedAt.Equal(before) {
					t.Fatalf("active=%s switched=%s, want %s (preserve timestamp=%v)", got.ActiveAccount, got.LastSwitchedAt, want, changed)
				}
			})
		}
	}
}
