package subswapper

import (
	"math"
	"strings"
	"testing"
	"time"
)

var reportBase = time.Date(2026, 1, 5, 0, 0, 0, 0, time.UTC)

// syntheticAccount describes one account's hourly history over four weekly
// windows followed by a few hours of a fifth, still running one. Weekly pct
// climbs to peaks[week] by the last hour of the week. Reset times jitter by a
// few seconds, as provider timestamps do.
type syntheticAccount struct {
	name         string
	peaks        [4]float64
	activeUntil  time.Duration // active while the sample is before reportBase+activeUntil
	activeFrom   time.Duration
	fiveHourHits []int // sample indexes (hours since reportBase) where the 5h window reads 100
}

const syntheticTailHours = 12

func syntheticHistory(accounts []syntheticAccount, exhausted map[int]bool) []UsageHistoryRecord {
	var records []UsageHistoryRecord
	hours := 4*7*24 + syntheticTailHours
	for hour := range hours {
		at := reportBase.Add(time.Duration(hour) * time.Hour)
		week := hour / (7 * 24)
		inWeek := hour % (7 * 24)
		for _, account := range accounts {
			peak, reset := 5.0, reportBase.Add(5*7*24*time.Hour)
			pct := peak * float64(inWeek+1) / syntheticTailHours
			if week < 4 {
				peak = account.peaks[week]
				reset = reportBase.Add(time.Duration(week+1) * 7 * 24 * time.Hour).Add(time.Duration(hour%3) * time.Second)
				pct = peak * float64(inWeek+1) / (7 * 24)
			}
			five := 10.0
			for _, hit := range account.fiveHourHits {
				if hit == hour {
					five = 100
				}
			}
			since := time.Duration(hour) * time.Hour
			records = append(records, UsageHistoryRecord{
				Time: at, Type: HistoryTypeSample, Service: "claude", Account: account.name,
				Active:     since >= account.activeFrom && since < account.activeUntil,
				Selectable: !exhausted[hour],
				State:      "ready",
				FiveHour:   &HistoryWindow{Pct: five, ResetsAt: at.Add(3 * time.Hour)},
				Weekly:     &HistoryWindow{Pct: pct, ResetsAt: reset},
			})
		}
	}
	return records
}

func reportOptions() UsageReportOptions {
	return UsageReportOptions{
		Since:     reportBase.Add(-time.Hour),
		Until:     reportBase.Add(time.Duration(4*7*24+syntheticTailHours) * time.Hour),
		Threshold: 0.9,
	}
}

func near(got, want float64) bool { return math.Abs(got-want) < 1e-6 }

func TestUsageReportAggregatesPerAccountWeeklyWindows(t *testing.T) {
	week := 7 * 24 * time.Hour
	records := syntheticHistory([]syntheticAccount{
		{name: "a", peaks: [4]float64{80, 60, 40, 90}, activeUntil: 2 * week},
		{name: "b", peaks: [4]float64{20, 10, 30, 20}, activeFrom: 2 * week, activeUntil: 10 * week, fiveHourHits: []int{10, 40}},
	}, nil)
	report := BuildUsageReport(records, reportOptions())
	if len(report.Services) != 1 {
		t.Fatalf("services = %+v", report.Services)
	}
	service := report.Services[0]
	if service.Service != "claude" || service.PoolSize != 2 || len(service.Accounts) != 2 {
		t.Fatalf("service = %+v", service)
	}
	a, b := service.Accounts[0], service.Accounts[1]
	if a.Name != "a" || b.Name != "b" {
		t.Fatalf("accounts out of order: %s %s", a.Name, b.Name)
	}

	// The fifth window is still running, so only four count; each loses
	// 100 - peak at its reset.
	wantPeaks := []float64{80, 60, 40, 90}
	if len(a.Weeks) != 4 {
		t.Fatalf("account a completed windows = %+v", a.Weeks)
	}
	for index, w := range a.Weeks {
		if !near(w.PeakPct, wantPeaks[index]) || !near(w.UnusedPct, 100-wantPeaks[index]) {
			t.Fatalf("a week %d = %+v", index, w)
		}
	}

	// Only the final sample of a's last window (90%) reaches the 90% switch
	// threshold, and each sample stands for one hour.
	if !near(a.AboveThresholdHours, 1) {
		t.Fatalf("a above threshold = %v h, want 1", a.AboveThresholdHours)
	}
	// b reads 100% on the five-hour window at two separate points.
	if b.FiveHourHits != 2 || a.FiveHourHits != 0 {
		t.Fatalf("five-hour hits a=%d b=%d, want 0 and 2", a.FiveHourHits, b.FiveHourHits)
	}
	if !near(b.AboveThresholdHours, 2) {
		t.Fatalf("b above threshold = %v h, want 2", b.AboveThresholdHours)
	}

	sampled := float64(4*7*24 + syntheticTailHours)
	if !near(a.SampledHours, sampled) || !near(a.ActiveShare, 14*24/sampled) {
		t.Fatalf("a sampled %v h, active share %v", a.SampledHours, a.ActiveShare)
	}
	if !near(b.ActiveShare, (sampled-14*24)/sampled) {
		t.Fatalf("b active share %v", b.ActiveShare)
	}

	// Peaks sum to 270 + 80 = 350 percent: 3.5 account-weeks over the
	// (28d + 11h) between the first and last sample.
	wantConsumption := 3.5 / ((float64(4*7*24+syntheticTailHours-1) / 24) / 7)
	if !near(service.WeeklyConsumption, wantConsumption) {
		t.Fatalf("weekly consumption = %v, want %v", service.WeeklyConsumption, wantConsumption)
	}
	if service.ExhaustedEpisodes != 0 || service.ExhaustedHours != 0 || service.AllCrossedSameWeek {
		t.Fatalf("unexpected exhaustion or crossing: %+v", service)
	}
	// 0.86 account-weeks per week exceeds 0.8 * (2 - 1).
	if service.Verdict != VerdictBalanced {
		t.Fatalf("verdict = %q, want balanced", service.Verdict)
	}
}

