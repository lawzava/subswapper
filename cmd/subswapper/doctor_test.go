package main

import (
	"bytes"
	"encoding/json"
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

func TestDoctorFollowsPaseoShimsAndFlagsHiddenCodexVersions(t *testing.T) {
	dir := t.TempDir()
	configPath := writeCodexProxyHomeConfig(t, dir, "127.0.0.1:1")
	fake := filepath.Join(dir, "fake", "subswapper")
	if err := os.MkdirAll(filepath.Dir(fake), 0o700); err != nil {
		t.Fatal(err)
	}
	// Like subswapper, the fake acts as Codex when started through a codex link.
	script := "#!/bin/sh\n[ \"$(basename \"$0\")\" = codex ] && exit 1\necho 'subswapper " + subswapper.Version() + " go'\n"
	if err := os.WriteFile(fake, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(dir, "data", "subswapper", "bin")
	if err := os.MkdirAll(bin, 0o700); err != nil {
		t.Fatal(err)
	}
	shim := filepath.Join(bin, "codex")
	if err := os.Symlink(fake, shim); err != nil {
		t.Fatal(err)
	}
	paseo := map[string]any{"providers": map[string]any{
		"codex-subswapper":  map[string]any{"extends": "codex", "label": "Codex", "command": []string{shim}},
		"codex-wrapped":     map[string]any{"extends": "codex", "label": "Codex", "command": []string{fake, "codex"}},
		"claude-subswapper": map[string]any{"extends": "claude", "label": "Claude", "command": []string{fake, "claude"}},
		"codex-gone":        map[string]any{"extends": "codex", "label": "Codex", "command": []string{filepath.Join(bin, "gone", "codex")}},
	}}
	data, err := json.Marshal(paseo)
	if err != nil {
		t.Fatal(err)
	}
	paseoDir := filepath.Join(dir, "native-home", ".paseo")
	if err := os.MkdirAll(paseoDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(paseoDir, "config.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	_ = run([]string{"doctor", "-config", configPath}, &stdout, &stderr)
	out := stdout.String()
	for _, want := range []string{
		"ok    paseo: provider codex-subswapper runs this subswapper (" + shim + ")",
		"warn  paseo: provider codex-wrapped runs " + fake + ", so Paseo reads subswapper's version",
		"ok    paseo: provider claude-subswapper runs this subswapper (" + fake + ")",
		"FAIL  paseo: provider codex-gone runs " + filepath.Join(bin, "gone", "codex") + ", which does not exist",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("doctor output lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "provider claude-subswapper runs "+fake+", so Paseo") {
		t.Fatalf("Claude provider flagged as hiding the Codex version:\n%s", out)
	}
}
