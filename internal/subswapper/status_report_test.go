package subswapper

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func statusReportFixture(now time.Time) []ServiceStatus {
	weekly := 86.0
	fiveHour := 15.0
	return []ServiceStatus{
		{
			Service: ServiceConfig{Name: "claude", Kind: "claude", HubURL: "http://100.64.0.1:7878"},
			Accounts: []AccountStatus{
				{
					Service: "claude",
					Account: AccountState{Name: "foxy2", Email: "f@example.com", Usage: UsageSnapshot{
						FiveHour:   LimitWindow{Pct: &fiveHour, ResetsAt: now.Add(4 * time.Hour)},
						Weekly:     LimitWindow{Pct: &weekly, ResetsAt: now.Add(48 * time.Hour)},
						ObservedAt: now.Add(-3 * time.Minute),
					}},
					Active: true, Selectable: true, Score: 0.86, Reason: "ready",
				},
				{Service: "claude", Account: AccountState{Name: "h2"}, Reason: "usage unavailable"},
			},
		},
		{Service: ServiceConfig{Name: "codex", Kind: "codex"}, Note: "hub at x unavailable"},
	}
}

func TestBuildStatusReport(t *testing.T) {
	now := time.Date(2026, 10, 3, 10, 0, 0, 0, time.UTC)
	report := BuildStatusReport(statusReportFixture(now), now)
	data, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	var decoded struct {
		GeneratedAt time.Time `json:"generated_at"`
		Services    []struct {
			Name     string `json:"name"`
			Kind     string `json:"kind"`
			Hub      string `json:"hub"`
			Note     string `json:"note"`
			Selected string `json:"selected"`
			Accounts []struct {
				Name     string   `json:"name"`
				Email    string   `json:"email"`
				Selected bool     `json:"selected"`
				Ready    bool     `json:"ready"`
				Score    *float64 `json:"score"`
				State    string   `json:"state"`
				Weekly   *struct {
					UsedPercent float64   `json:"used_percent"`
					ResetsAt    time.Time `json:"resets_at"`
				} `json:"weekly"`
				FiveHour    *struct{} `json:"five_hour"`
				FableWeekly *struct{} `json:"fable_weekly"`
				UpdatedAt   time.Time `json:"updated_at"`
			} `json:"accounts"`
		} `json:"services"`
	}
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatal(err)
	}
	if !decoded.GeneratedAt.Equal(now) || len(decoded.Services) != 2 {
		t.Fatalf("report = %s", data)
	}
	claude := decoded.Services[0]
	if claude.Name != "claude" || claude.Kind != "claude" || claude.Hub != "http://100.64.0.1:7878" || claude.Selected != "foxy2" || len(claude.Accounts) != 2 {
		t.Fatalf("claude = %s", data)
	}
	foxy := claude.Accounts[0]
	if !foxy.Selected || !foxy.Ready || foxy.Score == nil || *foxy.Score != 86 || foxy.State != "ready" ||
		foxy.Weekly == nil || foxy.Weekly.UsedPercent != 86 || !foxy.Weekly.ResetsAt.Equal(now.Add(48*time.Hour)) ||
		foxy.FiveHour == nil || foxy.FableWeekly != nil || !foxy.UpdatedAt.Equal(now.Add(-3*time.Minute)) {
		t.Fatalf("foxy2 = %s", data)
	}
	h2 := claude.Accounts[1]
	if h2.Ready || h2.Score != nil || h2.Weekly != nil || !h2.UpdatedAt.IsZero() || h2.State != "usage unavailable" {
		t.Fatalf("h2 = %s", data)
	}
	if codex := decoded.Services[1]; codex.Note != "hub at x unavailable" || codex.Accounts == nil {
		t.Fatalf("codex = %s; accounts must be an empty list, not null", data)
	}
}

func TestRenderStatusShowsUsageAge(t *testing.T) {
	now := time.Now()
	out := RenderStatus(statusReportFixture(now), nil, now)
	if !strings.Contains(out, "UPDATED") {
		t.Fatalf("no UPDATED column:\n%s", out)
	}
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "foxy2") && !strings.Contains(line, " 3m ") {
			t.Fatalf("foxy2 row lacks its 3m age: %q", line)
		}
	}
	for age, want := range map[time.Duration]string{
		20 * time.Second: "<1m", 3 * time.Minute: "3m", 5 * time.Hour: "5h", 50 * time.Hour: "2d",
	} {
		if got := formatAge(now.Add(-age), now); got != want {
			t.Fatalf("formatAge(%v) = %q, want %q", age, got, want)
		}
	}
	if got := formatAge(time.Time{}, now); got != "-" {
		t.Fatalf("formatAge(zero) = %q", got)
	}
}
