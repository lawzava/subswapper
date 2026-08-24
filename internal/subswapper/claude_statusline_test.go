package subswapper

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestParseClaudeStatusLineFreshUsage(t *testing.T) {
	observedAt := time.Unix(1_738_000_000, 123).UTC()
	input := []byte(`{
		"rate_limits": {
			"five_hour": {"used_percentage": 23.5, "resets_at": 1738003600},
			"seven_day": {"used_percentage": 41.2, "resets_at": 1738604800}
		}
	}`)

	sample, err := ParseClaudeStatusLine(input, observedAt, "revision-2")
	if err != nil {
		t.Fatal(err)
	}
	if !sample.ObservedAt.Equal(observedAt) {
		t.Fatalf("observed at = %s, want %s", sample.ObservedAt, observedAt)
	}
	if sample.TokenRevision != "revision-2" {
		t.Fatalf("token revision = %q", sample.TokenRevision)
	}
	if got := sample.Usage.FiveHour.Pct; got == nil || *got != 23.5 {
		t.Fatalf("five-hour percentage = %v", got)
	}
	if got := sample.Usage.Weekly.Pct; got == nil || *got != 41.2 {
		t.Fatalf("weekly percentage = %v", got)
	}
	if sample.Usage.FiveHour.ResetsAt.Unix() != 1_738_003_600 {
		t.Fatalf("five-hour reset = %s", sample.Usage.FiveHour.ResetsAt)
	}
	if sample.Usage.Weekly.ResetsAt.Unix() != 1_738_604_800 {
		t.Fatalf("weekly reset = %s", sample.Usage.Weekly.ResetsAt)
	}
	if sample.Usage.FableWeekly.Pct != nil {
		t.Fatalf("Fable usage unexpectedly available: %#v", sample.Usage.FableWeekly)
	}
}

func TestRunClaudeStatusLineCommandPreservesStdinAndStdoutBytes(t *testing.T) {
	input := []byte{'f', 'i', 'r', 's', 't', '\n', 0, 's', 'e', 'c', 'o', 'n', 'd'}
	var stdout bytes.Buffer
	if err := RunClaudeStatusLineCommand(context.Background(), "cat", input, &stdout); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(stdout.Bytes(), input) {
		t.Fatalf("forwarded output differs:\n got: %q\nwant: %q", stdout.Bytes(), input)
	}
}

func TestRunClaudeStatusLineCommandScrubsParentCredentials(t *testing.T) {
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "setup-secret-must-not-reach-statusline")
	t.Setenv("ANTHROPIC_API_KEY", "api-secret-must-not-reach-statusline")
	t.Setenv("SUBSWAPPER_TOKEN_REVISION", "routing-metadata-must-not-reach-statusline")
	var stdout bytes.Buffer
	command := `printf '%s|%s|%s' "${CLAUDE_CODE_OAUTH_TOKEN-unset}" "${ANTHROPIC_API_KEY-unset}" "${SUBSWAPPER_TOKEN_REVISION-unset}"`
	if err := RunClaudeStatusLineCommand(context.Background(), command, nil, &stdout); err != nil {
		t.Fatal(err)
	}
	if got := stdout.String(); got != "unset|unset|unset" {
		t.Fatalf("status-line environment = %q", got)
	}
}

func TestRunClaudeStatusLineCommandForwardsOutputBeforeFailureWithoutExposingCommand(t *testing.T) {
	secret := "setup-secret-must-not-appear"
	var stdout bytes.Buffer
	err := RunClaudeStatusLineCommand(
		context.Background(),
		"printf retained; printf "+secret+" >&2; exit 7",
		[]byte(`{"untrusted":"`+secret+`"}`),
		&stdout,
	)
	if err == nil {
		t.Fatal("expected command failure")
	}
	if stdout.String() != "retained" {
		t.Fatalf("stdout = %q", stdout.String())
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("error exposed command data: %v", err)
	}
}

