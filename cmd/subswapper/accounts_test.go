package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lawzava/subswapper/internal/subswapper"
)

func stubClaudeIdentity(t *testing.T) {
	t.Helper()
	original := lookupClaudeSetupTokenIdentity
	lookupClaudeSetupTokenIdentity = func(_ context.Context, token string) (subswapper.ClaudeSetupTokenIdentity, error) {
		return subswapper.ClaudeSetupTokenIdentity{AccountUUID: "uuid-" + token[len(token)-4:]}, nil
	}
	t.Cleanup(func() { lookupClaudeSetupTokenIdentity = original })
}

// writeAccountsConfig makes a standalone machine with Claude and Codex homes.
func writeAccountsConfig(t *testing.T, dir string) string {
	t.Helper()
	t.Setenv("HOME", filepath.Join(dir, "native-home"))
	t.Setenv("CODEX_HOME", "")
	path := filepath.Join(dir, "config.json")
	data := `{"backup_root":"` + filepath.Join(dir, "accounts") + `","state_path":"` + filepath.Join(dir, "state.json") +
		`","services":[{"name":"claude","kind":"claude"},{"name":"codex","kind":"codex"}]}`
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func fakeCodexLogin(email, access string) string {
	claims := base64.RawURLEncoding.EncodeToString([]byte(`{"email":"` + email + `"}`))
	return `{"auth_mode":"chatgpt","tokens":{"id_token":"e30.` + claims + `.sig","access_token":"` + access +
		`","refresh_token":"r-` + access + `","account_id":"a-` + access + `"}}`
}

// installFakeCodexLogin puts a codex on PATH whose login writes login into
// CODEX_HOME and records its arguments and home in record.
func installFakeCodexLogin(t *testing.T, dir, login string) string {
	t.Helper()
	bin := filepath.Join(dir, "bin")
	if err := os.MkdirAll(bin, 0o700); err != nil {
		t.Fatal(err)
	}
	record := filepath.Join(dir, "codex-record")
	script := "#!/bin/sh\nprintf 'args=%s\\nhome=%s\\n' \"$*\" \"$CODEX_HOME\" > '" + record + "'\n" +
		"cat > \"$CODEX_HOME/auth.json\" <<'EOF'\n" + login + "\nEOF\n"
	if err := os.WriteFile(filepath.Join(bin, "codex"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	return record
}

func TestAddAccountsLocally(t *testing.T) {
	stubClaudeIdentity(t)
	dir := t.TempDir()
	configPath := writeAccountsConfig(t, dir)

	var stdout, stderr bytes.Buffer
	if err := runWithInput([]string{"add", "-config", configPath, "claude", "work", "-email", "w@example.com"},
		strings.NewReader("sk-ant-oat01-work\n"), &stdout, &stderr); err != nil {
		t.Fatalf("add claude: %v; stderr=%s", err, stderr.String())
	}
	if !strings.Contains(stdout.String(), "added claude account work") {
		t.Fatalf("add claude output = %q", stdout.String())
	}
	cfg, err := subswapper.LoadConfig(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if token, status, err := subswapper.LoadClaudeSetupTokenWithStatus(*cfg, "claude", "work"); err != nil || !status.Usable || token != "sk-ant-oat01-work" {
		t.Fatalf("stored token = %q, %#v, %v", token, status, err)
	}

	record := installFakeCodexLogin(t, dir, fakeCodexLogin("c@example.com", "one"))
	stdout.Reset()
	if err := runWithInput([]string{"add", "-config", configPath, "codex", "main", "-device"}, strings.NewReader(""), &stdout, &stderr); err != nil {
		t.Fatalf("add codex: %v; stderr=%s", err, stderr.String())
	}
	if !strings.Contains(stdout.String(), "added codex account main (c@example.com)") {
		t.Fatalf("add codex output = %q", stdout.String())
	}
	recorded, err := os.ReadFile(record)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(recorded), `login --device-auth`) || !strings.Contains(string(recorded), `cli_auth_credentials_store="file"`) {
		t.Fatalf("codex login invocation = %s", recorded)
	}
	loginHome := strings.TrimSpace(strings.SplitN(string(recorded), "home=", 2)[1])
	if _, err := os.Stat(loginHome); !os.IsNotExist(err) {
		t.Fatalf("temporary login home %s was left behind: %v", loginHome, err)
	}
	stored, err := os.ReadFile(filepath.Join(subswapper.AccountDir(*cfg, "codex", "main"), "auth.json"))
	if err != nil || !strings.Contains(string(stored), `"access_token":"one"`) {
		t.Fatalf("codex account login = %s, %v", stored, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "native-home", ".codex", "auth.json")); !os.IsNotExist(err) {
		t.Fatalf("add touched the native Codex login: %v", err)
	}

	// Positional switch and remove; remove also drops the Claude token.
	if err := runWithInput([]string{"add", "-config", configPath, "claude", "other"}, strings.NewReader("sk-ant-oat01-othr\n"), &stdout, &stderr); err != nil {
		t.Fatal(err)
	}
	stdout.Reset()
	if err := run([]string{"switch", "-config", configPath, "claude", "other"}, &stdout, &stderr); err != nil || !strings.Contains(stdout.String(), "switched claude to other") {
		t.Fatalf("switch: %v; %q", err, stdout.String())
	}
	if err := run([]string{"remove", "-config", configPath, "claude", "work"}, &stdout, &stderr); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if _, status, _ := subswapper.LoadClaudeSetupTokenWithStatus(*cfg, "claude", "work"); status.Configured {
		t.Fatal("remove kept the setup token")
	}
}

func TestAccountCommandsOnHubClientReachTheHub(t *testing.T) {
	stubClaudeIdentity(t)
	hubDir := t.TempDir()
	hubConfig := writeAccountsConfig(t, hubDir)
	data, err := os.ReadFile(hubConfig)
	if err != nil {
		t.Fatal(err)
	}
	data = bytes.Replace(data, []byte(`"kind":"claude"`), []byte(`"kind":"claude","proxy_listen":"127.0.0.1:1"`), 1)
	data = bytes.Replace(data, []byte(`"kind":"codex"`), []byte(`"kind":"codex","proxy_listen":"127.0.0.1:2"`), 1)
	if err := os.WriteFile(hubConfig, data, 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := subswapper.LoadConfig(hubConfig)
	if err != nil {
		t.Fatal(err)
	}
	claudeProxy, err := subswapper.NewClaudeProxy(*cfg, "claude", nil)
	if err != nil {
		t.Fatal(err)
	}
	claudeProxy.IdentityLookup = lookupClaudeSetupTokenIdentity
	codexProxy, err := subswapper.NewCodexProxy(*cfg, "codex", nil)
	if err != nil {
		t.Fatal(err)
	}
	claudeHub := httptest.NewServer(claudeProxy)
	t.Cleanup(claudeHub.Close)
	codexHub := httptest.NewServer(codexProxy)
	t.Cleanup(codexHub.Close)
	secret, err := subswapper.LoadOrCreateClaudeProxySecret(*cfg, "claude")
	if err != nil {
		t.Fatal(err)
	}
	placeholder, err := subswapper.LoadOrCreateCodexProxyPlaceholder(*cfg, "codex")
	if err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	t.Setenv("HOME", filepath.Join(dir, "native-home"))
	configPath := filepath.Join(dir, "config.json")
	if err := os.WriteFile(configPath, []byte(`{"state_path":"`+filepath.Join(dir, "state.json")+`","services":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := subswapper.ImportHubBundle(configPath, subswapper.HubBundle{Version: 1, Services: []subswapper.HubBundleService{
		{Name: "claude", Kind: "claude", HubURL: claudeHub.URL, Secret: secret},
		{Name: "codex", Kind: "codex", HubURL: codexHub.URL, Secret: placeholder.Token},
	}}); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	if err := runWithInput([]string{"add", "-config", configPath, "claude", "work"}, strings.NewReader("sk-ant-oat01-work\n"), &stdout, &stderr); err != nil {
		t.Fatalf("add claude via hub: %v; stderr=%s", err, stderr.String())
	}
	if !strings.Contains(stdout.String(), "added claude account work on the hub at "+claudeHub.URL) {
		t.Fatalf("output = %q", stdout.String())
	}
	if token, status, err := subswapper.LoadClaudeSetupTokenWithStatus(*cfg, "claude", "work"); err != nil || !status.Usable || token != "sk-ant-oat01-work" {
		t.Fatalf("hub token = %q, %#v, %v", token, status, err)
	}

	record := installFakeCodexLogin(t, dir, fakeCodexLogin("c@example.com", "hubbed"))
	stdout.Reset()
	if err := runWithInput([]string{"add", "-config", configPath, "codex", "main"}, strings.NewReader(""), &stdout, &stderr); err != nil {
		t.Fatalf("add codex via hub: %v; stderr=%s", err, stderr.String())
	}
	stored, err := os.ReadFile(filepath.Join(subswapper.AccountDir(*cfg, "codex", "main"), "auth.json"))
	if err != nil || !strings.Contains(string(stored), `"access_token":"hubbed"`) {
		t.Fatalf("hub codex login = %s, %v", stored, err)
	}
	recorded, _ := os.ReadFile(record)
	loginHome := strings.TrimSpace(strings.SplitN(string(recorded), "home=", 2)[1])
	if _, err := os.Stat(loginHome); !os.IsNotExist(err) {
		t.Fatal("the client kept a copy of the uploaded login")
	}

	if err := runWithInput([]string{"add", "-config", configPath, "claude", "second"}, strings.NewReader("sk-ant-oat01-scnd\n"), &stdout, &stderr); err != nil {
		t.Fatal(err)
	}
	stdout.Reset()
	if err := run([]string{"switch", "-config", configPath, "claude", "second"}, &stdout, &stderr); err != nil || !strings.Contains(stdout.String(), "switched claude to second on the hub") {
		t.Fatalf("switch via hub: %v; %q", err, stdout.String())
	}
	if state, _ := subswapper.LoadState(cfg.StatePath); state.Service("claude").ActiveAccount != "second" {
		t.Fatal("hub selection did not change")
	}
	if err := run([]string{"remove", "-config", configPath, "claude", "work"}, &stdout, &stderr); err != nil {
		t.Fatalf("remove via hub: %v", err)
	}
	if state, _ := subswapper.LoadState(cfg.StatePath); state.Service("claude").Accounts["work"].Name != "" {
		t.Fatal("hub kept the removed account")
	}

	pauseProbes(t, *cfg)
	stdout.Reset()
	if err := run([]string{"status", "-config", configPath, "-json"}, &stdout, &stderr); err != nil {
		t.Fatalf("status -json: %v", err)
	}
	var report subswapper.StatusReport
	if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
		t.Fatalf("status -json output: %v\n%s", err, stdout.String())
	}
	if len(report.Services) != 2 || report.Services[0].Hub != claudeHub.URL || report.Services[0].Selected != "second" {
		t.Fatalf("report = %#v", report)
	}
}

func TestProviderShortcutRunsThroughHomeRun(t *testing.T) {
	hub, secret := startClaudeHub(t)
	dir := t.TempDir()
	configPath := writeHubClientConfig(t, dir, "claude", hub.URL, secret)
	fake := writeFakeClaude(t, dir, fakeClaudeEnvReport)
	t.Setenv("PATH", filepath.Dir(fake)+string(os.PathListSeparator)+os.Getenv("PATH"))
	var stdout, stderr bytes.Buffer
	if err := runWithInput([]string{"claude", "-config", configPath, "--", "-p"}, strings.NewReader(""), &stdout, &stderr); err != nil {
		t.Fatalf("subswapper claude: %v; stderr=%s", err, stderr.String())
	}
	if !strings.Contains(stdout.String(), "proxy=1") {
		t.Fatalf("shortcut output = %q", stdout.String())
	}
}

// pauseProbes puts every account into probe backoff so a status run makes no
// network calls.
func pauseProbes(t *testing.T, cfg subswapper.Config) {
	t.Helper()
	state, err := subswapper.LoadState(cfg.StatePath)
	if err != nil {
		t.Fatal(err)
	}
	for _, service := range state.Services {
		for name, account := range service.Accounts {
			account.FetchBackoffUntil = time.Now().Add(time.Hour)
			account.CredentialsError = "probe paused for test"
			service.Accounts[name] = account
		}
	}
	if err := subswapper.SaveState(cfg.StatePath, state); err != nil {
		t.Fatal(err)
	}
}
