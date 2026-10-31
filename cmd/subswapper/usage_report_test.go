package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lawzava/subswapper/internal/subswapper"
)

func writeUsageHistoryFixture(t *testing.T, configPath string) {
	t.Helper()
	cfg, err := subswapper.LoadConfig(configPath)
	if err != nil {
		t.Fatal(err)
	}
	var lines bytes.Buffer
	now := time.Now().UTC().Truncate(time.Hour)
	// Ten days of hourly samples for two accounts in one weekly window that
	// resets 3 days ago and another that is still running.
	for hour := 10 * 24; hour >= 0; hour-- {
		at := now.Add(-time.Duration(hour) * time.Hour)
		reset := now.Add(4 * 24 * time.Hour)
		pct := 30.0
		if at.Before(now.Add(-3 * 24 * time.Hour)) {
			reset = now.Add(-3 * 24 * time.Hour)
			pct = 50
		}
		for _, name := range []string{"a", "b"} {
			line, err := json.Marshal(subswapper.UsageHistoryRecord{
				Time: at, Type: subswapper.HistoryTypeSample, Service: "claude", Account: name,
				Active: name == "a", Selectable: true, State: "ready",
				Weekly: &subswapper.HistoryWindow{Pct: pct, ResetsAt: reset},
			})
			if err != nil {
				t.Fatal(err)
			}
			lines.Write(line)
			lines.WriteByte('\n')
		}
	}
	if err := os.WriteFile(subswapper.UsageHistoryPath(*cfg), lines.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestUsageReportPrintsVerdictAndEvidence(t *testing.T) {
	configPath := writeProxyHomeConfig(t, t.TempDir(), "127.0.0.1:1")
	writeUsageHistoryFixture(t, configPath)

	var stdout, stderr bytes.Buffer
	if err := run([]string{"usage-report", "-config", configPath}, &stdout, &stderr); err != nil {
		t.Fatalf("usage-report failed: %v", err)
	}
	for _, want := range []string{"claude: ", "2 accounts", "account-weeks", "not comparable", "50/50"} {
		if !strings.Contains(stdout.String(), want) {
			t.Fatalf("report lacks %q:\n%s", want, stdout.String())
		}
	}
	// Ten days of history is enough for a verdict.
	if strings.Contains(stdout.String(), "insufficient data") {
		t.Fatalf("unexpected insufficient data:\n%s", stdout.String())
	}

	stdout.Reset()
	if err := run([]string{"usage-report", "-config", configPath, "-service", "claude", "-since", "48h", "-json"}, &stdout, &stderr); err != nil {
		t.Fatalf("usage-report -json failed: %v", err)
	}
	var report subswapper.UsageReport
	if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
		t.Fatalf("invalid json: %v\n%s", err, stdout.String())
	}
	if len(report.Services) != 1 || report.Services[0].Verdict != subswapper.VerdictInsufficient {
		t.Fatalf("48h report = %+v", report.Services)
	}
}

func TestUsageReportRejectsUnknownServiceAndBadRange(t *testing.T) {
	configPath := writeProxyHomeConfig(t, t.TempDir(), "127.0.0.1:1")
	var stdout, stderr bytes.Buffer
	if err := run([]string{"usage-report", "-config", configPath, "-service", "nope"}, &stdout, &stderr); err == nil {
		t.Fatal("unknown service succeeded")
	}
	if err := run([]string{"usage-report", "-config", configPath, "-since", "0s"}, &stdout, &stderr); err == nil {
		t.Fatal("zero -since succeeded")
	}
}

func TestUsageReportOnHubClientPointsAtTheHub(t *testing.T) {
	dir := t.TempDir()
	hub, secret := startClaudeHub(t)
	configPath := writeHubClientConfig(t, dir, "claude", hub.URL, secret)
	var stdout, stderr bytes.Buffer
	err := run([]string{"usage-report", "-config", configPath}, &stdout, &stderr)
	if err == nil || !strings.Contains(err.Error(), "run it on the hub") {
		t.Fatalf("usage-report on a client = %v, want a run-it-on-the-hub error", err)
	}
	err = run([]string{"usage-report", "-config", configPath, "-service", "claude"}, &stdout, &stderr)
	if err == nil || !strings.Contains(err.Error(), "run it on the hub") {
		t.Fatalf("usage-report -service on a client = %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(dir, "usage-history.jsonl")); statErr == nil {
		t.Fatal("client created a history file")
	}
}
