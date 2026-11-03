package subswapper

import (
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"
)

const (
	VerdictShort        = "short"
	VerdictBalanced     = "balanced"
	VerdictSurplus      = "surplus"
	VerdictInsufficient = "insufficient data"

	// minVerdictHistory is the least history a verdict is given for: less
	// than a week cannot show a weekly cycle.
	minVerdictHistory = 7 * 24 * time.Hour
	// surplusUtilizationCeiling is the share of the remaining accounts'
	// weekly capacity the consumption may use for the pool to count as
	// oversized: it keeps a 20% safety margin after dropping one account.
	surplusUtilizationCeiling = 0.8
	// usageHistoryMaxGap caps how long one sample stands for. The monitor
	// writes at least hourly, so a longer gap means it was not running.
	usageHistoryMaxGap = 3 * time.Hour
	// weeklyResetJitter merges reset timestamps of one window that the
	// provider reports a few seconds apart.
	weeklyResetJitter = time.Hour
	weekLength        = 7 * 24 * time.Hour

	// DefaultUsageReportSince is the report range when none is given.
	DefaultUsageReportSince = 28 * 24 * time.Hour

	usageReportPlanNote = "Percentages are relative to each account's own plan; they are not comparable in absolute tokens across plans."
)

type UsageReportOptions struct {
	Service   string // empty means every service
	Since     time.Time
	Until     time.Time
	Threshold float64 // switch threshold as a ratio, e.g. 0.9
}

type UsageReport struct {
	Since           time.Time         `json:"since"`
	Until           time.Time         `json:"until"`
	SwitchThreshold float64           `json:"switch_threshold"`
	Note            string            `json:"note"`
	Services        []ServiceCapacity `json:"services"`
}

type ServiceCapacity struct {
	Service  string `json:"service"`
	Verdict  string `json:"verdict"`
	PoolSize int    `json:"pool_size"`
	// HistoryDays is the span between the first and last sample in range.
	HistoryDays float64 `json:"history_days"`
	// WeeklyConsumption is the pool's weekly usage in account-weeks per
	// 7 days: the sum of per-account weekly peaks of completed windows / 100,
	// divided by the weeks of history.
	WeeklyConsumption float64 `json:"weekly_consumption_account_weeks"`
	// SurplusCeiling is the consumption at or below which the pool is
	// oversized by at least one account.
	SurplusCeiling     float64           `json:"surplus_ceiling_account_weeks"`
	ExhaustedHours     float64           `json:"exhausted_hours"`
	ExhaustedEpisodes  int               `json:"exhausted_episodes"`
	AllCrossedSameWeek bool              `json:"all_crossed_threshold_in_same_week"`
	Switches           int               `json:"switches"`
	Accounts           []AccountCapacity `json:"accounts"`
}

type AccountCapacity struct {
	Name                string      `json:"name"`
	SampledHours        float64     `json:"sampled_hours"`
	ActiveShare         float64     `json:"active_share"`
	AboveThresholdHours float64     `json:"above_threshold_hours"`
	FiveHourHits        int         `json:"five_hour_limit_hits"`
	Weeks               []WeekUsage `json:"weeks"`
}

// WeekUsage is one completed weekly window: how far it got before its reset
// and the percentage lost at the reset.
type WeekUsage struct {
	ResetsAt  time.Time `json:"resets_at"`
	PeakPct   float64   `json:"peak_pct"`
	UnusedPct float64   `json:"unused_pct"`
}

type capacityEvidence struct {
	Accounts           int
	Span               time.Duration
	WeeklyConsumption  float64
	ExhaustedEpisodes  int
	AllCrossedSameWeek bool
}

// capacityVerdict decides whether a pool is too small, about right, or
// larger than its usage needs.
func capacityVerdict(e capacityEvidence) string {
	switch {
	case e.Span < minVerdictHistory:
		return VerdictInsufficient
	case e.ExhaustedEpisodes > 0 || e.AllCrossedSameWeek:
		return VerdictShort
	case e.Accounts >= 2 && e.WeeklyConsumption <= surplusCeiling(e.Accounts):
		return VerdictSurplus
	}
	return VerdictBalanced
}

func surplusCeiling(accounts int) float64 {
	return surplusUtilizationCeiling * float64(max(accounts-1, 0))
}

// BuildUsageReport turns history records into per-account and per-service
// capacity evidence. Weekly windows of different accounts reset at different
// times, so windows are measured per account and then summed.
func BuildUsageReport(records []UsageHistoryRecord, opts UsageReportOptions) UsageReport {
	report := UsageReport{Since: opts.Since, Until: opts.Until, SwitchThreshold: opts.Threshold, Note: usageReportPlanNote}
	samples := map[string]map[string][]UsageHistoryRecord{}
	switches := map[string]int{}
	for _, record := range records {
		if record.Time.Before(opts.Since) || record.Time.After(opts.Until) ||
			(opts.Service != "" && record.Service != opts.Service) {
			continue
		}
		switch record.Type {
		case HistoryTypeSample:
			if samples[record.Service] == nil {
				samples[record.Service] = map[string][]UsageHistoryRecord{}
			}
			samples[record.Service][record.Account] = append(samples[record.Service][record.Account], record)
		case HistoryTypeSwitch:
			switches[record.Service]++
		}
	}
	for _, name := range slices.Sorted(maps.Keys(samples)) {
		service := buildServiceCapacity(name, samples[name], opts)
		service.Switches = switches[name]
		report.Services = append(report.Services, service)
	}
	return report
}

