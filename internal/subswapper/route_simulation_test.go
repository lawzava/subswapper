package subswapper

import (
	"context"
	"fmt"
	"math/rand/v2"
	"os"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"
)

// Each actor yields between snapshot and completion. Only the scheduler runs
// code, so neither goroutine timing nor map iteration determines event order.
// The collector is synthetic; MonitorOnce's locked merge and decision are real.
func TestRouteInterleavingSimulation(t *testing.T) {
	seeds := []uint64{1, 2, 7, 42}
	defaultSeedCount := len(seeds)
	defaultSeeds := os.Getenv("SUBSWAPPER_SIM_SEED") == ""
	if value := os.Getenv("SUBSWAPPER_SIM_SEED"); value != "" {
		seed, err := strconv.ParseUint(value, 0, 64)
		if err != nil {
			t.Fatal(err)
		}
		seeds = []uint64{seed}
	}
	if value := os.Getenv("SUBSWAPPER_SIM_SEEDS"); value != "" {
		count, err := strconv.Atoi(value)
		if err != nil || count < 0 {
			t.Fatalf("invalid SUBSWAPPER_SIM_SEEDS %q", value)
		}
		for seed := range count {
			seeds = append(seeds, uint64(seed)+1000)
		}
	}
	var coverage [2]routeSimCoverage
	defaultSeedsRun := 0
	for index, seed := range seeds {
		var counts [2]routeSimCoverage
		ran := false
		t.Run(strconv.FormatUint(seed, 10), func(t *testing.T) {
			ran = true
			runRouteSimulation(t, seed, &counts)
		})
		if index < defaultSeedCount {
			if ran {
				defaultSeedsRun++
			}
			for service := range coverage {
				coverage[service].FencedFallbacks += counts[service].FencedFallbacks
				coverage[service].FallbackTakeovers += counts[service].FallbackTakeovers
				coverage[service].StaleSuccesses += counts[service].StaleSuccesses
				coverage[service].StaleIdentities += counts[service].StaleIdentities
				coverage[service].Recoveries += counts[service].Recoveries
			}
		}
	}
	t.Logf("coverage claude=%+v codex=%+v", coverage[0], coverage[1])
	// A -run filter that skips a default seed, or a failed seed, leaves the
	// totals partial; the coverage check would then report a false gap.
	if defaultSeeds && defaultSeedsRun == defaultSeedCount && !t.Failed() {
		for service, counts := range coverage {
			if counts.FencedFallbacks == 0 || counts.FallbackTakeovers == 0 || counts.StaleSuccesses == 0 ||
				counts.Recoveries == 0 || counts.StaleIdentities == 0 {
				t.Errorf("default seeds missed required paths for %s: %+v", []string{"claude", "codex"}[service], counts)
			}
		}
	}
}

type routeSimCoverage struct {
	FencedFallbacks   int
	FallbackTakeovers int
	StaleSuccesses    int
	StaleIdentities   int
	Recoveries        int
}

type routeSimRequest struct {
	service    ServiceConfig
	claude     claudeProxyRoute
	codex      codexProxyRoute
	account    string
	active     string
	switchedAt time.Time
	addedAt    time.Time
	startedAt  time.Time
	outcome    int // success, quota, 401
}

