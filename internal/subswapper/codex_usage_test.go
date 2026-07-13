package subswapper

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestFetchCodexUsageWithAppServer(t *testing.T) {
	dir := t.TempDir()
	fakeCodex := filepath.Join(dir, "codex")
	if err := os.WriteFile(fakeCodex, []byte(`#!/bin/sh
while IFS= read -r line; do
	case "$line" in
		*'"id":1'*)
			printf '%s\n' '{"id":1,"result":{"userAgent":"test","codexHome":"'$CODEX_HOME'","platformFamily":"unix","platformOs":"linux"}}'
			;;
		*'"id":2'*)
			if [ ! -f "$CODEX_HOME/auth.json" ]; then
				printf '%s\n' '{"id":2,"error":{"code":-32000,"message":"missing auth"}}'
				exit 0
			fi
			printf '%s\n' '{"id":2,"result":{"rateLimits":{"limitId":"codex","primary":{"usedPercent":12,"windowDurationMins":300,"resetsAt":1909954910},"secondary":{"usedPercent":6,"windowDurationMins":10080,"resetsAt":1910414767},"planType":"pro","rateLimitReachedType":null},"rateLimitsByLimitId":{"codex":{"limitId":"codex","primary":{"usedPercent":12,"windowDurationMins":300,"resetsAt":1909954910},"secondary":{"usedPercent":6,"windowDurationMins":10080,"resetsAt":1910414767},"planType":"pro","rateLimitReachedType":null}}}}'
			exit 0
			;;
	esac
done
`), 0o700); err != nil {
		t.Fatal(err)
	}
	oldCommand := codexCommand
	codexCommand = fakeCodex
	t.Cleanup(func() { codexCommand = oldCommand })

	cfg := Config{
		BackupRoot: filepath.Join(dir, "accounts"),
		StatePath:  filepath.Join(dir, "state.json"),
		Services: []ServiceConfig{
			{Name: "codex", Kind: "codex", Files: []ManagedFile{requiredFile(filepath.Join(dir, "active-auth.json"), "auth.json")}},
		},
	}
	accountDir := AccountDir(cfg, "codex", "main")
	if err := os.MkdirAll(accountDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(accountDir, "auth.json"), []byte(`{"auth_mode":"chatgpt","tokens":{"access_token":"access","refresh_token":"refresh"}}`), 0o600); err != nil {
		t.Fatal(err)
	}

	usage, err := fetchCodexUsage(testContext(t), cfg, cfg.Services[0], AccountState{Name: "main"}, false)
	if err != nil {
		t.Fatal(err)
	}
	fiveHour, ok := usage.FiveHour.Ratio()
	if !ok || fiveHour != 0.12 {
		t.Fatalf("unexpected five-hour ratio %v %v", fiveHour, ok)
	}
	weekly, ok := usage.Weekly.Ratio()
	if !ok || weekly != 0.06 {
		t.Fatalf("unexpected weekly ratio %v %v", weekly, ok)
	}
	if want := time.Unix(1909954910, 0).UTC(); !usage.FiveHour.ResetsAt.Equal(want) {
		t.Fatalf("unexpected reset %s", usage.FiveHour.ResetsAt)
	}
}

func TestFetchCodexUsageAcceptsWeeklyOnly(t *testing.T) {
	dir := t.TempDir()
	fakeCodex := filepath.Join(dir, "codex")
	if err := os.WriteFile(fakeCodex, []byte(`#!/bin/sh
while IFS= read -r line; do
	case "$line" in
		*'"id":1'*)
			printf '%s\n' '{"id":1,"result":{"userAgent":"test"}}'
			;;
		*'"id":2'*)
			printf '%s\n' '{"id":2,"result":{"rateLimits":{"limitId":"codex","primary":{"usedPercent":3,"windowDurationMins":10080,"resetsAt":1910414767},"secondary":null}}}'
			exit 0
			;;
	esac
done
`), 0o700); err != nil {
		t.Fatal(err)
	}
	oldCommand := codexCommand
	codexCommand = fakeCodex
	t.Cleanup(func() { codexCommand = oldCommand })

	cfg := Config{
		BackupRoot: filepath.Join(dir, "accounts"),
		StatePath:  filepath.Join(dir, "state.json"),
		Services: []ServiceConfig{
			{Name: "codex", Kind: "codex", Files: []ManagedFile{requiredFile(filepath.Join(dir, "active-auth.json"), "auth.json")}},
		},
	}
	accountDir := AccountDir(cfg, "codex", "main")
	if err := os.MkdirAll(accountDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(accountDir, "auth.json"), []byte(`{"auth_mode":"chatgpt","tokens":{"access_token":"access","account_id":"account"}}`), 0o600); err != nil {
		t.Fatal(err)
	}

	usage, err := fetchCodexUsage(testContext(t), cfg, cfg.Services[0], AccountState{Name: "main"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := usage.FiveHour.Ratio(); ok {
		t.Fatal("expected no five-hour window")
	}
	weekly, ok := usage.Weekly.Ratio()
	if !ok || weekly != 0.03 {
		t.Fatalf("unexpected weekly ratio %v %v", weekly, ok)
	}
	if score := usage.Score(); score != 0.03 {
		t.Fatalf("unexpected score %v", score)
	}
}

func TestValidateCodexAuthRejectsAPIKeyMode(t *testing.T) {
	err := validateCodexAuth([]byte(`{"auth_mode":"api_key","OPENAI_API_KEY":"key"}`))
	if err == nil || !strings.Contains(err.Error(), "no subscription limits") {
		t.Fatalf("expected subscription limit error, got %v", err)
	}
}

func TestConvertCodexRateLimitReachedMarksUsageExhausted(t *testing.T) {
	usage := convertCodexRateLimits(codexRateLimitsResponse{
		RateLimits: codexRateLimitSnapshot{
			Primary:              &codexRateLimitWindow{UsedPercent: 20},
			Secondary:            &codexRateLimitWindow{UsedPercent: 30},
			RateLimitReachedType: "rate_limit_reached",
		},
	})
	if !usage.Exhausted() {
		t.Fatal("expected reached Codex rate limit to be exhausted")
	}
}

func TestEnvWithOverrideReplacesExistingValue(t *testing.T) {
	got := envWithOverride([]string{"A=1", "CODEX_HOME=/old", "B=2", "CODEX_HOME=/older"}, "CODEX_HOME", "/new")
	want := []string{"A=1", "CODEX_HOME=/new", "B=2"}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("unexpected env:\n%s", strings.Join(got, "\n"))
	}
}