func buildServiceCapacity(name string, byAccount map[string][]UsageHistoryRecord, opts UsageReportOptions) ServiceCapacity {
	var start, end time.Time
	for _, samples := range byAccount {
		slices.SortStableFunc(samples, func(a, b UsageHistoryRecord) int { return a.Time.Compare(b.Time) })
		if start.IsZero() || samples[0].Time.Before(start) {
			start = samples[0].Time
		}
		if last := samples[len(samples)-1].Time; last.After(end) {
			end = last
		}
	}
	span := end.Sub(start)
	service := ServiceCapacity{Service: name, HistoryDays: span.Hours() / 24}

	crossed := map[string]map[int]bool{} // pool account -> week buckets back from end
	var peakTotal float64
	for _, accountName := range slices.Sorted(maps.Keys(byAccount)) {
		samples := byAccount[accountName]
		account, accountCrossed := buildAccountCapacity(accountName, samples, start, end, opts)
		for _, w := range account.Weeks {
			peakTotal += w.PeakPct
		}
		service.Accounts = append(service.Accounts, account)
		if end.Sub(samples[len(samples)-1].Time) <= usageHistoryMaxGap {
			service.PoolSize++
			crossed[accountName] = accountCrossed
		}
	}
	if span > 0 {
		service.WeeklyConsumption = peakTotal / 100 / (float64(span) / float64(weekLength))
	}
	service.SurplusCeiling = surplusCeiling(service.PoolSize)
	service.ExhaustedHours, service.ExhaustedEpisodes = poolExhaustion(byAccount, opts.Until)
	service.AllCrossedSameWeek = crossedTogether(crossed)
	service.Verdict = capacityVerdict(capacityEvidence{
		Accounts:           service.PoolSize,
		Span:               span,
		WeeklyConsumption:  service.WeeklyConsumption,
		ExhaustedEpisodes:  service.ExhaustedEpisodes,
		AllCrossedSameWeek: service.AllCrossedSameWeek,
	})
	return service
}

// sampleDurations is how long each sample stands for: until the account's
// next sample, capped, and until the end of the range for the last.
func sampleDurations(samples []UsageHistoryRecord, until time.Time) []time.Duration {
	durations := make([]time.Duration, len(samples))
	for index, sample := range samples {
		next := until
		if index+1 < len(samples) {
			next = samples[index+1].Time
		}
		durations[index] = min(max(next.Sub(sample.Time), 0), usageHistoryMaxGap)
	}
	return durations
}

func buildAccountCapacity(name string, samples []UsageHistoryRecord, start, end time.Time, opts UsageReportOptions) (AccountCapacity, map[int]bool) {
	account := AccountCapacity{Name: name}
	crossed := map[int]bool{}
	durations := sampleDurations(samples, opts.Until)
	var sampled, active, above time.Duration
	hitting := false
	var previousEnd time.Time
	for index, sample := range samples {
		d := durations[index]
		sampled += d
		if sample.Active {
			active += d
		}
		weeklyHot := windowAtOrAbove(sample.Weekly, opts.Threshold)
		if weeklyHot || windowAtOrAbove(sample.FiveHour, opts.Threshold) {
			above += d
		}
		// Five-hour crossings are routine under auto-switching; only a weekly
		// crossing on every account shows the pool is too small.
		if weeklyHot {
			crossed[int(end.Sub(sample.Time)/weekLength)] = true
		}
		hit := sample.FiveHour != nil && sample.FiveHour.Pct >= 100
		contiguous := !previousEnd.IsZero() && !sample.Time.After(previousEnd)
		if hit && (!hitting || !contiguous) {
			account.FiveHourHits++
		}
		hitting = hit
		previousEnd = sample.Time.Add(d)
	}
	account.SampledHours = sampled.Hours()
	account.AboveThresholdHours = above.Hours()
	if sampled > 0 {
		account.ActiveShare = float64(active) / float64(sampled)
	}
	account.Weeks = completedWeeks(samples, start, end)
	return account, crossed
}

func windowAtOrAbove(w *HistoryWindow, threshold float64) bool {
	return w != nil && w.Pct/100 >= threshold
}