func TestUsageReportCountsPoolExhaustionEpisodes(t *testing.T) {
	records := syntheticHistory([]syntheticAccount{
		{name: "a", peaks: [4]float64{10, 10, 10, 10}},
		{name: "b", peaks: [4]float64{10, 10, 10, 10}},
	}, map[int]bool{100: true, 101: true, 102: true, 103: true, 500: true, 501: true})
	service := BuildUsageReport(records, reportOptions()).Services[0]
	if service.ExhaustedEpisodes != 2 || !near(service.ExhaustedHours, 6) {
		t.Fatalf("exhaustion = %d episodes, %v h; want 2 episodes, 6 h", service.ExhaustedEpisodes, service.ExhaustedHours)
	}
	if service.Verdict != VerdictShort {
		t.Fatalf("verdict = %q, want short", service.Verdict)
	}
}

func TestUsageReportFlagsEveryAccountCrossingInTheSameWeek(t *testing.T) {
	records := syntheticHistory([]syntheticAccount{
		{name: "a", peaks: [4]float64{10, 10, 10, 95}},
		{name: "b", peaks: [4]float64{10, 10, 10, 92}},
	}, nil)
	service := BuildUsageReport(records, reportOptions()).Services[0]
	if !service.AllCrossedSameWeek || service.Verdict != VerdictShort {
		t.Fatalf("service = %+v", service)
	}
}

// Auto-switching routinely pushes Max accounts past the threshold on the
// five-hour window; only weekly crossings show that the pool is too small.
func TestUsageReportIgnoresFiveHourCrossingsForSameWeek(t *testing.T) {
	records := syntheticHistory([]syntheticAccount{
		{name: "a", peaks: [4]float64{10, 10, 10, 10}, fiveHourHits: []int{30}},
		{name: "b", peaks: [4]float64{10, 10, 10, 10}, fiveHourHits: []int{31}},
	}, nil)
	service := BuildUsageReport(records, reportOptions()).Services[0]
	if service.AllCrossedSameWeek || service.Verdict == VerdictShort {
		t.Fatalf("service = %+v", service)
	}
}

func TestUsageReportIgnoresRecordsOutsideTheRangeAndOtherServices(t *testing.T) {
	records := syntheticHistory([]syntheticAccount{
		{name: "a", peaks: [4]float64{80, 60, 40, 90}},
		{name: "b", peaks: [4]float64{20, 10, 30, 20}},
	}, nil)
	other := records[0]
	other.Service = "codex"
	records = append(records, other)
	records = append(records, UsageHistoryRecord{Time: reportBase.Add(time.Hour), Type: HistoryTypeSwitch, Service: "claude", From: "a", To: "b", Reason: HistorySwitchAuto})

	opts := reportOptions()
	opts.Service = "claude"
	report := BuildUsageReport(records, opts)
	if len(report.Services) != 1 || report.Services[0].Service != "claude" {
		t.Fatalf("services = %+v", report.Services)
	}
	if report.Services[0].Switches != 1 {
		t.Fatalf("switches = %d, want 1", report.Services[0].Switches)
	}
	// Restricting the range to the last two completed windows drops the first two.
	opts.Since = reportBase.Add(14 * 24 * time.Hour)
	narrow := BuildUsageReport(records, opts).Services[0]
	if got := len(narrow.Accounts[0].Weeks); got != 2 {
		t.Fatalf("windows in the narrowed range = %d, want 2", got)
	}
}

