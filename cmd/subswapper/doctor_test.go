package main

import (
	"bytes"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lawzava/subswapper/internal/subswapper"
)

func TestDoctorPassesForAHealthyHubClient(t *testing.T) {
	hub, secret := startClaudeHub(t)
	dir := t.TempDir()
	configPath := writeHubClientConfig(t, dir, "claude", hub.URL, secret)
	var stdout, stderr bytes.Buffer
	if err := run([]string{"doctor", "-config", configPath}, &stdout, &stderr); err != nil {
		t.Fatalf("doctor failed: %v\n%s", err, stdout.String())
	}
	if !strings.Contains(stdout.String(), "ok    claude: hub at "+hub.URL+" accepts this machine") {
		t.Fatalf("doctor output:\n%s", stdout.String())
	}

	hub.Close()
	stdout.Reset()
	err := run([]string{"doctor", "-config", configPath}, &stdout, &stderr)
	if err == nil || !strings.Contains(stdout.String(), "FAIL  claude: hub at "+hub.URL+" did not answer") {
		t.Fatalf("doctor with the hub down: %v\n%s", err, stdout.String())
	}
}

func TestDoctorFlagsARealCodexLoginOnAClient(t *testing.T) {
	hubDir := t.TempDir()
	hubConfig := writeCodexProxyHomeConfig(t, hubDir, "127.0.0.1:1")
	cfg, err := subswapper.LoadConfig(hubConfig)
	if err != nil {
		t.Fatal(err)
	}
	proxy, err := subswapper.NewCodexProxy(*cfg, "codex", nil)
	if err != nil {
		t.Fatal(err)
	}
	placeholder, err := subswapper.LoadOrCreateCodexProxyPlaceholder(*cfg, "codex")
	if err != nil {
		t.Fatal(err)
	}
	hub := httptest.NewServer(proxy)
	t.Cleanup(hub.Close)

	dir := t.TempDir()
	configPath := writeHubClientConfig(t, dir, "codex", hub.URL, placeholder.Token)
	native := filepath.Join(dir, "native-home", ".codex")
	if err := os.MkdirAll(native, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(native, "auth.json"), []byte(`{"auth_mode":"chatgpt","tokens":{"access_token":"real","refresh_token":"real"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	err = run([]string{"doctor", "-config", configPath}, &stdout, &stderr)
	if err == nil || !strings.Contains(stdout.String(), "subswapper home proxy-auth -service codex") {
		t.Fatalf("doctor over a real native login: %v\n%s", err, stdout.String())
	}
}

func TestDoctorWarnsAboutAServiceWithoutAccounts(t *testing.T) {
	dir := t.TempDir()
	configPath := writeAccountsConfig(t, dir)
	var stdout, stderr bytes.Buffer
	if err := run([]string{"doctor", "-config", configPath}, &stdout, &stderr); err != nil {
		t.Fatalf("doctor failed on warnings only: %v\n%s", err, stdout.String())
	}
	if !strings.Contains(stdout.String(), "warn  claude: no accounts; add one with `subswapper add claude <name>`") {
		t.Fatalf("doctor output:\n%s", stdout.String())
	}
}
