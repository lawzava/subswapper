package subswapper

import (
	"fmt"
	"math"
	"time"
)

// StatusReport is the stable JSON form of `subswapper status -json`, read by
// the Paseo plugin and scripts. Percentages are 0-100.
type StatusReport struct {
	GeneratedAt time.Time             `json:"generated_at"`
	Services    []StatusReportService `json:"services"`
}

type StatusReportService struct {
	Name string `json:"name"`
	Kind string `json:"kind"`
	// Hub is the hub URL when this machine is a hub client for the service.
	Hub      string                `json:"hub,omitempty"`
	Note     string                `json:"note,omitempty"`
	Selected string                `json:"selected,omitempty"`
	Accounts []StatusReportAccount `json:"accounts"`
}

type StatusReportAccount struct {
	Name     string `json:"name"`
	Email    string `json:"email,omitempty"`
	Selected bool   `json:"selected"`
	// Ready means the account can take requests; Score is its worst window.
	Ready       bool          `json:"ready"`
	Score       *float64      `json:"score,omitempty"`
	State       string        `json:"state"`
	FiveHour    *StatusWindow `json:"five_hour,omitempty"`
	Weekly      *StatusWindow `json:"weekly,omitempty"`
	FableWeekly *StatusWindow `json:"fable_weekly,omitempty"`
	UpdatedAt   time.Time     `json:"updated_at,omitzero"`
}

type StatusWindow struct {
	UsedPercent float64   `json:"used_percent"`
	ResetsAt    time.Time `json:"resets_at,omitzero"`
}

func BuildStatusReport(results []ServiceStatus, now time.Time) StatusReport {
	report := StatusReport{GeneratedAt: now, Services: make([]StatusReportService, 0, len(results))}
	for _, result := range results {
		service := StatusReportService{
			Name:     result.Service.Name,
			Kind:     result.Service.Kind,
			Hub:      result.Service.HubURL,
			Note:     result.Note,
			Accounts: make([]StatusReportAccount, 0, len(result.Accounts)),
		}
		for _, account := range result.Accounts {
			entry := StatusReportAccount{
				Name:        account.Account.Name,
				Email:       account.Account.Email,
				Selected:    account.Active,
				Ready:       account.Selectable,
				State:       account.Reason,
				FiveHour:    statusWindow(account.Account.Usage.FiveHour),
				Weekly:      statusWindow(account.Account.Usage.Weekly),
				FableWeekly: statusWindow(account.Account.Usage.FableWeekly),
				UpdatedAt:   account.Account.Usage.ObservedAt,
			}
			if account.Selectable {
				score := math.Round(account.Score * 100)
				entry.Score = &score
			}
			if account.Active {
				service.Selected = account.Account.Name
			}
			service.Accounts = append(service.Accounts, entry)
		}
		report.Services = append(report.Services, service)
	}
	return report
}

func statusWindow(window LimitWindow) *StatusWindow {
	ratio, ok := window.Ratio()
	if !ok {
		return nil
	}
	return &StatusWindow{UsedPercent: math.Round(ratio*10000) / 100, ResetsAt: window.ResetsAt}
}

// formatAge renders how long ago a usage sample was taken.
func formatAge(at, now time.Time) string {
	if at.IsZero() {
		return "-"
	}
	age := now.Sub(at)
	switch {
	case age < time.Minute:
		return "<1m"
	case age < time.Hour:
		return fmt.Sprintf("%dm", int(age/time.Minute))
	case age < 48*time.Hour:
		return fmt.Sprintf("%dh", int(age/time.Hour))
	default:
		return fmt.Sprintf("%dd", int(age/(24*time.Hour)))
	}
}
