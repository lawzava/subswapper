package subswapper

import (
	"cmp"
	"slices"
	"time"
)

// BestAccount picks the selectable account that should serve next under
// compareDrainOrder.
func BestAccount(accounts []AccountStatus, threshold float64) (AccountStatus, bool) {
	candidates := make([]AccountStatus, 0, len(accounts))
	for _, account := range accounts {
		if account.Selectable {
			candidates = append(candidates, account)
		}
	}
	if len(candidates) == 0 {
		return AccountStatus{}, false
	}
	now := time.Now()
	slices.SortStableFunc(candidates, func(left, right AccountStatus) int {
		if order := compareDrainOrder(left.Account.Usage, right.Account.Usage, threshold, now); order != 0 {
			return order
		}
		return cmp.Compare(left.Account.Name, right.Account.Name)
	})
	return candidates[0], true
}

// compareDrainOrder orders accounts by which should serve next. Weekly quota
// left unused at a reset is lost, so among accounts below the switch
// threshold the one whose weekly window resets first goes first, and an
// account with no running weekly window goes last because waiting costs it
// nothing. Accounts at or above the threshold follow, least used first.
// Accounts without usage data come last.
func compareDrainOrder(left, right UsageSnapshot, threshold float64, now time.Time) int {
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
