package subswapper

import (
	"cmp"
	"slices"
	"time"
)

// BestAccount picks the selectable account that should serve next under
// compareDrainOrder, with one extra rule: within the below-threshold group,
// and within the at-or-above group, an account whose usage sample is at
// least staleAfter old ranks after every fresh one. An idle account's old
// reading can hide use by other clients. Staleness never moves an account
// across the threshold split, so a fresh hot account still yields to a stale
// cool one, and a stale account stays eligible.
func BestAccount(accounts []AccountStatus, threshold float64, staleAfter time.Duration, now time.Time) (AccountStatus, bool) {
	candidates := make([]AccountStatus, 0, len(accounts))
	for _, account := range accounts {
		if account.Selectable {
			candidates = append(candidates, account)
		}
	}
	if len(candidates) == 0 {
		return AccountStatus{}, false
	}
	stale := func(usage UsageSnapshot) bool { return StaleUsage(usage, staleAfter, now) }
	slices.SortStableFunc(candidates, func(left, right AccountStatus) int {
		if order := drainOrder(left.Account.Usage, right.Account.Usage, threshold, now, stale); order != 0 {
			return order
		}
		return cmp.Compare(left.Account.Name, right.Account.Name)
	})
	return candidates[0], true
}

// staleUsageClockSkew is how far in the future a usage sample may be dated
// and still count as fresh. Hub clients render status with their own clock
// while the hub stamped the sample, so a few seconds of skew is normal.
const staleUsageClockSkew = 5 * time.Minute

// StaleUsage reports whether a usage sample is too old to rank with fresh
// ones. A sample without an observation time, or dated more than
// staleUsageClockSkew after now (a clock correction leaves its age unknown),
// counts as stale.
func StaleUsage(usage UsageSnapshot, staleAfter time.Duration, now time.Time) bool {
	return usage.ObservedAt.IsZero() || usage.ObservedAt.After(now.Add(staleUsageClockSkew)) || now.Sub(usage.ObservedAt) >= staleAfter
}

// compareDrainOrder orders accounts by which should serve next. Weekly quota
// left unused at a reset is lost, so among accounts below the switch
// threshold the one whose weekly window resets first goes first, and an
// account with no running weekly window goes last because waiting costs it
// nothing. Accounts at or above the threshold follow, least used first.
// Accounts without usage data come last. The proxies route on this order
// without a staleness split.
func compareDrainOrder(left, right UsageSnapshot, threshold float64, now time.Time) int {
	return drainOrder(left, right, threshold, now, nil)
}

// drainOrder is compareDrainOrder with an optional staleness split. When
// stale is not nil, a fresh sample ranks before a stale one after the
// known-limits and threshold splits and before every finer ordering.
func drainOrder(left, right UsageSnapshot, threshold float64, now time.Time, stale func(UsageSnapshot) bool) int {
	if leftKnown, rightKnown := left.HasAnyLimit(), right.HasAnyLimit(); leftKnown != rightKnown {
		if leftKnown {
			return -1
		}
		return 1
	}
	leftHot, rightHot := left.AtOrAbove(threshold), right.AtOrAbove(threshold)
	if leftHot != rightHot {
		if leftHot {
			return 1
		}
		return -1
	}
	if stale != nil {
		if leftStale, rightStale := stale(left), stale(right); leftStale != rightStale {
			if leftStale {
				return 1
			}
			return -1
		}
	}
	if !leftHot {
		if order := compareWeeklyExpiry(left, right, now); order != 0 {
			return order
		}
	}
	if order := cmp.Compare(left.Score(), right.Score()); order != 0 {
		return order
	}
	return cmp.Compare(left.AverageRatio(), right.AverageRatio())
}

// compareWeeklyExpiry puts the earlier running weekly reset first and
// accounts without a running weekly window last.
func compareWeeklyExpiry(left, right UsageSnapshot, now time.Time) int {
	leftReset, leftRunning := left.Weekly.pendingReset(now)
	rightReset, rightRunning := right.Weekly.pendingReset(now)
	switch {
	case leftRunning && rightRunning:
		return leftReset.Compare(rightReset)
	case leftRunning:
		return -1
	case rightRunning:
		return 1
	}
	return 0
}

// pendingReset returns when a running window resets. A window whose reset
// has passed restarts only on the next use, so it is not running.
func (w LimitWindow) pendingReset(now time.Time) (time.Time, bool) {
	if _, ok := w.Ratio(); !ok || w.ResetsAt.IsZero() || !now.Before(w.ResetsAt) {
		return time.Time{}, false
	}
	return w.ResetsAt, true
}
