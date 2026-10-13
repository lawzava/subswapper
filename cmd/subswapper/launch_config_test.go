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

// launchConfigFixture serves a Codex proxy for a native-home config and puts
// a fake codex first on PATH. Shims go to a private data root.
func launchConfigFixture(t *testing.T) (string, string) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("XDG_DATA_HOME", filepath.Join(dir, "data"))
	server := httptest.NewUnstartedServer(nil)
	configPath := writeCodexProxyHomeConfig(t, dir, server.Listener.Addr().String())
	cfg, err := subswapper.LoadConfig(configPath)
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
	service, _ := cfg.Service("codex")
	if err := subswapper.EnsureCodexProxyAuth(subswapper.RuntimeHome(*cfg, service, ""), placeholder); err != nil {
		t.Fatal(err)
	}
	server.Config.Handler = proxy
	server.Start()
	t.Cleanup(server.Close)
	binary := writeFakeCodex(t, dir)
	t.Setenv("PATH", filepath.Dir(binary)+string(os.PathListSeparator)+os.Getenv("PATH"))
	return configPath, binary
}

func readLaunchConfig(t *testing.T, configPath string) subswapper.LaunchConfig {
	t.Helper()
	var stdout, stderr bytes.Buffer
	if err := run([]string{"launch-config", "-config", configPath, "-service", "codex"}, &stdout, &stderr); err != nil {
		t.Fatalf("launch-config: %v; stderr=%s", err, stderr.String())
	}
	var launch subswapper.LaunchConfig
	if err := json.Unmarshal(stdout.Bytes(), &launch); err != nil {
		t.Fatal(err)
	}
	return launch
}

func TestLaunchConfigLinksAShimToSubswapper(t *testing.T) {
	configPath, _ := launchConfigFixture(t)
	launch := readLaunchConfig(t, configPath)
	shim := subswapper.ProviderShimPath("codex")
	if len(launch.Command) != 1 || launch.Command[0] != shim || subswapper.ProviderShimName(shim) != "codex" {
		t.Fatalf("command = %v, want [%s]", launch.Command, shim)
	}
	if len(launch.Env) != 2 || launch.Env["SUBSWAPPER_CONFIG_PATH"] != configPath || launch.Env["SUBSWAPPER_SERVICE"] != "codex" {
		t.Fatalf("env = %v", launch.Env)
	}
	self, err := os.Stat(os.Args[0])
	if err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(shim); err != nil || !os.SameFile(info, self) {
		t.Fatalf("shim does not reach this executable: %v", err)
	}

	// A link left by another install is repointed; a second export is a no-op.
	if err := os.Remove(shim); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(t.TempDir(), "old-subswapper"), shim); err != nil {
		t.Fatal(err)
	}
	readLaunchConfig(t, configPath)
	readLaunchConfig(t, configPath)
	if info, err := os.Stat(shim); err != nil || !os.SameFile(info, self) {
		t.Fatalf("stale shim was not repointed: %v", err)
	}
}

