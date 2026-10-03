package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lawzava/subswapper/internal/subswapper"
)

// writeHubClientConfig makes dir a hub client of hubURL for one service and
// stores the hub's credential the way `hub import` would.
func writeHubClientConfig(t *testing.T, dir, kind, hubURL, secret string) string {
	t.Helper()
	t.Setenv("HOME", filepath.Join(dir, "native-home"))
	t.Setenv("CODEX_HOME", "")
	configPath := filepath.Join(dir, "config.json")
	if err := os.WriteFile(configPath, []byte(`{"state_path":"`+filepath.Join(dir, "state.json")+`","services":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	bundle := subswapper.HubBundle{Version: 1, Services: []subswapper.HubBundleService{{Name: kind, Kind: kind, HubURL: hubURL, Secret: secret}}}
	if _, err := subswapper.ImportHubBundle(configPath, bundle); err != nil {
		t.Fatal(err)
	}
	return configPath
}

func startClaudeHub(t *testing.T) (*httptest.Server, string) {
	t.Helper()
	dir := t.TempDir()
	configPath := writeProxyHomeConfig(t, dir, "127.0.0.1:1")
	createHomeAccount(t, configPath, "claude", "work")
	storeTestSetupToken(t, configPath, "work", "sk-ant-oat01-real-account-token")
	cfg, err := subswapper.LoadConfig(configPath)
	if err != nil {
		t.Fatal(err)
	}
	proxy, err := subswapper.NewClaudeProxy(*cfg, "claude", nil)
	if err != nil {
		t.Fatal(err)
	}
	secret, err := subswapper.LoadOrCreateClaudeProxySecret(*cfg, "claude")
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(proxy)
	t.Cleanup(server.Close)
	return server, secret
}

func TestRunHomeClaudeThroughHubRelay(t *testing.T) {
	hub, secret := startClaudeHub(t)
	dir := t.TempDir()
	configPath := writeHubClientConfig(t, dir, "claude", hub.URL, secret)
	fakeClaude := writeFakeClaude(t, dir, fakeClaudeEnvReport)

	var stdout, stderr bytes.Buffer
	if err := runWithInput(
		[]string{"home", "run", "-config", configPath, "-service", "claude", "--", fakeClaude, "-p"},
		strings.NewReader(""), &stdout, &stderr,
	); err != nil {
		t.Fatalf("home run failed: %v; stderr=%s", err, stderr.String())
	}
	got := stdout.String()
	// The process talks to a loopback relay, never to the hub directly.
	for _, want := range []string{"token=[REDACTED]", "base=http://127.0.0.1:", "firstparty=1", "proxy=1"} {
		if !strings.Contains(got, want) {
			t.Fatalf("hub launch output lacks %q:\n%s%s", want, got, stderr.String())
		}
	}
	if strings.Contains(got, hub.URL) || strings.Contains(got+stderr.String(), "real-account-token") {
		t.Fatalf("hub launch leaked the hub address or a real token:\n%s", got)
	}

	// -account cannot pin a route that lives on another machine.
	err := runWithInput(
		[]string{"home", "run", "-config", configPath, "-service", "claude", "-account", "work", "--", fakeClaude},
		strings.NewReader(""), &stdout, &stderr,
	)
	if err == nil || !strings.Contains(err.Error(), "hub") {
		t.Fatalf("pinned account on a hub client: err = %v", err)
	}
}

func TestRunHomeThroughHubRefusesDirectLaunchWhenHubIsDown(t *testing.T) {
	hub, secret := startClaudeHub(t)
	hub.Close()
	dir := t.TempDir()
	configPath := writeHubClientConfig(t, dir, "claude", hub.URL, secret)
	fakeClaude := writeFakeClaude(t, dir, fakeClaudeEnvReport)

	var stdout, stderr bytes.Buffer
	err := runWithInput(
		[]string{"home", "run", "-config", configPath, "-service", "claude", "--", fakeClaude, "-p"},
		strings.NewReader(""), &stdout, &stderr,
	)
	if err == nil || !strings.Contains(err.Error(), "unreachable") {
		t.Fatalf("launch with the hub down: err = %v", err)
	}
	if stdout.Len() != 0 {
		t.Fatalf("the provider ran without the hub: %q", stdout.String())
	}
}

func TestRunHomeCodexThroughHubRelay(t *testing.T) {
	hubDir := t.TempDir()
	hubConfig := writeCodexProxyHomeConfig(t, hubDir, "127.0.0.1:1")
	createHomeAccount(t, hubConfig, "codex", "main2")
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
	fakeCodex := writeFakeCodex(t, dir)
	var stdout, stderr bytes.Buffer
	if err := runWithInput(
		[]string{"home", "run", "-config", configPath, "-service", "codex", "--", fakeCodex, "exec"},
		strings.NewReader(""), &stdout, &stderr,
	); err != nil {
		t.Fatalf("home run failed: %v; stderr=%s", err, stderr.String())
	}
	got := stdout.String()
	for _, want := range []string{
		"-c chatgpt_base_url=http://localhost:",
		"model_providers.subswapper.base_url=http://127.0.0.1:",
		" exec\n",
		"home=unset proxy=1",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("hub launch output lacks %q:\n%s%s", want, got, stderr.String())
		}
	}
	nativeAuth, err := os.ReadFile(filepath.Join(dir, "native-home", ".codex", "auth.json"))
	if err != nil || !subswapper.IsCodexProxyAuthFile(nativeAuth) || !strings.Contains(string(nativeAuth), placeholder.Token) {
		t.Fatalf("native auth.json = %s, err = %v", nativeAuth, err)
	}

	// A real login on the client is moved aside by proxy-auth, as on a hub.
	if err := os.WriteFile(filepath.Join(dir, "native-home", ".codex", "auth.json"),
		[]byte(`{"auth_mode":"chatgpt","tokens":{"access_token":"native","refresh_token":"native"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	err = runWithInput([]string{"home", "run", "-config", configPath, "-service", "codex", "--", fakeCodex}, strings.NewReader(""), &stdout, &stderr)
	if err == nil || !strings.Contains(err.Error(), "proxy-auth") {
		t.Fatalf("launch over a real native login: err = %v", err)
	}
	stdout.Reset()
	if err := run([]string{"home", "proxy-auth", "-config", configPath, "-service", "codex"}, &stdout, &stderr); err != nil {
		t.Fatalf("proxy-auth failed: %v", err)
	}
	if !strings.Contains(stdout.String(), "moved the previous login to") {
		t.Fatalf("proxy-auth output = %q", stdout.String())
	}
}

func TestHubExportAndImportCommands(t *testing.T) {
	hubDir := t.TempDir()
	t.Setenv("HOME", filepath.Join(hubDir, "native-home"))
	hubConfig := filepath.Join(hubDir, "config.json")
	config := map[string]any{
		"backup_root": filepath.Join(hubDir, "accounts"),
		"state_path":  filepath.Join(hubDir, "state.json"),
		"services": []any{map[string]any{
			"name": "claude", "kind": "claude", "proxy_listen": "127.0.0.1:7878",
			"hub_listen": "100.67.68.117:7878", "shared_runtime_home": "native",
		}},
	}
	data, err := json.Marshal(config)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(hubConfig, data, 0o600); err != nil {
		t.Fatal(err)
	}
	bundlePath := filepath.Join(hubDir, "hub.json")
	var stdout, stderr bytes.Buffer
	if err := run([]string{"hub", "export", "-config", hubConfig, "-host", "box-box", "-out", bundlePath}, &stdout, &stderr); err != nil {
		t.Fatalf("hub export failed: %v", err)
	}
	info, err := os.Stat(bundlePath)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("bundle mode = %v, err = %v", info, err)
	}
	if err := run([]string{"hub", "export", "-config", hubConfig, "-out", bundlePath}, &stdout, &stderr); err == nil {
		t.Fatal("hub export overwrote an existing bundle")
	}
	if err := run([]string{"hub", "export", "-config", hubConfig}, &stdout, &stderr); err == nil || !strings.Contains(err.Error(), "-out") {
		t.Fatalf("hub export without -out: err = %v", err)
	}

	clientDir := t.TempDir()
	t.Setenv("XDG_DATA_HOME", filepath.Join(clientDir, "data"))
	clientConfig := filepath.Join(clientDir, "config.json")
	stdout.Reset()
	if err := run([]string{"hub", "import", "-config", clientConfig, "-in", bundlePath}, &stdout, &stderr); err != nil {
		t.Fatalf("hub import failed: %v", err)
	}
	if !strings.Contains(stdout.String(), "claude now uses the hub at http://box-box:7878") {
		t.Fatalf("hub import output = %q", stdout.String())
	}
	cfg, err := subswapper.LoadConfig(clientConfig)
	if err != nil {
		t.Fatal(err)
	}
	if service, _ := cfg.Service("claude"); !service.HubClient() {
		t.Fatalf("imported service = %#v", service)
	}
	// Account management stays on the hub.
	err = run([]string{"home", "login", "-config", clientConfig, "-service", "claude"}, &stdout, &stderr)
	if err == nil || !strings.Contains(err.Error(), "on the hub") {
		t.Fatalf("login on a hub client: err = %v", err)
	}
}

func TestRunHomeClaudeUsesFixedClientRelayWhenServing(t *testing.T) {
	hub, secret := startClaudeHub(t)
	dir := t.TempDir()
	configPath := writeHubClientConfig(t, dir, "claude", hub.URL, secret)
	probe := httptest.NewUnstartedServer(nil)
	relayListen := probe.Listener.Addr().String()
	_ = probe.Listener.Close()
	data, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	data = bytes.Replace(data, []byte(`"hub_url"`), []byte(`"proxy_listen": "`+relayListen+`", "hub_url"`), 1)
	if err := os.WriteFile(configPath, data, 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := subswapper.LoadConfig(configPath)
	if err != nil {
		t.Fatal(err)
	}
	services := configuredProxyServices(*cfg, "")
	if len(services) != 1 {
		t.Fatalf("proxy services on a relay client = %#v", services)
	}
	fakeClaude := writeFakeClaude(t, dir, fakeClaudeEnvReport)
	launch := func() string {
		t.Helper()
		var stdout, stderr bytes.Buffer
		if err := runWithInput(
			[]string{"home", "run", "-config", configPath, "-service", "claude", "--", fakeClaude, "-p"},
			strings.NewReader(""), &stdout, &stderr,
		); err != nil {
			t.Fatalf("home run failed: %v; stderr=%s", err, stderr.String())
		}
		return stdout.String()
	}

	// With the fixed relay down, the launch still gets a private relay.
	if got := launch(); strings.Contains(got, "base=http://"+relayListen) || !strings.Contains(got, "base=http://127.0.0.1:") {
		t.Fatalf("launch without the fixed relay = %q", got)
	}

	relay, err := newServiceProxy(*cfg, services[0], nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	served := make(chan error, 1)
	go func() { served <- relay.Serve(ctx) }()
	deadline := time.Now().Add(5 * time.Second)
	for !subswapper.HubRelayReachable(relayListen, secret) {
		select {
		case err := <-served:
			t.Fatalf("fixed relay stopped: %v", err)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("fixed relay never served")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if got := launch(); !strings.Contains(got, "base=http://"+relayListen+" ") {
		t.Fatalf("launch with the fixed relay = %q", got)
	}
}

func TestHubConnectEnrollsFromHub(t *testing.T) {
	hubDir := t.TempDir()
	hubConfig := writeProxyHomeConfig(t, hubDir, "127.0.0.1:1")
	data, err := os.ReadFile(hubConfig)
	if err != nil {
		t.Fatal(err)
	}
	data = bytes.Replace(data, []byte(`"proxy_listen"`), []byte(`"hub_listen":"127.0.0.2:1","hub_enroll":true,"proxy_listen"`), 1)
	if err := os.WriteFile(hubConfig, data, 0o600); err != nil {
		t.Fatal(err)
	}
	createHomeAccount(t, hubConfig, "claude", "work")
	storeTestSetupToken(t, hubConfig, "work", "sk-ant-oat01-real-account-token")
	cfg, err := subswapper.LoadConfig(hubConfig)
	if err != nil {
		t.Fatal(err)
	}
	proxy, err := subswapper.NewClaudeProxy(*cfg, "claude", nil)
	if err != nil {
		t.Fatal(err)
	}
	hub := httptest.NewServer(proxy)
	t.Cleanup(hub.Close)

	dir := t.TempDir()
	t.Setenv("HOME", filepath.Join(dir, "native-home"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(dir, "data"))
	configPath := filepath.Join(dir, "config.json")
	var stdout, stderr bytes.Buffer
	if err := run([]string{"hub", "connect", "-config", configPath, strings.TrimPrefix(hub.URL, "http://")}, &stdout, &stderr); err != nil {
		t.Fatalf("hub connect failed: %v; stderr=%s", err, stderr.String())
	}
	if !strings.Contains(stdout.String(), "claude now uses the hub at "+hub.URL) {
		t.Fatalf("hub connect output = %q", stdout.String())
	}
	fakeClaude := writeFakeClaude(t, dir, fakeClaudeEnvReport)
	stdout.Reset()
	if err := runWithInput(
		[]string{"home", "run", "-config", configPath, "-service", "claude", "--", fakeClaude, "-p"},
		strings.NewReader(""), &stdout, &stderr,
	); err != nil {
		t.Fatalf("home run after connect failed: %v; stderr=%s", err, stderr.String())
	}
	if !strings.Contains(stdout.String(), "proxy=1") {
		t.Fatalf("launch after connect = %q", stdout.String())
	}
	if err := run([]string{"hub", "connect", "-config", configPath}, &stdout, &stderr); err == nil {
		t.Fatal("hub connect without an address succeeded")
	}
}
