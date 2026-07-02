package subswapper

import (
	"fmt"
	"strings"
	"time"
)

func RenderStatus(results []ServiceStatus, switches []SwitchEvent, observedAt time.Time) string {
	var b strings.Builder
	fmt.Fprintf(&b, "subswapper status %s\n\n", observedAt.Format(time.RFC3339))
	fmt.Fprintf(&b, "%-10s %-24s %-7s %-28s %-28s %-28s %-8s %s\n", "SERVICE", "ACCOUNT", "ACTIVE", "5H", "WEEKLY", "FABLE5", "SCORE", "STATE")
	fmt.Fprintf(&b, "%-10s %-24s %-7s %-28s %-28s %-28s %-8s %s\n", "-------", "-------", "------", "--", "------", "------", "-----", "-----")
	for _, result := range results {
		if len(result.Accounts) == 0 {
			fmt.Fprintf(&b, "%-10s %-24s %-7s %-28s %-28s %-28s %-8s %s\n", result.Service.Name, "-", "", "-", "-", "-", "-", "no captured accounts")
			continue
		}
		for _, account := range result.Accounts {
			active := ""
			if account.Active {
				active = "yes"
			}
			score := "-"
			if account.Selectable {
				score = fmt.Sprintf("%.0f%%", account.Score*100)
			}
			fmt.Fprintf(&b, "%-10s %-24s %-7s %-28s %-28s %-28s %-8s %s\n",
				account.Service,
				account.Account.Name,
				active,
				formatWindow(account.Account.Usage.FiveHour),
				formatWindow(account.Account.Usage.Weekly),
				formatWindow(account.Account.Usage.FableWeekly),
				score,
				account.Reason,
			)
		}
	}
	if len(switches) > 0 {
		fmt.Fprintf(&b, "\nswitches:\n")
		for _, event := range switches {
			fmt.Fprintf(&b, "- %s -> %s\n", event.Service, event.Account)
		}
	}
	return b.String()
}

func formatWindow(window LimitWindow) string {
	ratio, ok := window.Ratio()
	if !ok {
		return "-"
	}
	if window.Limit > 0 {
		return withReset(window, fmt.Sprintf("%.0f/%.0f %.0f%%", window.Used, window.Limit, ratio*100))
	}
	return withReset(window, fmt.Sprintf("%.0f%%", ratio*100))
}

func withReset(window LimitWindow, value string) string {
	if window.ResetsAt.IsZero() {
		return value
	}
	return value + " reset " + window.ResetsAt.Local().Format("Jan02 15:04")
}
