package subswapper

import (
	"testing"
	"time"
)

func TestBestAccountChoosesLowestWorstWindowUsage(t *testing.T) {
	accounts := []AccountStatus{
		statusForTest("higher-5h", 50, 10),
		statusForTest("best", 20, 20),
		{
			Account:    AccountState{Name: "exhausted", Usage: usageForTest(0, 0)},
			Selectable: false,
			Score:      0,
		},
	}

	best, ok := BestAccount(accounts, defaultAutoSwitchThreshold, defaultStaleUsageAfter, time.Now())
	if !ok {
		t.Fatal("expected a best account")
	}
	if best.Account.Name != "best" {
		t.Fatalf("expected best, got %s", best.Account.Name)
	}
}

func TestBestAccountConsidersFableWeeklyUsage(t *testing.T) {
	lowGeneralHighFable := usageForTest(10, 10)
	lowGeneralHighFable.FableWeekly = LimitWindow{Pct: PtrFloat64(90)}
	steady := usageForTest(30, 30)
	accounts := []AccountStatus{
		{
			Account:    AccountState{Name: "fable-heavy", Usage: lowGeneralHighFable},
			Selectable: true,
			Score:      lowGeneralHighFable.Score(),
		},
		{
			Account:    AccountState{Name: "steady", Usage: steady},
			Selectable: true,
			Score:      steady.Score(),
		},
	}

	best, ok := BestAccount(accounts, defaultAutoSwitchThreshold, defaultStaleUsageAfter, time.Now())
	if !ok {
		t.Fatal("expected a best account")
	}
	if best.Account.Name != "steady" {
		t.Fatalf("expected steady, got %s", best.Account.Name)
	}
}

func TestBestAccountTieBreaksByAverageThenName(t *testing.T) {
	// All three tie on Score (worst window 40%); "winner" has the lowest
	// average, so only the average tie-break can select it.
	accounts := []AccountStatus{
		statusForTest("b", 40, 10),
		statusForTest("a", 10, 40),
		statusForTest("winner", 40, 5),
	}
	best, ok := BestAccount(accounts, defaultAutoSwitchThreshold, defaultStaleUsageAfter, time.Now())
	if !ok {
		t.Fatal("expected a best account")
	}
	if best.Account.Name != "winner" {
		t.Fatalf("expected winner via average tie-break, got %s", best.Account.Name)
	}

	// "a" and "b" tie on Score and average; the name tie-break decides.
	best, ok = BestAccount(accounts[:2], defaultAutoSwitchThreshold, defaultStaleUsageAfter, time.Now())
	if !ok {
		t.Fatal("expected a best account")
	}
	if best.Account.Name != "a" {
		t.Fatalf("expected a via name tie-break, got %s", best.Account.Name)
	}
}

func statusForTest(name string, fiveHourPct, weeklyPct float64) AccountStatus {
	usage := usageForTest(fiveHourPct, weeklyPct)
	return AccountStatus{
		Account:    AccountState{Name: name, Usage: usage},
		Selectable: true,
		Score:      usage.Score(),
	}
}

func TestBestAccountDrainsEarliestWeeklyResetFirst(t *testing.T) {
	now := time.Now()
	// Mirrors a real table: "soon" is half used but its weekly window
	// resets today, "later" resets in three days, and "idle" has no running
	// weekly window, so waiting costs it nothing.
	accounts := []AccountStatus{
		weeklyResetStatusForTest("later", 14, 36, now.Add(72*time.Hour)),
		weeklyResetStatusForTest("soon", 0, 49, now.Add(9*time.Hour)),
		weeklyResetStatusForTest("idle", 0, 0, now.Add(-96*time.Hour)),
	}
	best, ok := BestAccount(accounts, defaultAutoSwitchThreshold, defaultStaleUsageAfter, time.Now())
	if !ok || best.Account.Name != "soon" {
		t.Fatalf("best = %q, want soon", best.Account.Name)
	}
	best, ok = BestAccount([]AccountStatus{accounts[0], accounts[2]}, defaultAutoSwitchThreshold, defaultStaleUsageAfter, time.Now())
	if !ok || best.Account.Name != "later" {
		t.Fatalf("best = %q, want the running window before the idle one", best.Account.Name)
	}
}

func TestBestAccountSkipsEarlyResetAtThreshold(t *testing.T) {
	now := time.Now()
	accounts := []AccountStatus{
		weeklyResetStatusForTest("hot", 95, 40, now.Add(2*time.Hour)),
		weeklyResetStatusForTest("cool", 10, 60, now.Add(72*time.Hour)),
	}
	best, ok := BestAccount(accounts, defaultAutoSwitchThreshold, defaultStaleUsageAfter, time.Now())
	if !ok || best.Account.Name != "cool" {
		t.Fatalf("best = %q, want cool", best.Account.Name)
	}
}