func TestUsageReportWithShortHistoryHasInsufficientData(t *testing.T) {
	records := syntheticHistory([]syntheticAccount{{name: "a"}, {name: "b"}}, nil)
	opts := reportOptions()
	opts.Until = reportBase.Add(3 * 24 * time.Hour)
	var kept []UsageHistoryRecord
	for _, record := range records {
		if record.Time.Before(opts.Until) {
			kept = append(kept, record)
		}
	}
	if verdict := BuildUsageReport(kept, opts).Services[0].Verdict; verdict != VerdictInsufficient {
		t.Fatalf("verdict = %q, want %q", verdict, VerdictInsufficient)
	}
}

func TestCapacityVerdict(t *testing.T) {
	week := 7 * 24 * time.Hour
	tests := []struct {
		name string
		in   capacityEvidence
		want string
	}{
		{"short history", capacityEvidence{Accounts: 3, Span: 6 * 24 * time.Hour, ExhaustedEpisodes: 2}, VerdictInsufficient},
		{"exhausted", capacityEvidence{Accounts: 3, Span: 4 * week, ExhaustedEpisodes: 1, WeeklyConsumption: 0.1}, VerdictShort},
		{"all crossed", capacityEvidence{Accounts: 3, Span: 4 * week, AllCrossedSameWeek: true, WeeklyConsumption: 0.1}, VerdictShort},
		{"fits one fewer with margin", capacityEvidence{Accounts: 3, Span: 4 * week, WeeklyConsumption: 1.6}, VerdictSurplus},
		{"just over the margin", capacityEvidence{Accounts: 3, Span: 4 * week, WeeklyConsumption: 1.61}, VerdictBalanced},
		{"two accounts at the limit", capacityEvidence{Accounts: 2, Span: 4 * week, WeeklyConsumption: 0.8}, VerdictSurplus},
		{"two accounts over", capacityEvidence{Accounts: 2, Span: 4 * week, WeeklyConsumption: 0.81}, VerdictBalanced},
		{"single account has nothing to drop", capacityEvidence{Accounts: 1, Span: 4 * week, WeeklyConsumption: 0.1}, VerdictBalanced},
		{"exactly seven days", capacityEvidence{Accounts: 3, Span: week, WeeklyConsumption: 1}, VerdictSurplus},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := capacityVerdict(test.in); got != test.want {
				t.Fatalf("capacityVerdict(%+v) = %q, want %q", test.in, got, test.want)
			}
		})
	}
}

func TestRenderUsageReportShowsEvidenceNextToVerdict(t *testing.T) {
	week := 7 * 24 * time.Hour
	records := syntheticHistory([]syntheticAccount{
		{name: "a", peaks: [4]float64{80, 60, 40, 90}, activeUntil: 2 * week},
		{name: "b", peaks: [4]float64{20, 10, 30, 20}, activeFrom: 2 * week, activeUntil: 10 * week},
	}, nil)
	out := RenderUsageReport(BuildUsageReport(records, reportOptions()))
	for _, want := range []string{
		"claude", "balanced", "account-weeks", "2 accounts",
		"not comparable", "a ", "b ", "unused",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("report lacks %q:\n%s", want, out)
		}
	}
	t.Log("\n" + out)
}

func TestUsageReportDoesNotCountGapsWhenTheMonitorWasDown(t *testing.T) {
	sample := func(at time.Time) UsageHistoryRecord {
		return UsageHistoryRecord{Time: at, Type: HistoryTypeSample, Service: "codex", Account: "a", Selectable: true, State: "ready",
			Weekly: &HistoryWindow{Pct: 10, ResetsAt: at.Add(24 * time.Hour)}}
	}
	records := []UsageHistoryRecord{sample(reportBase), sample(reportBase.Add(time.Hour)), sample(reportBase.Add(20 * time.Hour))}
	opts := UsageReportOptions{Since: reportBase, Until: reportBase.Add(21 * time.Hour), Threshold: 0.9}
	account := BuildUsageReport(records, opts).Services[0].Accounts[0]
	// The 19-hour hole after the second sample counts for the 3-hour cap only.
	want := (time.Hour + usageHistoryMaxGap + time.Hour).Hours()
	if !near(account.SampledHours, want) {
		t.Fatalf("sampled %v h, want %v", account.SampledHours, want)
	}
}