func runRouteSimulation(t *testing.T, seed uint64, coverage *[2]routeSimCoverage) {
	t.Helper()
	rng := rand.New(rand.NewPCG(seed, seed^0x9e3779b97f4a7c15))
	// Future resets keep LimitWindow.Ratio's wall-clock expiry out of the schedule.
	now := time.Date(2100, 1, 1, 0, 0, 0, 0, time.UTC)
	oldClaude, oldCodex, oldRoute, oldCollector := claudeProxyNow, codexProxyNow, routeStateNow, monitorUsageSnapshot
	claudeProxyNow = func() time.Time { return now }
	codexProxyNow = func() time.Time { return now }
	routeStateNow = func() time.Time { return now }
	t.Cleanup(func() {
		claudeProxyNow, codexProxyNow, routeStateNow, monitorUsageSnapshot = oldClaude, oldCodex, oldRoute, oldCollector
	})
	cfg := setupTokenTestConfig(t.TempDir())
	cfg.Services = append(cfg.Services, ServiceConfig{Name: "codex", Kind: "codex", AccountMode: AccountModeHome})
	cfg.ApplyDefaults()
	for _, name := range []string{"a", "b", "c"} {
		registerSetupTokenTestAccount(t, cfg, name)
		if _, err := ReplaceClaudeSetupToken(context.Background(), cfg, "claude", name, "synthetic-"+name, nil); err != nil {
			t.Fatal(err)
		}
		writeCodexTestLogin(t, cfg, name, "synthetic-"+name, "synthetic-id-"+name)
	}
	state := routeTestState(t, cfg)
	for _, service := range cfg.Services {
		ss := state.Service(service.Name)
		for _, name := range []string{"a", "b", "c"} {
			account := ss.Accounts[name]
			account.Name, account.AddedAt = name, now
			account.Usage = routeSimUsage(now, 10, account.SetupTokenRevision)
			ss.Accounts[name] = account
		}
		selectAccount(state, service, "a", now)
	}
	routeTestSave(t, cfg, state)

	var events []string
	var violations []string
	violation := func(format string, args ...any) {
		violations = append(violations, fmt.Sprintf("event=%d %s", len(events)-1, fmt.Sprintf(format, args...)))
	}
	fail := func(format string, args ...any) {
		t.Helper()
		t.Fatalf("seed=%d replay: SUBSWAPPER_SIM_SEED=%d\n%s\n%s",
			seed, seed, strings.Join(events, "\n"), fmt.Sprintf(format, args...))
	}
	log := func(format string, args ...any) {
		events = append(events, fmt.Sprintf("%02d t=+%ds %s", len(events), now.Sub(time.Date(2100, 1, 1, 0, 0, 0, 0, time.UTC))/time.Second, fmt.Sprintf(format, args...)))
	}
	// Six proxy actors, one manual actor, one split monitor, one
	// remove/re-add actor. Pending requests are drained in seed-chosen order.
	var pending [6]*routeSimRequest
	var probe *State
	var results []ServiceStatus
	var removed string
	var removedService ServiceConfig
	for step := 0; step < 96 || probe != nil || routeSimPending(pending) || removed != ""; step++ {
		actors := []int{0, 1, 2, 3, 4, 5, 6, 7, 8}
		if step >= 96 {
			actors = nil
			for actor, request := range pending {
				if request != nil {
					actors = append(actors, actor)
				}
			}
			if probe != nil {
				actors = append(actors, 7)
			}
			if removed != "" {
				actors = append(actors, 8)
			}
		}
		now = now.Add(time.Duration(1+rng.IntN(5)) * time.Second)
		if step > 0 && step%24 == 0 {
			// Let rejected routes become eligible for a fresh recovery request.
			now = now.Add(credentialsErrorBackoff + time.Second)
		}
		actor := actors[rng.IntN(len(actors))]
		before := routeTestState(t, cfg)
		switch {
		case actor < 6:
			service := cfg.Services[actor/3]
			counts := &coverage[actor/3]
			if pending[actor] == nil {
				ss := before.Service(service.Name)
				request := &routeSimRequest{service: service, active: ss.ActiveAccount, switchedAt: ss.LastSwitchedAt, startedAt: now, outcome: rng.IntN(3)}
				if service.Name == "claude" {
					routes, err := claudeProxyRoutes(cfg, service, "")
					if err != nil {
						fail("dispatch: %v", err)
					}
					if len(routes) == 0 {
						log("actor=%d claude dispatch no usable routes", actor)
						break
					}
					request.claude = routes[rng.IntN(len(routes))]
					request.account = request.claude.Account
				} else {
					routes, err := codexProxyRoutes(cfg, service)
					if err != nil {
						fail("dispatch: %v", err)
					}
					if len(routes) == 0 {
						log("actor=%d codex dispatch no usable routes", actor)
						break
					}
					request.codex = routes[rng.IntN(len(routes))]
					request.account = request.codex.Account
				}
				request.addedAt = ss.Accounts[request.account].AddedAt
				pending[actor] = request
				log("actor=%d %s dispatch %s active=%s switched=%s outcome=%s", actor, service.Name, request.account, request.active, request.switchedAt.Format(time.RFC3339), []string{"success", "quota", "401"}[request.outcome])
			} else {
				request := pending[actor]
				pending[actor] = nil
				log("actor=%d %s complete %s outcome=%s dispatch=%s", actor, service.Name, request.account, []string{"success", "quota", "401"}[request.outcome], request.startedAt.Format(time.RFC3339))
				pct := 10.0
				if request.outcome == 1 {
					pct = 100
				}
				usage := routeSimUsage(request.startedAt, pct, request.claude.Revision)
				var err error
				switchTo := request.outcome == 0 && request.account != request.active
				if service.Name == "claude" {
					// Claude headers are stamped when received, unlike a Codex fetch.
					usage.ObservedAt = now
					err = recordClaudeProxyObservation(cfg, service, request.claude, claudeRateLimitObservation{HasUsage: request.outcome != 2, Usage: usage, Rejected: request.outcome == 1}, request.outcome == 2, switchTo)
				} else {
					var sample *UsageSnapshot
					if request.outcome != 2 {
						sample = &usage
					}
					err = recordCodexProxyState(context.Background(), cfg, service, request.codex, sample, request.outcome == 2, switchTo, false)
				}
				after := routeTestState(t, cfg)
				oldSS, newSS := before.Service(service.Name), after.Service(service.Name)
				oldAccount, newAccount := oldSS.Accounts[request.account], newSS.Accounts[request.account]
				if !oldAccount.AddedAt.Equal(request.addedAt) {
					if err == nil || !reflect.DeepEqual(newAccount, oldAccount) {
						violation("previous account identity accepted for %s/%s", service.Name, request.account)
					} else {
						counts.StaleIdentities++
					}
				} else if err != nil {
					fail("record: %v", err)
				}
				if oldSS.ActiveAccount != request.active || !oldSS.LastSwitchedAt.Equal(request.switchedAt) {
					if newSS.ActiveAccount != oldSS.ActiveAccount || !newSS.LastSwitchedAt.Equal(oldSS.LastSwitchedAt) {
						violation("stale completion undid newer switch: %s active=%s -> %s", service.Name, oldSS.ActiveAccount, newSS.ActiveAccount)
					} else if switchTo && oldSS.ActiveAccount != request.account && oldAccount.AddedAt.Equal(request.addedAt) {
						counts.FencedFallbacks++
					}
				}
				if oldAccount.AddedAt.Equal(request.addedAt) && switchTo &&
					oldSS.ActiveAccount == request.active && oldSS.LastSwitchedAt.Equal(request.switchedAt) {
					if newSS.ActiveAccount != request.account || !newSS.LastSwitchedAt.Equal(now) {
						violation("unchanged selection did not fail over: %s -> %s", service.Name, request.account)
					} else {
						counts.FallbackTakeovers++
					}
				}
				if request.outcome != 0 && (newSS.ActiveAccount != oldSS.ActiveAccount || !newSS.LastSwitchedAt.Equal(oldSS.LastSwitchedAt)) {
					violation("failed request changed selection: %s/%s", service.Name, request.account)
				}
				if request.outcome != 2 && oldAccount.CredentialsError != "" && request.startedAt.Before(oldAccount.LastProbeStartedAt) {
					if newAccount.CredentialsError != oldAccount.CredentialsError || !newAccount.FetchBackoffUntil.Equal(oldAccount.FetchBackoffUntil) {
						violation("older success cleared newer rejection: %s/%s", service.Name, request.account)
					} else if request.outcome == 0 && oldAccount.AddedAt.Equal(request.addedAt) {
						counts.StaleSuccesses++
					}
				}
				if request.outcome == 0 && oldAccount.AddedAt.Equal(request.addedAt) && oldAccount.CredentialsError != "" &&
					!request.startedAt.Before(oldAccount.LastProbeStartedAt) {
					if newAccount.CredentialsError != "" || !newAccount.FetchBackoffUntil.IsZero() {
						violation("fresh success did not clear rejection: %s/%s", service.Name, request.account)
					} else {
						counts.Recoveries++
					}
				}
			}
		case actor == 6:
			service := cfg.Services[rng.IntN(2)]
			names := routeSimAccountNames(before.Service(service.Name))
			target := names[rng.IntN(len(names))]
			log("manual %s -> %s", service.Name, target)
			if err := SwitchAccount(cfg, service.Name, target); err != nil {
				fail("manual: %v", err)
			}
		case actor == 7:
			if probe == nil {
				probe = before
				results = nil
				var samples []string
				for _, service := range cfg.Services {
					ss := probe.Service(service.Name)
					result := ServiceStatus{Service: service}
					for _, name := range []string{"a", "b", "c"} {
						account, ok := ss.Accounts[name]
						if !ok {
							continue
						}
						account.LastProbeStartedAt = now
						account.Usage = routeSimUsage(now, float64(rng.IntN(2)*100), account.SetupTokenRevision)
						account.CredentialsError, account.FetchBackoffUntil = "", time.Time{}
						ss.Accounts[name] = account
						samples = append(samples, fmt.Sprintf("%s/%s=%.0f%%", service.Name, name, *account.Usage.Weekly.Pct))
						result.Accounts = append(result.Accounts, AccountStatus{Service: service.Name, Account: account, Active: ss.ActiveAccount == name, Selectable: !account.Usage.Exhausted(), Score: account.Usage.Score()})
					}
					results = append(results, result)
				}
				log("monitor snapshot %s", strings.Join(samples, " "))
			} else {
				log("monitor merge/decide")
				monitorUsageSnapshot = func(context.Context, Config) (*State, []ServiceStatus, error) { return probe, results, nil }
				cycle := MonitorOnce(context.Background(), cfg, true)
				events[len(events)-1] += fmt.Sprintf(" switches=%v errors=%v", cycle.Switches, cycle.Errors)
				for _, err := range cycle.Errors {
					if !strings.Contains(err.Error(), "changed during usage probe") && !strings.Contains(err.Error(), "no selectable accounts") {
						fail("monitor: %v", err)
					}
				}
				after := routeTestState(t, cfg)
				for _, service := range cfg.Services {
					oldSS, snapshotSS, gotSS := before.Service(service.Name), probe.Service(service.Name), after.Service(service.Name)
					if (oldSS.ActiveAccount != snapshotSS.ActiveAccount || !oldSS.LastSwitchedAt.Equal(snapshotSS.LastSwitchedAt)) &&
						(gotSS.ActiveAccount != oldSS.ActiveAccount || !gotSS.LastSwitchedAt.Equal(oldSS.LastSwitchedAt)) {
						violation("stale monitor undid newer switch: %s", service.Name)
					}
					for _, name := range []string{"a", "b", "c"} {
						old := before.Service(service.Name).Accounts[name]
						got := after.Service(service.Name).Accounts[name]
						snapshot := probe.Service(service.Name).Accounts[name]
						if old.CredentialsError != "" && old.LastProbeStartedAt.After(snapshot.LastProbeStartedAt) && got.CredentialsError == "" {
							violation("older monitor cleared newer rejection: %s/%s", service.Name, name)
						}
					}
				}
				probe = nil
			}
		default:
			if removed == "" {
				removedService = cfg.Services[rng.IntN(2)]
				names := routeSimAccountNames(before.Service(removedService.Name))
				removed = names[rng.IntN(len(names))]
				log("%s force-remove %s", removedService.Name, removed)
				if removedService.Name == "claude" {
					if err := RemoveClaudeSetupToken(context.Background(), cfg, "claude", removed); err != nil {
						fail("remove setup token: %v", err)
					}
				}
				if err := RemoveAccountWithOptions(cfg, removedService.Name, removed, true, false); err != nil {
					fail("remove: %v", err)
				}
				if before.Service(removedService.Name).ActiveAccount == removed && routeTestState(t, cfg).Service(removedService.Name).ActiveAccount != "" {
					violation("force-removing the active account did not clear selection")
				}
			} else {
				log("%s re-add %s", removedService.Name, removed)
				state := routeTestState(t, cfg)
				state.Service(removedService.Name).Accounts[removed] = AccountState{Name: removed, AddedAt: now}
				routeTestSave(t, cfg, state)
				if removedService.Name == "claude" {
					if _, err := ReplaceClaudeSetupToken(context.Background(), cfg, "claude", removed, fmt.Sprintf("synthetic-%s-%d", removed, step), nil); err != nil {
						fail("re-add setup token: %v", err)
					}
				}
				removed = ""
			}
		}
		after := routeTestState(t, cfg)
		for _, service := range cfg.Services {
			old, got := before.Service(service.Name), after.Service(service.Name)
			// Force removal deliberately leaves no selection until another writer selects.
			if _, ok := got.Accounts[got.ActiveAccount]; got.ActiveAccount != "" && !ok {
				violation("active account missing: %s/%s", service.Name, got.ActiveAccount)
			}
			if got.LastSwitchedAt.Before(old.LastSwitchedAt) {
				violation("LastSwitchedAt went backwards: %s", service.Name)
			}
		}
	}
	if len(violations) != 0 {
		fail("%s", strings.Join(violations, "\n"))
	}
}

func routeSimAccountNames(service *ServiceState) []string {
	var names []string
	for _, name := range []string{"a", "b", "c"} {
		if _, ok := service.Accounts[name]; ok {
			names = append(names, name)
		}
	}
	return names
}

func routeSimUsage(now time.Time, pct float64, revision string) UsageSnapshot {
	return UsageSnapshot{
		FiveHour:   LimitWindow{Pct: PtrFloat64(pct), ResetsAt: now.Add(5 * time.Hour)},
		Weekly:     LimitWindow{Pct: PtrFloat64(pct), ResetsAt: now.Add(7 * 24 * time.Hour)},
		ObservedAt: now, Source: claudeUsageSourceProxy, TokenRevision: revision,
	}
}

func routeSimPending(pending [6]*routeSimRequest) bool {
	for _, request := range pending {
		if request != nil {
			return true
		}
	}
	return false
}
