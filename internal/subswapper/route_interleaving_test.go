package subswapper

import (
	"context"
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