func TestResolveClaudeStatusLineCommandUsesLocalProjectUserPrecedence(t *testing.T) {
	root := t.TempDir()
	accountHome := filepath.Join(root, "account")
	workspace := filepath.Join(root, "workspace")
	writeClaudeSettings(t, filepath.Join(accountHome, "settings.json"), `{"statusLine":{"type":"command","command":"printf user"}}`)
	writeClaudeSettings(t, filepath.Join(workspace, ".claude", "settings.json"), `{"statusLine":{"type":"command","command":"printf project"}}`)
	writeClaudeSettings(t, filepath.Join(workspace, ".claude", "settings.local.json"), `{"statusLine":{"type":"command","command":"printf local"}}`)

	command, found, err := ResolveClaudeStatusLineCommand(accountHome, workspace)
	if err != nil {
		t.Fatal(err)
	}
	if !found || command != "printf local" {
		t.Fatalf("resolved command = %q, %v", command, found)
	}
}

func TestResolveClaudeStatusLineCommandFallsBackWithoutWritingSettings(t *testing.T) {
	root := t.TempDir()
	accountHome := filepath.Join(root, "account")
	workspace := filepath.Join(root, "workspace")
	userPath := filepath.Join(accountHome, "settings.json")
	projectPath := filepath.Join(workspace, ".claude", "settings.json")
	userSettings := []byte(`{"statusLine":{"type":"command","command":"printf user"},"theme":"dark"}`)
	projectSettings := []byte(`{"permissions":{"allow":[]}}`)
	writeClaudeSettings(t, userPath, string(userSettings))
	writeClaudeSettings(t, projectPath, string(projectSettings))

	command, found, err := ResolveClaudeStatusLineCommand(accountHome, workspace)
	if err != nil {
		t.Fatal(err)
	}
	if !found || command != "printf user" {
		t.Fatalf("resolved command = %q, %v", command, found)
	}
	assertFileBytes(t, userPath, userSettings)
	assertFileBytes(t, projectPath, projectSettings)
}

func TestResolveClaudeStatusLineCommandReportsMalformedSettingsWithoutCommandData(t *testing.T) {
	root := t.TempDir()
	accountHome := filepath.Join(root, "account")
	secretCommand := "printf setup-secret-must-not-appear"
	writeClaudeSettings(t, filepath.Join(accountHome, "settings.json"), `{"statusLine":{"type":"command","command":`+secretCommand)

	_, _, err := ResolveClaudeStatusLineCommand(accountHome, filepath.Join(root, "workspace"))
	if err == nil {
		t.Fatal("expected malformed settings error")
	}
	if strings.Contains(err.Error(), secretCommand) || strings.Contains(err.Error(), "setup-secret") {
		t.Fatalf("error exposed settings data: %v", err)
	}
}

func writeClaudeSettings(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
}

func assertFileBytes(t *testing.T, path string, want []byte) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(want) {
		t.Fatalf("settings changed:\n got: %s\nwant: %s", got, want)
	}
}

