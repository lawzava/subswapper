package main

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lawzava/subswapper/internal/subswapper"
)

func TestSetupHubWritesConfigWithoutServices(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", filepath.Join(dir, "home"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(dir, "data"))
	configPath := filepath.Join(dir, "config.json")
	var stdout, stderr bytes.Buffer
	if err := run([]string{"setup", "hub", "-config", configPath, "-tailscale-ip", "100.64.0.5", "-enroll", "-no-service"}, &stdout, &stderr); err != nil {
		t.Fatalf("setup hub: %v; stderr=%s", err, stderr.String())
	}
	cfg, err := subswapper.LoadConfig(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if service, _ := cfg.Service("claude"); service.HubListen != "100.64.0.5:7878" || !service.HubEnroll {
		t.Fatalf("claude = %#v", service)
	}
	for _, want := range []string{"subswapper add claude <name>", "subswapper setup client 100.64.0.5"} {
		if !strings.Contains(stdout.String(), want) {
			t.Fatalf("setup hub output lacks %q:\n%s", want, stdout.String())
		}
	}
}

func TestHubUnitsRunProxyAndMonitor(t *testing.T) {
	units := hubUnits("/opt/bin/subswapper", "/opt/bin:/usr/bin")
	proxy, monitor := units["subswapper-hub.service"], units["subswapper.service"]
	if !strings.Contains(proxy, "ExecStart=/opt/bin/subswapper proxy\n") || !strings.Contains(monitor, "ExecStart=/opt/bin/subswapper monitor -interval 5m\n") {
		t.Fatalf("units:\n%s\n%s", proxy, monitor)
	}
	if !strings.Contains(monitor, "Environment=PATH=/opt/bin:/usr/bin\n") {
		t.Fatalf("monitor unit lacks PATH:\n%s", monitor)
	}
}

func TestSetupClientConnectsAndMovesRealCodexLoginAside(t *testing.T) {
	hubDir := t.TempDir()
	hubConfig := writeCodexProxyHomeConfig(t, hubDir, "127.0.0.1:1")
	data, err := os.ReadFile(hubConfig)
	if err != nil {
		t.Fatal(err)
	}
	data = bytes.Replace(data, []byte(`"proxy_listen"`), []byte(`"hub_listen":"127.0.0.2:1","hub_enroll":true,"proxy_listen"`), 1)
	if err := os.WriteFile(hubConfig, data, 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := subswapper.LoadConfig(hubConfig)
	if err != nil {
		t.Fatal(err)
	}
	proxy, err := subswapper.NewCodexProxy(*cfg, "codex", nil)
	if err != nil {
		t.Fatal(err)
	}
	hub := newTestServer(t, proxy)

	dir := t.TempDir()
	t.Setenv("HOME", filepath.Join(dir, "native-home"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(dir, "data"))
	native := filepath.Join(dir, "native-home", ".codex")
	if err := os.MkdirAll(native, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(native, "auth.json"), []byte(`{"auth_mode":"chatgpt","tokens":{"access_token":"real","refresh_token":"real"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(dir, "config.json")
	var stdout, stderr bytes.Buffer
	if err := run([]string{"setup", "client", "-config", configPath, strings.TrimPrefix(hub.URL, "http://")}, &stdout, &stderr); err != nil {
		t.Fatalf("setup client: %v\n%s", err, stdout.String())
	}
	auth, err := os.ReadFile(filepath.Join(native, "auth.json"))
	if err != nil || !subswapper.IsCodexProxyAuthFile(auth) {
		t.Fatalf("native login after setup = %s, %v", auth, err)
	}
	for _, want := range []string{"codex now uses the hub", "moved the previous login to", "subswapper codex"} {
		if !strings.Contains(stdout.String(), want) {
			t.Fatalf("setup client output lacks %q:\n%s", want, stdout.String())
		}
	}
}

func newTestServer(t *testing.T, handler http.Handler) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	return server
}

func TestSetupLocalConfiguresProxiesWithoutHub(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", filepath.Join(dir, "home"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(dir, "data"))
	configPath := filepath.Join(dir, "config.json")
	var stdout, stderr bytes.Buffer
	if err := run([]string{"setup", "local", "-config", configPath, "-no-service"}, &stdout, &stderr); err != nil {
		t.Fatalf("setup local: %v; stderr=%s", err, stderr.String())
	}
	cfg, err := subswapper.LoadConfig(configPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"claude", "codex"} {
		service, _ := cfg.Service(name)
		if !service.ProxyEnabled() || service.HubListen != "" || !service.UsesNativeRuntimeHome() {
			t.Fatalf("%s = %#v", name, service)
		}
	}
	if strings.Contains(stdout.String(), "setup client") {
		t.Fatalf("local setup suggested clients:\n%s", stdout.String())
	}
}
