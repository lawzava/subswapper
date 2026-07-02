package subswapper

import "sort"

func BestAccount(accounts []AccountStatus) (AccountStatus, bool) {
	candidates := make([]AccountStatus, 0, len(accounts))
	for _, account := range accounts {
		if account.Selectable {
			candidates = append(candidates, account)
		}
	}
	if len(candidates) == 0 {
		return AccountStatus{}, false
	}
	sort.SliceStable(candidates, func(i, j int) bool {
		left := candidates[i]
		right := candidates[j]
		if left.Score != right.Score {
			return left.Score < right.Score
		}
		if left.Account.Usage.AverageRatio() != right.Account.Usage.AverageRatio() {
			return left.Account.Usage.AverageRatio() < right.Account.Usage.AverageRatio()
		}
		return left.Account.Name < right.Account.Name
	})
	return candidates[0], true
}
