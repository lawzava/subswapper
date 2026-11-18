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
	// 1022 is the only default seed that reaches ReaddFences.
	seeds := []uint64{1, 2, 7, 42, 1022}
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
				coverage[service].RejectionFences += counts[service].RejectionFences
				coverage[service].ReaddFences += counts[service].ReaddFences
				coverage[service].KeptQuotaRejections += counts[service].KeptQuotaRejections
				coverage[service].AddedRejectedWindows += counts[service].AddedRejectedWindows
				coverage[service].RepeatedRejections += counts[service].RepeatedRejections
			}
		}
	}
	t.Logf("coverage claude=%+v codex=%+v", coverage[0], coverage[1])
	// A -run filter that skips a default seed, or a failed seed, leaves the
	// totals partial; the coverage check would then report a false gap.
	if defaultSeeds && defaultSeedsRun == defaultSeedCount && !t.Failed() {
		for service, counts := range coverage {
			if counts.FencedFallbacks == 0 || counts.FallbackTakeovers == 0 || counts.StaleSuccesses == 0 ||
				counts.Recoveries == 0 || counts.StaleIdentities == 0 || counts.RejectionFences == 0 || counts.ReaddFences == 0 ||
				counts.KeptQuotaRejections == 0 || service == 0 && (counts.AddedRejectedWindows == 0 || counts.RepeatedRejections == 0) {
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
	// RejectionFences counts fallback successes stopped only by a rejection
	// of their account recorded after dispatch.
	RejectionFences int
	// ReaddFences counts fallback successes stopped only by a force-remove
	// and re-add of the account selected at dispatch.
	ReaddFences int
	// KeptQuotaRejections counts successes dispatched before a live quota
	// rejection that completed without erasing it.
	KeptQuotaRejections int
	// AddedRejectedWindows counts Claude 429s dispatched before a live quota
	// rejection that named a window not yet full. Codex has no such case.
	AddedRejectedWindows int
	// RepeatedRejections counts Claude 429s dispatched before a live quota
	// rejection that named a window already full. Codex has no such case.
	RepeatedRejections int
}

type routeSimRequest struct {
	service       ServiceConfig
	claude        claudeProxyRoute
	codex         codexProxyRoute
	account       string
	active        string
	activeAddedAt time.Time
	switchedAt    time.Time
	addedAt       time.Time
	startedAt     time.Time
	outcome       int // success, quota, 401
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
	// CreateAccountHome links shared Claude configuration from the native home.
	t.Setenv("HOME", t.TempDir())
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
	var probed []string
	var probeAt time.Time
	var removed string
	var removedService ServiceConfig
	// The model's own history of each account incarnation by service/account.
	// It changes only on simulated events, never by reading stored state.
	models := map[string]*routeSimAccountModel{}
	model := func(key string) *routeSimAccountModel {
		if models[key] == nil {
			models[key] = &routeSimAccountModel{}
		}
		return models[key]
	}
	// Accounts removed while a monitor probe was in flight; its merge must skip them.
	removedSinceProbe := map[string]bool{}
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
				request := &routeSimRequest{service: service, active: ss.ActiveAccount, activeAddedAt: ss.Accounts[ss.ActiveAccount].AddedAt,
					switchedAt: ss.LastSwitchedAt, startedAt: now, outcome: rng.IntN(3)}
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
				// A Codex quota sample reads both windows full. A Claude 429 marks
				// only its binding window, chosen by step parity so the rng
				// sequence is unchanged; the two windows reset at different times.
				window := -1
				if service.Name == "claude" {
					// Claude headers are stamped when received, unlike a Codex fetch.
					usage.ObservedAt = now
					if request.outcome == 1 {
						window = step % 2
						usage = routeSimUsage(request.startedAt, 10, request.claude.Revision)
						usage.ObservedAt = now
						[]*LimitWindow{&usage.FiveHour, &usage.Weekly}[window].Pct = PtrFloat64(100)
					}
					err = recordClaudeProxyObservation(cfg, service, request.claude, claudeRateLimitObservation{HasUsage: request.outcome != 2, Usage: usage, Rejected: request.outcome == 1}, request.outcome == 2, switchTo)
				} else {
					var sample *UsageSnapshot
					if request.outcome != 2 {
						sample = &usage
					}
					err = recordCodexProxyState(context.Background(), cfg, service, request.codex, sample, request.outcome == 2, switchTo, false, false)
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
				key := service.Name + "/" + request.account
				identity := oldAccount.AddedAt.Equal(request.addedAt)
				selectionChanged := newSS.ActiveAccount != oldSS.ActiveAccount || !newSS.LastSwitchedAt.Equal(oldSS.LastSwitchedAt)
				switched := oldSS.ActiveAccount != request.active || !oldSS.LastSwitchedAt.Equal(request.switchedAt)
				// Force-remove and re-add selects the account again without a switch.
				readded := !oldSS.Accounts[oldSS.ActiveAccount].AddedAt.Equal(request.activeAddedAt)
				m := model(key)
				quotaAt := m.quotaLatest()
				newerRejection := identity && (request.startedAt.Before(m.credAt) || request.startedAt.Before(quotaAt))
				if identity && request.outcome != 2 && request.startedAt.Before(quotaAt) {
					if !newAccount.ProxyUsage.Exhausted() {
						violation("stale observation erased newer quota rejection: %s/%s", service.Name, request.account)
					} else if request.outcome == 0 {
						counts.KeptQuotaRejections++
					} else if window >= 0 && m.quota[window].IsZero() {
						counts.AddedRejectedWindows++
					} else if window >= 0 {
						counts.RepeatedRejections++
					}
				}
				if switched || readded {
					if selectionChanged {
						violation("stale completion undid newer selection: %s active=%s -> %s (re-added=%v)", service.Name, oldSS.ActiveAccount, newSS.ActiveAccount, readded)
					} else if switchTo && oldSS.ActiveAccount != request.account && identity {
						if switched {
							counts.FencedFallbacks++
						} else if !newerRejection {
							counts.ReaddFences++
						}
					}
				}
				if identity && switchTo && newerRejection {
					if selectionChanged {
						violation("stale success selected %s/%s despite a newer rejection", service.Name, request.account)
					} else if !switched && !readded {
						counts.RejectionFences++
					}
				}
				if identity && switchTo && !newerRejection && !switched && !readded {
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
				if identity {
					m.complete(service.Name == "claude", request.outcome, window, request.startedAt, usage.ObservedAt, now)
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
				probed, probeAt = nil, now
				clear(removedSinceProbe)
				var samples []string
				for _, service := range cfg.Services {
					ss := probe.Service(service.Name)
					result := ServiceStatus{Service: service}
					for _, name := range []string{"a", "b", "c"} {
						account, ok := ss.Accounts[name]
						if !ok {
							continue
						}
						probed = append(probed, service.Name+"/"+name)
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
					if (oldSS.ActiveAccount != snapshotSS.ActiveAccount || !oldSS.LastSwitchedAt.Equal(snapshotSS.LastSwitchedAt) ||
						!oldSS.Accounts[oldSS.ActiveAccount].AddedAt.Equal(snapshotSS.Accounts[snapshotSS.ActiveAccount].AddedAt)) &&
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
				for _, key := range probed {
					if !removedSinceProbe[key] {
						model(key).probe(probeAt)
					}
				}
				probe = nil
			}
		default:
			if removed == "" {
				removedService = cfg.Services[rng.IntN(2)]
				names := routeSimAccountNames(before.Service(removedService.Name))
				removed = names[rng.IntN(len(names))]
				if active := before.Service(removedService.Name).ActiveAccount; active != "" && rng.IntN(2) == 0 {
					// Removing the selection is the case that re-selects on re-add.
					removed = active
				}
				log("%s force-remove %s", removedService.Name, removed)
				if removedService.Name == "claude" {
					if err := RemoveClaudeSetupToken(context.Background(), cfg, "claude", removed); err != nil {
						fail("remove setup token: %v", err)
					}
				}
				if err := RemoveAccountWithOptions(cfg, removedService.Name, removed, true, false); err != nil {
					fail("remove: %v", err)
				}
				// Removal ends the incarnation and its history; a re-add starts fresh.
				delete(models, removedService.Name+"/"+removed)
				removedSinceProbe[removedService.Name+"/"+removed] = true
				if before.Service(removedService.Name).ActiveAccount == removed && routeTestState(t, cfg).Service(removedService.Name).ActiveAccount != "" {
					violation("force-removing the active account did not clear selection")
				}
			} else {
				log("%s re-add %s", removedService.Name, removed)
				// The production add path selects the account when nothing is selected.
				if _, _, err := CreateAccountHome(cfg, removedService.Name, removed, ""); err != nil {
					fail("re-add: %v", err)
				}
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
			// Every rejection the model still holds must still be stored.
			for _, name := range []string{"a", "b", "c"} {
				m := models[service.Name+"/"+name]
				if m == nil {
					continue
				}
				account := got.Accounts[name]
				// Report each lost rejection once, at the event that lost it.
				if !m.credAt.IsZero() && account.CredentialsError == "" && !m.credAt.Equal(m.lostCred) {
					m.lostCred = m.credAt
					violation("token rejection at %s lost: %s/%s", m.credAt.Format(time.RFC3339), service.Name, name)
				}
				if latest := m.quotaLatest(); !latest.IsZero() && !latest.Equal(m.lostQuota) {
					lost := !account.ProxyUsage.ObservedAt.Equal(latest)
					for window, at := range m.quota {
						ratio, _ := []LimitWindow{account.ProxyUsage.FiveHour, account.ProxyUsage.Weekly}[window].Ratio()
						lost = lost || !at.IsZero() && ratio < 1
					}
					if lost {
						m.lostQuota = latest
						violation("quota rejection at %s lost: %s/%s windows=%v", latest.Format(time.RFC3339), service.Name, name, m.quota)
					}
				}
			}
		}
	}
	if len(violations) != 0 {
		fail("%s", strings.Join(violations, "\n"))
	}
}

// routeSimAccountModel is the simulation's own history of one account
// incarnation. A rejection retires only on an event that supersedes it, never
// because stored state stopped showing it.
type routeSimAccountModel struct {
	credAt   time.Time    // newest live token rejection
	quota    [2]time.Time // evidence time of the live five-hour and weekly rejections
	sampleAt time.Time    // Codex: newest accepted proxy sample

	lostCred, lostQuota time.Time // last rejection reported lost
}

// quotaLatest is the newest live quota rejection, or zero.
func (m *routeSimAccountModel) quotaLatest() time.Time {
	if m.quota[0].After(m.quota[1]) {
		return m.quota[0]
	}
	return m.quota[1]
}

// complete applies one recorded completion. A request's observation counts
// only if no live quota rejection is newer than its dispatch (Claude) or its
// sample is not older than the newest accepted one (Codex, which orders
// samples by ObservedAt). An accepted sample replaces the quota state; a
// usage-bearing response from a request dispatched at or after a token
// rejection proves the token works again. A Claude rejection that does not
// count still adds the window it names (window; -1 means both windows).
func (m *routeSimAccountModel) complete(claude bool, outcome, window int, dispatchedAt, observedAt, now time.Time) {
	if outcome == 2 {
		m.credAt = now
		return
	}
	accepted := !dispatchedAt.Before(m.quotaLatest())
	if !claude {
		accepted = !observedAt.Before(m.sampleAt)
		if accepted {
			m.sampleAt = observedAt
		}
	}
	if !dispatchedAt.Before(m.credAt) && (claude || accepted && !observedAt.Before(m.credAt)) {
		m.credAt = time.Time{}
	}
	if accepted {
		m.quota = [2]time.Time{}
	} else if !claude || outcome != 1 {
		return
	}
	// Each rejection dates its window, also one already live; never backwards.
	if outcome == 1 {
		for slot := range m.quota {
			if (window < 0 || slot == window) && observedAt.After(m.quota[slot]) {
				m.quota[slot] = observedAt
			}
		}
	}
}

// probe applies a merged monitor probe that started at startedAt. It clears
// a token rejection no newer than its start. Monitor usage is not a proxy
// sample, so it leaves quota rejections alone.
func (m *routeSimAccountModel) probe(startedAt time.Time) {
	if !startedAt.Before(m.credAt) {
		m.credAt = time.Time{}
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