func TestParseClaudeStatusLineRejectsUnavailablePartialStaleAndMalformedUsage(t *testing.T) {
	observedAt := time.Unix(1_000, 0).UTC()
	tests := []struct {
		name  string
		input string
	}{
		{name: "absent", input: `{}`},
		{name: "partial", input: `{"rate_limits":{"five_hour":{"used_percentage":10,"resets_at":2000}}}`},
		{name: "missing percentage", input: `{"rate_limits":{"five_hour":{"resets_at":2000},"seven_day":{"used_percentage":20,"resets_at":3000}}}`},
		{name: "null percentage", input: `{"rate_limits":{"five_hour":{"used_percentage":null,"resets_at":2000},"seven_day":{"used_percentage":20,"resets_at":3000}}}`},
		{name: "null reset", input: `{"rate_limits":{"five_hour":{"used_percentage":10,"resets_at":null},"seven_day":{"used_percentage":20,"resets_at":3000}}}`},
		{name: "expired reset", input: `{"rate_limits":{"five_hour":{"used_percentage":10,"resets_at":1000},"seven_day":{"used_percentage":20,"resets_at":3000}}}`},
		{name: "negative percentage", input: `{"rate_limits":{"five_hour":{"used_percentage":-1,"resets_at":2000},"seven_day":{"used_percentage":20,"resets_at":3000}}}`},
		{name: "oversized percentage", input: `{"rate_limits":{"five_hour":{"used_percentage":101,"resets_at":2000},"seven_day":{"used_percentage":20,"resets_at":3000}}}`},
		{name: "non-finite percentage", input: `{"rate_limits":{"five_hour":{"used_percentage":1e1000,"resets_at":2000},"seven_day":{"used_percentage":20,"resets_at":3000}}}`},
		{name: "fractional reset", input: `{"rate_limits":{"five_hour":{"used_percentage":10,"resets_at":2000.5},"seven_day":{"used_percentage":20,"resets_at":3000}}}`},
		{name: "malformed", input: `{"rate_limits":`},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			secret := "setup-secret-must-not-appear"
			input := []byte(test.input + " " + secret)
			if test.input == `{}` {
				input = []byte(`{"untrusted":"` + secret + `"}`)
			} else if test.name != "malformed" {
				input = []byte(strings.TrimSuffix(test.input, "}") + `,"untrusted":"` + secret + `"}`)
			}
			_, err := ParseClaudeStatusLine(input, observedAt, "revision-1")
			if err == nil {
				t.Fatal("expected rejection")
			}
			if strings.Contains(err.Error(), secret) {
				t.Fatalf("error exposed input data: %v", err)
			}
		})
	}
}

func TestClaudeStatusLineSampleRoutingUsage(t *testing.T) {
	observedAt := time.Unix(1_000, 0).UTC()
	sample, err := ParseClaudeStatusLine([]byte(`{
		"rate_limits": {
			"five_hour": {"used_percentage": 10, "resets_at": 2000},
			"seven_day": {"used_percentage": 20, "resets_at": 3000}
		}
	}`), observedAt, "revision-1")
	if err != nil {
		t.Fatal(err)
	}

	if usage, ok := sample.RoutingUsage(observedAt.Add(29*time.Second), "revision-1", 30*time.Second); !ok || !usage.HasCoreLimits() {
		t.Fatalf("fresh usage rejected: %#v, %v", usage, ok)
	}
	tampered := sample
	tampered.Usage.FableWeekly = LimitWindow{Pct: PtrFloat64(99), ResetsAt: time.Unix(3_000, 0).UTC()}
	if usage, ok := tampered.RoutingUsage(observedAt.Add(time.Second), "revision-1", 30*time.Second); !ok || usage.FableWeekly.Pct != nil {
		t.Fatalf("status-line routing exposed unsupported Fable usage: %#v, %v", usage, ok)
	}
	checks := []struct {
		name     string
		now      time.Time
		revision string
		maxAge   time.Duration
	}{
		{name: "stale", now: observedAt.Add(30 * time.Second), revision: "revision-1", maxAge: 30 * time.Second},
		{name: "revision changed", now: observedAt.Add(time.Second), revision: "revision-2", maxAge: 30 * time.Second},
		{name: "unknown revision", now: observedAt.Add(time.Second), revision: "", maxAge: 30 * time.Second},
		{name: "future observation", now: observedAt.Add(-time.Second), revision: "revision-1", maxAge: 30 * time.Second},
		{name: "invalid max age", now: observedAt, revision: "revision-1", maxAge: 0},
		{name: "reset passed", now: time.Unix(2_000, 0).UTC(), revision: "revision-1", maxAge: 2_000 * time.Second},
	}
	for _, check := range checks {
		t.Run(check.name, func(t *testing.T) {
			if usage, ok := sample.RoutingUsage(check.now, check.revision, check.maxAge); ok || usage.HasAnyLimit() {
				t.Fatalf("untrusted usage accepted: %#v", usage)
			}
		})
	}
}