func weeklyResetStatusForTest(name string, fiveHourPct, weeklyPct float64, weeklyReset time.Time) AccountStatus {
	status := statusForTest(name, fiveHourPct, weeklyPct)
	status.Account.Usage.Weekly.ResetsAt = weeklyReset
	return status
}

func TestBestAccountOrdersWeeklyOnlyAccountsByWeeklyDrain(t *testing.T) {
	now := time.Now()
	weeklyOnly := func(name string, weeklyPct float64, weeklyReset time.Time) AccountStatus {
		usage := UsageSnapshot{Weekly: LimitWindow{Pct: PtrFloat64(weeklyPct), ResetsAt: weeklyReset}}
		return AccountStatus{Account: AccountState{Name: name, Usage: usage}, Selectable: true, Score: usage.Score()}
	}
	accounts := []AccountStatus{
		weeklyOnly("later", 36, now.Add(72*time.Hour)),
		weeklyOnly("soon", 49, now.Add(9*time.Hour)),
	}
	best, ok := BestAccount(accounts, defaultAutoSwitchThreshold, defaultStaleUsageAfter, time.Now())
	if !ok || best.Account.Name != "soon" {
		t.Fatalf("best = %q, want soon", best.Account.Name)
	}
	// Mixed with an account that still has a five-hour window.
	mixed := []AccountStatus{weeklyResetStatusForTest("max", 5, 36, now.Add(72*time.Hour)), accounts[1]}
	if best, ok = BestAccount(mixed, defaultAutoSwitchThreshold, defaultStaleUsageAfter, time.Now()); !ok || best.Account.Name != "soon" {
		t.Fatalf("mixed best = %q, want soon", best.Account.Name)
	}
	hot := weeklyOnly("hot", 95, now.Add(2*time.Hour))
	if best, ok = BestAccount([]AccountStatus{hot, accounts[0]}, defaultAutoSwitchThreshold, defaultStaleUsageAfter, time.Now()); !ok || best.Account.Name != "later" {
		t.Fatalf("best = %q, want later because hot is past the threshold", best.Account.Name)
	}
}

func observedStatusForTest(name string, weeklyPct float64, weeklyReset, observedAt time.Time) AccountStatus {
	status := weeklyResetStatusForTest(name, 0, weeklyPct, weeklyReset)
	status.Account.Usage.ObservedAt = observedAt
	return status
}

func TestBestAccountRanksStaleIdleAccountAfterFreshAccount(t *testing.T) {
	now := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)
	// "idle" would win on drain order: its weekly window resets first. Its
	// sample is days old, so other clients may have used the account since.
	accounts := []AccountStatus{
		observedStatusForTest("idle", 10, now.Add(6*time.Hour), now.Add(-72*time.Hour)),
		observedStatusForTest("fresh", 40, now.Add(96*time.Hour), now.Add(-5*time.Minute)),
	}
	best, ok := BestAccount(accounts, defaultAutoSwitchThreshold, defaultStaleUsageAfter, now)
	if !ok || best.Account.Name != "fresh" {
		t.Fatalf("best = %q, want fresh before the stale idle account", best.Account.Name)
	}
}

func TestBestAccountUsesStaleAccountWhenNoFreshCandidate(t *testing.T) {
	now := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)
	stale := observedStatusForTest("stale", 10, now.Add(6*time.Hour), now.Add(-72*time.Hour))
	unselectable := observedStatusForTest("fresh-exhausted", 40, now.Add(96*time.Hour), now)
	unselectable.Selectable = false
	best, ok := BestAccount([]AccountStatus{unselectable, stale}, defaultAutoSwitchThreshold, defaultStaleUsageAfter, now)
	if !ok || best.Account.Name != "stale" {
		t.Fatalf("best = %q, %v; want the only selectable stale account", best.Account.Name, ok)
	}

	// When every candidate is stale, drain order still decides.
	allStale := []AccountStatus{
		observedStatusForTest("later", 10, now.Add(96*time.Hour), now.Add(-24*time.Hour)),
		observedStatusForTest("soon", 40, now.Add(6*time.Hour), now.Add(-72*time.Hour)),
		observedStatusForTest("unknown-time", 5, now.Add(2*time.Hour), time.Time{}),
	}
	best, ok = BestAccount(allStale, defaultAutoSwitchThreshold, defaultStaleUsageAfter, now)
	if !ok || best.Account.Name != "unknown-time" {
		t.Fatalf("best = %q, want unknown-time by earliest weekly reset", best.Account.Name)
	}
	best, ok = BestAccount(allStale[:2], defaultAutoSwitchThreshold, defaultStaleUsageAfter, now)
	if !ok || best.Account.Name != "soon" {
		t.Fatalf("best = %q, want soon by earliest weekly reset", best.Account.Name)
	}
}