func TestLaunchConfigRefusesWithoutTheProviderCLIOrOverAFile(t *testing.T) {
	configPath, _ := launchConfigFixture(t)
	t.Setenv("PATH", t.TempDir())
	var stdout, stderr bytes.Buffer
	err := run([]string{"launch-config", "-config", configPath, "-service", "codex"}, &stdout, &stderr)
	if err == nil || !strings.Contains(err.Error(), "codex is not on PATH") || stdout.Len() != 0 {
		t.Fatalf("missing codex: err=%v stdout=%q", err, stdout.String())
	}
	if _, err := os.Lstat(subswapper.ProviderShimPath("codex")); !os.IsNotExist(err) {
		t.Fatalf("shim created without a provider CLI: %v", err)
	}

	configPath, _ = launchConfigFixture(t)
	shim := subswapper.ProviderShimPath("codex")
	if err := os.MkdirAll(filepath.Dir(shim), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(shim, []byte("user file"), 0o755); err != nil {
		t.Fatal(err)
	}
	err = run([]string{"launch-config", "-config", configPath, "-service", "codex"}, &stdout, &stderr)
	if data, _ := os.ReadFile(shim); err == nil || string(data) != "user file" {
		t.Fatalf("overwrote a regular file: err=%v", err)
	}
}

func TestProviderShimPassesTheVersionProbeToTheRealCLI(t *testing.T) {
	_, binary := launchConfigFixture(t)
	// Paseo probes without the provider's environment, so no config is needed.
	t.Setenv("SUBSWAPPER_CONFIG_PATH", filepath.Join(t.TempDir(), "missing.json"))
	var stdout, stderr bytes.Buffer
	if err := runProviderShim("codex", []string{"--version"}, strings.NewReader(""), &stdout, &stderr); err != nil {
		t.Fatalf("version probe: %v; stderr=%s", err, stderr.String())
	}
	if !strings.HasPrefix(stdout.String(), "args=--version\n") {
		t.Fatalf("version probe did not reach %s directly:\n%s", binary, stdout.String())
	}
}

func TestProviderShimLaunchesThroughTheProxy(t *testing.T) {
	configPath, _ := launchConfigFixture(t)
	createHomeAccount(t, configPath, "codex", "main")
	t.Setenv("SUBSWAPPER_CONFIG_PATH", configPath)
	t.Setenv("SUBSWAPPER_SERVICE", "codex")
	var stdout, stderr bytes.Buffer
	if err := runProviderShim("codex", []string{"app-server"}, strings.NewReader(""), &stdout, &stderr); err != nil {
		t.Fatalf("shim launch: %v; stderr=%s", err, stderr.String())
	}
	for _, want := range []string{"model_provider=subswapper", " app-server\n", "proxy=1"} {
		if !strings.Contains(stdout.String(), want) {
			t.Fatalf("shim launch lacks %q:\n%s%s", want, stdout.String(), stderr.String())
		}
	}
}

func TestProviderShimSelectsTheServiceByKind(t *testing.T) {
	configPath, _ := launchConfigFixture(t)
	data, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	renamed := strings.Replace(string(data), `"name":"codex"`, `"name":"work"`, 1)
	renamed = strings.Replace(renamed, `"services":[`, `"services":[{"name":"claude","kind":"claude","account_mode":"home"},`, 1)
	if err := os.WriteFile(configPath, []byte(renamed), 0o600); err != nil {
		t.Fatal(err)
	}
	createHomeAccount(t, configPath, "work", "main")
	t.Setenv("SUBSWAPPER_CONFIG_PATH", configPath)
	var stdout, stderr bytes.Buffer
	// A Claude session exports its own service; codex must not adopt it.
	t.Setenv("SUBSWAPPER_SERVICE", "claude")
	if err := runProviderShim("codex", []string{"exec"}, strings.NewReader(""), &stdout, &stderr); err == nil || !strings.Contains(err.Error(), `"codex"`) {
		t.Fatalf("claude service selected for codex: %v", err)
	}
	t.Setenv("SUBSWAPPER_SERVICE", "work")
	if err := runProviderShim("codex", []string{"exec"}, strings.NewReader(""), &stdout, &stderr); err != nil {
		t.Fatalf("codex service work: %v; stderr=%s", err, stderr.String())
	}
}

func TestLookProviderBinarySkipsSubswapper(t *testing.T) {
	shimDir := t.TempDir()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(self, filepath.Join(shimDir, "codex")); err != nil {
		t.Fatal(err)
	}
	real := writeFakeCodex(t, t.TempDir())
	t.Setenv("PATH", shimDir+string(os.PathListSeparator)+filepath.Dir(real))
	if got, err := lookProviderBinary("codex"); err != nil || got != real {
		t.Fatalf("lookProviderBinary = %q, %v; want %q", got, err, real)
	}
}
