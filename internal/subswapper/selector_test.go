package subswapper

import "testing"

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

	best, ok := BestAccount(accounts)
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

	best, ok := BestAccount(accounts)
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
	best, ok := BestAccount(accounts)
	if !ok {
		t.Fatal("expected a best account")
	}
	if best.Account.Name != "winner" {
		t.Fatalf("expected winner via average tie-break, got %s", best.Account.Name)
	}

	// "a" and "b" tie on Score and average; the name tie-break decides.
	best, ok = BestAccount(accounts[:2])
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