// completedWeeks groups an account's samples by weekly window, identified by
// its reset time, and keeps the windows that reset inside the range.
func completedWeeks(samples []UsageHistoryRecord, start, end time.Time) []WeekUsage {
	var weeks []WeekUsage
	var clusterStart time.Time
	for _, sample := range samples {
		w := sample.Weekly
		if w == nil || w.ResetsAt.IsZero() {
			continue
		}
		if len(weeks) == 0 || w.ResetsAt.Sub(clusterStart) > weeklyResetJitter {
			clusterStart = w.ResetsAt
			weeks = append(weeks, WeekUsage{})
		}
		current := &weeks[len(weeks)-1]
		current.ResetsAt = w.ResetsAt
		current.PeakPct = max(current.PeakPct, w.Pct)
	}
	weeks = slices.DeleteFunc(weeks, func(w WeekUsage) bool {
		return w.ResetsAt.After(end) || !w.ResetsAt.After(start)
	})
	for index := range weeks {
		weeks[index].UnusedPct = max(0, 100-weeks[index].PeakPct)
	}
	return weeks
}

// poolExhaustion measures the time no account with known state was
// selectable. Each timestamp at which any account was sampled stands for the
// interval to the next one, capped like single samples.
func poolExhaustion(byAccount map[string][]UsageHistoryRecord, until time.Time) (hours float64, episodes int) {
	var times []time.Time
	for _, samples := range byAccount {
		for _, sample := range samples {
			times = append(times, sample.Time)
		}
	}
	slices.SortFunc(times, func(a, b time.Time) int { return a.Compare(b) })
	times = slices.CompactFunc(times, func(a, b time.Time) bool { return a.Equal(b) })

	cursors := map[string]int{}
	var total time.Duration
	var previousEnd time.Time
	previousExhausted := false
	for index, at := range times {
		known, selectable := 0, 0
		for name, samples := range byAccount {
			cursor := cursors[name]
			for cursor+1 < len(samples) && !samples[cursor+1].Time.After(at) {
				cursor++
			}
			cursors[name] = cursor
			current := samples[cursor]
			if current.Time.After(at) || at.Sub(current.Time) > usageHistoryMaxGap {
				continue
			}
			known++
			if current.Selectable {
				selectable++
			}
		}
		next := until
		if index+1 < len(times) {
			next = times[index+1]
		}
		d := min(max(next.Sub(at), 0), usageHistoryMaxGap)
		exhausted := known > 0 && selectable == 0
		if exhausted {
			total += d
			if !previousExhausted || at.After(previousEnd) {
				episodes++
			}
		}
		previousExhausted = exhausted
		previousEnd = at.Add(d)
	}
	return total.Hours(), episodes
}

// crossedTogether reports whether some week bucket has every account across
// the switch threshold.
func crossedTogether(crossed map[string]map[int]bool) bool {
	if len(crossed) == 0 {
		return false
	}
	var common map[int]bool
	for _, weeks := range crossed {
		if common == nil {
			common = maps.Clone(weeks)
			continue
		}
		for bucket := range common {
			if !weeks[bucket] {
				delete(common, bucket)
			}
		}
	}
	return len(common) > 0
}

// RenderUsageReport formats the report for a terminal.
func RenderUsageReport(report UsageReport) string {
	var b strings.Builder
	fmt.Fprintf(&b, "subswapper usage report %s to %s (switch threshold %.0f%%)\n",
		report.Since.Local().Format("2006-01-02 15:04"), report.Until.Local().Format("2006-01-02 15:04"), report.SwitchThreshold*100)
	fmt.Fprintln(&b, report.Note)
	if len(report.Services) == 0 {
		fmt.Fprintln(&b, "\nno usage history in this range")
		return b.String()
	}
	for _, service := range report.Services {
		fmt.Fprintf(&b, "\n%s: %s\n", service.Service, service.Verdict)
		fmt.Fprintf(&b, "  %d accounts, %.1f days of history, %d switches\n", service.PoolSize, service.HistoryDays, service.Switches)
		fmt.Fprintf(&b, "  weekly consumption %.2f account-weeks per week (surplus needs <= %.2f with %d accounts)\n",
			service.WeeklyConsumption, service.SurplusCeiling, service.PoolSize)
		fmt.Fprintf(&b, "  pool exhausted (no account selectable): %.1f h in %d episodes\n", service.ExhaustedHours, service.ExhaustedEpisodes)
		fmt.Fprintf(&b, "  every account above the threshold in the same week: %s\n", yesNo(service.AllCrossedSameWeek))
		fmt.Fprintf(&b, "  %-20s %8s %8s %12s %9s %s\n", "ACCOUNT", "SAMPLED", "ACTIVE", "ABOVE THRESH", "5H HITS", "WEEKS (peak% / unused%)")
		for _, account := range service.Accounts {
			fmt.Fprintf(&b, "  %-20s %7.0fh %7.0f%% %11.1fh %9d %s\n", account.Name, account.SampledHours,
				account.ActiveShare*100, account.AboveThresholdHours, account.FiveHourHits, formatWeeks(account.Weeks))
		}
	}
	return b.String()
}

func formatWeeks(weeks []WeekUsage) string {
	if len(weeks) == 0 {
		return "none completed"
	}
	parts := make([]string, 0, len(weeks))
	for _, w := range weeks {
		parts = append(parts, fmt.Sprintf("%.0f/%.0f", w.PeakPct, w.UnusedPct))
	}
	return strings.Join(parts, " ")
}

func yesNo(value bool) string {
	if value {
		return "yes"
	}
	return "no"
}