func TestBestAccountStaleUsageThreshold(t *testing.T) {
	now := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name       string
		staleAfter time.Duration
		age        time.Duration
		want       string
	}{
		{name: "default just under", staleAfter: defaultStaleUsageAfter, age: defaultStaleUsageAfter - time.Second, want: "early"},
		{name: "default at limit", staleAfter: defaultStaleUsageAfter, age: defaultStaleUsageAfter, want: "fresh"},
		{name: "default over limit", staleAfter: defaultStaleUsageAfter, age: defaultStaleUsageAfter + time.Minute, want: "fresh"},
		{name: "custom keeps four hours fresh", staleAfter: 6 * time.Hour, age: 4 * time.Hour, want: "early"},
		{name: "custom marks two hours stale", staleAfter: time.Hour, age: 2 * time.Hour, want: "fresh"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			accounts := []AccountStatus{
				observedStatusForTest("early", 10, now.Add(6*time.Hour), now.Add(-test.age)),
				observedStatusForTest("fresh", 40, now.Add(96*time.Hour), now),
			}
			best, ok := BestAccount(accounts, defaultAutoSwitchThreshold, test.staleAfter, now)
			if !ok || best.Account.Name != test.want {
				t.Fatalf("best = %q, want %q", best.Account.Name, test.want)
			}
		})
	}
}

func TestStaleUsageTreatsMissingObservationAsStale(t *testing.T) {
	now := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)
	if !StaleUsage(UsageSnapshot{}, defaultStaleUsageAfter, now) {
		t.Fatal("a sample without ObservedAt must count as stale")
	}
	if StaleUsage(UsageSnapshot{ObservedAt: now}, defaultStaleUsageAfter, now) {
		t.Fatal("a sample observed now must count as fresh")
	}
}

func TestStaleUsageTreatsFutureObservationAsStale(t *testing.T) {
	now := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)
	// A clock correction can leave a sample dated after now; its age is
	// unknown, so it must not outrank a genuinely fresh reading.
	if !StaleUsage(UsageSnapshot{ObservedAt: now.Add(time.Minute)}, defaultStaleUsageAfter, now) {
		t.Fatal("a sample observed in the future must count as stale")
	}
	future := observedStatusForTest("future", 20, now.Add(24*time.Hour), now.Add(time.Hour))
	fresh := observedStatusForTest("fresh", 20, now.Add(72*time.Hour), now.Add(-time.Minute))
	best, ok := BestAccount([]AccountStatus{future, fresh}, defaultAutoSwitchThreshold, defaultStaleUsageAfter, now)
	if !ok || best.Account.Name != "fresh" {
		t.Fatalf("best = %q, want the fresh account before the future-dated one", best.Account.Name)
	}
}

func TestBestAccountPrefersStaleCoolAccountOverFreshHotActive(t *testing.T) {
	now := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)
	// The active account is fresh but at the switch threshold. Staleness
	// must not keep the monitor on it while a cool account is selectable.
	hot := weeklyResetStatusForTest("hot-active", 95, 60, now.Add(96*time.Hour))
	hot.Account.Usage.ObservedAt = now
	hot.Active = true
	idle := observedStatusForTest("stale-idle", 20, now.Add(72*time.Hour), now.Add(-72*time.Hour))
	best, ok := BestAccount([]AccountStatus{hot, idle}, defaultAutoSwitchThreshold, defaultStaleUsageAfter, now)
	if !ok || best.Account.Name != "stale-idle" {
		t.Fatalf("best = %q, want the stale cool account before the fresh hot one", best.Account.Name)
	}

	// Among hot accounts, a fresh reading still ranks first.
	staleHot := weeklyResetStatusForTest("stale-hot", 91, 60, now.Add(96*time.Hour))
	staleHot.Account.Usage.ObservedAt = now.Add(-72 * time.Hour)
	best, ok = BestAccount([]AccountStatus{staleHot, hot}, defaultAutoSwitchThreshold, defaultStaleUsageAfter, now)
	if !ok || best.Account.Name != "hot-active" {
		t.Fatalf("best = %q, want the fresh hot account before the stale hot one", best.Account.Name)
	}
}
