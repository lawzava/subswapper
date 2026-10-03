package subswapper

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestHubConfigValidation(t *testing.T) {
	for _, test := range []struct {
		name    string
		service ServiceConfig
		wantErr string
	}{
		{name: "tailnet IPv4 hub", service: ServiceConfig{Name: "claude", Kind: "claude", ProxyListen: "127.0.0.1:7878", HubListen: "100.67.68.117:7878"}},
		{name: "tailnet IPv6 hub", service: ServiceConfig{Name: "claude", Kind: "claude", ProxyListen: "127.0.0.1:7878", HubListen: "[fd7a:115c:a1e0::1]:7878"}},
		{name: "loopback hub", service: ServiceConfig{Name: "claude", Kind: "claude", ProxyListen: "127.0.0.1:7878", HubListen: "127.0.0.2:7878"}},
		{name: "wildcard hub", service: ServiceConfig{Name: "claude", Kind: "claude", ProxyListen: "127.0.0.1:7878", HubListen: "0.0.0.0:7878"}, wantErr: "Tailscale"},
		{name: "LAN hub", service: ServiceConfig{Name: "claude", Kind: "claude", ProxyListen: "127.0.0.1:7878", HubListen: "192.168.1.5:7878"}, wantErr: "Tailscale"},
		{name: "hostname hub", service: ServiceConfig{Name: "claude", Kind: "claude", ProxyListen: "127.0.0.1:7878", HubListen: "box-box:7878"}, wantErr: "Tailscale"},
		{name: "hub without local proxy", service: ServiceConfig{Name: "claude", Kind: "claude", HubListen: "100.67.68.117:7878"}, wantErr: "requires proxy_listen"},
		{name: "native client", service: ServiceConfig{Name: "codex", Kind: "codex", HubURL: "http://box-box:7879", SharedRuntimeHome: NativeRuntimeHome}},
		{name: "shared home client", service: ServiceConfig{Name: "claude", Kind: "claude", HubURL: "http://100.67.68.117:7878", SharedRuntimeHome: "/srv/claude"}},
		{name: "client without runtime home", service: ServiceConfig{Name: "claude", Kind: "claude", HubURL: "http://box-box:7878"}, wantErr: "shared_runtime_home"},
		{name: "client with fixed relay", service: ServiceConfig{Name: "claude", Kind: "claude", HubURL: "http://box-box:7878", SharedRuntimeHome: NativeRuntimeHome, ProxyListen: "127.0.0.1:7878"}},
		{name: "client relay off loopback", service: ServiceConfig{Name: "claude", Kind: "claude", HubURL: "http://box-box:7878", SharedRuntimeHome: NativeRuntimeHome, ProxyListen: "100.67.68.117:7878"}, wantErr: "loopback"},
		{name: "client with hub listener", service: ServiceConfig{Name: "claude", Kind: "claude", HubURL: "http://box-box:7878", SharedRuntimeHome: NativeRuntimeHome, ProxyListen: "127.0.0.1:7878", HubListen: "100.67.68.117:7878"}, wantErr: "hub_url replaces"},
		{name: "client with upstream", service: ServiceConfig{Name: "claude", Kind: "claude", HubURL: "http://box-box:7878", SharedRuntimeHome: NativeRuntimeHome, ProxyListen: "127.0.0.1:7878", ProxyUpstream: "https://api.anthropic.com"}, wantErr: "hub_url replaces"},
		{name: "client with path", service: ServiceConfig{Name: "claude", Kind: "claude", HubURL: "http://box-box:7878/v1", SharedRuntimeHome: NativeRuntimeHome}, wantErr: "hub_url"},
		{name: "bundle client", service: ServiceConfig{Name: "claude", Kind: "claude", AccountMode: AccountModeBundle, HubURL: "http://box-box:7878", SharedRuntimeHome: NativeRuntimeHome}, wantErr: "account_mode"},
	} {
		t.Run(test.name, func(t *testing.T) {
			cfg := Config{Services: []ServiceConfig{test.service}}
			cfg.ApplyDefaults()
			err := cfg.Validate()
			if test.wantErr == "" {
				if err != nil {
					t.Fatalf("Validate() = %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("Validate() = %v, want %q", err, test.wantErr)
			}
		})
	}
}

func TestHubClientIsNotALocalProxy(t *testing.T) {
	service := ServiceConfig{Name: "claude", Kind: "claude", AccountMode: AccountModeHome, HubURL: "http://box-box:7878", SharedRuntimeHome: NativeRuntimeHome}
	if !service.HubClient() || service.ProxyEnabled() || service.HubRelayEnabled() {
		t.Fatalf("HubClient = %v, ProxyEnabled = %v, HubRelayEnabled = %v", service.HubClient(), service.ProxyEnabled(), service.HubRelayEnabled())
	}
	// A fixed relay address keeps the client from serving an account proxy.
	service.ProxyListen = "127.0.0.1:7878"
	if service.ProxyEnabled() || service.ClaudeProxyEnabled() || !service.HubRelayEnabled() {
		t.Fatalf("relay client: ProxyEnabled = %v, HubRelayEnabled = %v", service.ProxyEnabled(), service.HubRelayEnabled())
	}
}

func TestHubRelayServesFixedClientAddress(t *testing.T) {
	upstream := newProxyUpstream(t)
	cfg, proxy := setupProxyAccounts(t, upstream.server.URL)
	hub := httptest.NewServer(proxy)
	t.Cleanup(hub.Close)
	client := ServiceConfig{Name: "claude", Kind: "claude", AccountMode: AccountModeHome, HubURL: hub.URL, SharedRuntimeHome: NativeRuntimeHome, ProxyListen: freeLoopbackAddress(t)}
	relay, err := NewHubRelay(client)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- relay.Serve(ctx) }()
	deadline := time.Now().Add(5 * time.Second)
	// A session launched against the old local proxy keeps its secret and
	// address, so it reaches the hub through the relay unchanged.
	probe := cfg.Services[0]
	probe.ProxyListen = client.ProxyListen
	for !ClaudeProxyReachable(probe, proxy.secret) {
		if time.Now().After(deadline) {
			t.Fatal("hub never reachable through the fixed relay")
		}
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("relay did not stop")
	}
	if _, err := NewHubRelay(cfg.Services[0]); err == nil {
		t.Fatal("relay built for a service that is not a hub client")
	}
}

func TestHubReimportKeepsClientRelay(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.json")
	if err := os.WriteFile(configPath, []byte(`{"state_path":"`+filepath.Join(dir, "state.json")+`","services":[{"name":"claude","kind":"claude","proxy_listen":"127.0.0.1:7878","hub_url":"http://old-hub:7878","shared_runtime_home":"native"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	bundle := HubBundle{Version: 1, Services: []HubBundleService{{Name: "claude", Kind: "claude", HubURL: "http://box-box:7878", Secret: "sk-ant-oat01-" + strings.Repeat("ab", 32)}}}
	if _, err := ImportHubBundle(configPath, bundle); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if service, _ := cfg.Service("claude"); service.HubURL != "http://box-box:7878" || service.ProxyListen != "127.0.0.1:7878" {
		t.Fatalf("reimported service = %#v", service)
	}
}

func freeLoopbackAddress(t *testing.T) string {
	t.Helper()
	server := httptest.NewUnstartedServer(nil)
	listen := server.Listener.Addr().String()
	_ = server.Listener.Close()
	return listen
}

func TestClaudeProxyServesTheHubListenerToo(t *testing.T) {
	upstream := newProxyUpstream(t)
	cfg, _ := setupProxyAccounts(t, upstream.server.URL)
	cfg.Services[0].ProxyListen = freeLoopbackAddress(t)
	cfg.Services[0].HubListen = freeLoopbackAddress(t)
	proxy, err := NewClaudeProxy(cfg, "claude", func(string, ...any) {})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- proxy.Serve(ctx) }()
	local := cfg.Services[0]
	hub := cfg.Services[0]
	hub.ProxyListen = hub.HubListen
	deadline := time.Now().Add(5 * time.Second)
	for !ClaudeProxyReachable(local, proxy.secret) || !ClaudeProxyReachable(hub, proxy.secret) {
		if time.Now().After(deadline) {
			t.Fatal("proxy never served both listeners")
		}
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("proxy did not stop")
	}
	if ClaudeProxyReachable(hub, proxy.secret) {
		t.Fatal("hub listener still serving after shutdown")
	}
}

func TestHubBundleRoundTripConfiguresClient(t *testing.T) {
	hubDir := t.TempDir()
	hubCfg := Config{
		BackupRoot: filepath.Join(hubDir, "accounts"),
		StatePath:  filepath.Join(hubDir, "state.json"),
		Services: []ServiceConfig{
			{Name: "claude", Kind: "claude", ProxyListen: "127.0.0.1:7878", HubListen: "100.67.68.117:7878", SharedRuntimeHome: NativeRuntimeHome},
			{Name: "codex", Kind: "codex", ProxyListen: "127.0.0.1:7879", HubListen: "100.67.68.117:7879", SharedRuntimeHome: NativeRuntimeHome},
			{Name: "local", Kind: "claude", ProxyListen: "127.0.0.1:7880"},
		},
	}
	hubCfg.ApplyDefaults()
	if err := hubCfg.Validate(); err != nil {
		t.Fatal(err)
	}
	bundle, err := ExportHubBundle(hubCfg, "", "box-box")
	if err != nil {
		t.Fatal(err)
	}
	if len(bundle.Services) != 2 {
		t.Fatalf("exported services = %#v; only hub_listen services belong in the bundle", bundle.Services)
	}
	secret, err := LoadOrCreateClaudeProxySecret(hubCfg, "claude")
	if err != nil {
		t.Fatal(err)
	}
	placeholder, err := LoadOrCreateCodexProxyPlaceholder(hubCfg, "codex")
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]HubBundleService{
		"claude": {Name: "claude", Kind: "claude", HubURL: "http://box-box:7878", Secret: secret},
		"codex":  {Name: "codex", Kind: "codex", HubURL: "http://box-box:7879", Secret: placeholder.Token},
	}
	for _, service := range bundle.Services {
		if service != want[service.Name] {
			t.Fatalf("bundle service = %#v, want %#v", service, want[service.Name])
		}
	}

	clientDir := t.TempDir()
	configPath := filepath.Join(clientDir, "config", "config.json")
	clientState := filepath.Join(clientDir, "data", "state.json")
	// An existing client config keeps its other settings.
	if err := os.MkdirAll(filepath.Dir(configPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(configPath, []byte(`{"state_path":"`+clientState+`","monitor":{"interval":"1m"},"services":[{"name":"codex","kind":"codex","display_name":"Codex here"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	names, err := ImportHubBundle(configPath, bundle)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(names, ",") != "claude,codex" {
		t.Fatalf("imported = %v", names)
	}
	clientCfg, err := LoadConfig(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if clientCfg.Monitor.Interval.Duration != time.Minute {
		t.Fatalf("monitor interval lost: %v", clientCfg.Monitor.Interval)
	}
	codex, _ := clientCfg.Service("codex")
	if !codex.HubClient() || codex.HubURL != "http://box-box:7879" || codex.DisplayName != "Codex here" || !codex.UsesNativeRuntimeHome() {
		t.Fatalf("client codex service = %#v", codex)
	}
	claude, _ := clientCfg.Service("claude")
	if !claude.HubClient() || claude.HubURL != "http://box-box:7878" {
		t.Fatalf("client claude service = %#v", claude)
	}
	gotSecret, err := LoadHubClaudeSecret(*clientCfg, "claude")
	if err != nil || gotSecret != secret {
		t.Fatalf("client secret = %q, %v", gotSecret, err)
	}
	gotPlaceholder, err := LoadHubCodexPlaceholder(*clientCfg, "codex")
	if err != nil || gotPlaceholder != placeholder {
		t.Fatalf("client placeholder = %#v, %v", gotPlaceholder, err)
	}
	for _, path := range []string{claudeProxySecretPath(*clientCfg, "claude"), codexProxyPlaceholderPath(*clientCfg, "codex"), configPath} {
		info, err := os.Stat(path)
		if err != nil || info.Mode().Perm() != 0o600 {
			t.Fatalf("%s mode = %v, err = %v", filepath.Base(path), info.Mode().Perm(), err)
		}
	}

	// Importing into the hub's own config would discard its local proxy.
	hubConfigPath := filepath.Join(hubDir, "config.json")
	data, err := json.Marshal(hubCfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(hubConfigPath, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ImportHubBundle(hubConfigPath, bundle); err == nil || !strings.Contains(err.Error(), "proxy_listen") {
		t.Fatalf("import over a serving proxy: err = %v", err)
	}
}

func TestHubImportCreatesMissingConfig(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("XDG_DATA_HOME", filepath.Join(dir, "data"))
	configPath := filepath.Join(dir, "config", "subswapper", "config.json")
	bundle := HubBundle{Version: 1, Services: []HubBundleService{{Name: "claude", Kind: "claude", HubURL: "http://box-box:7878", Secret: "sk-ant-oat01-" + strings.Repeat("ab", 32)}}}
	if _, err := ImportHubBundle(configPath, bundle); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if secret, err := LoadHubClaudeSecret(*cfg, "claude"); err != nil || secret != bundle.Services[0].Secret {
		t.Fatalf("secret = %q, %v", secret, err)
	}
}

func TestHubImportRejectsMalformedBundles(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.json")
	valid := HubBundleService{Name: "claude", Kind: "claude", HubURL: "http://box-box:7878", Secret: "sk-ant-oat01-" + strings.Repeat("ab", 32)}
	for _, test := range []struct {
		name   string
		bundle HubBundle
	}{
		{name: "version", bundle: HubBundle{Version: 2, Services: []HubBundleService{valid}}},
		{name: "empty", bundle: HubBundle{Version: 1}},
		{name: "claude secret", bundle: HubBundle{Version: 1, Services: []HubBundleService{{Name: "claude", Kind: "claude", HubURL: valid.HubURL, Secret: "real-token"}}}},
		{name: "codex placeholder", bundle: HubBundle{Version: 1, Services: []HubBundleService{{Name: "codex", Kind: "codex", HubURL: valid.HubURL, Secret: "not-a-jwt"}}}},
		{name: "kind", bundle: HubBundle{Version: 1, Services: []HubBundleService{{Name: "x", Kind: "other", HubURL: valid.HubURL, Secret: valid.Secret}}}},
		{name: "url", bundle: HubBundle{Version: 1, Services: []HubBundleService{{Name: "claude", Kind: "claude", HubURL: "ftp://box-box", Secret: valid.Secret}}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := ImportHubBundle(configPath, test.bundle); err == nil {
				t.Fatal("malformed bundle imported")
			}
			if _, err := os.Stat(configPath); !os.IsNotExist(err) {
				t.Fatalf("rejected import wrote the config: %v", err)
			}
		})
	}
}

func TestHubClientCredentialIsNeverCreated(t *testing.T) {
	cfg := Config{StatePath: filepath.Join(t.TempDir(), "state.json"), Services: []ServiceConfig{{Name: "claude", Kind: "claude", HubURL: "http://box-box:7878", SharedRuntimeHome: NativeRuntimeHome}}}
	cfg.ApplyDefaults()
	if _, err := LoadHubClaudeSecret(cfg, "claude"); err == nil || !strings.Contains(err.Error(), "hub import") {
		t.Fatalf("missing secret: err = %v", err)
	}
	if _, err := os.Stat(claudeProxySecretPath(cfg, "claude")); !os.IsNotExist(err) {
		t.Fatalf("a client minted its own secret: %v", err)
	}
}

func TestHubRelayForwardsVerbatimAndStreams(t *testing.T) {
	type seen struct {
		path, query, auth, host, body string
	}
	calls := make(chan seen, 1)
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		calls <- seen{r.URL.Path, r.URL.RawQuery, r.Header.Get("Authorization"), r.Host, string(body)}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: one\n\n")
		w.(http.Flusher).Flush()
		_, _ = io.WriteString(w, "event: two\n\n")
	}))
	t.Cleanup(hub.Close)
	listen, stop, err := StartHubRelay(hub.URL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(stop)
	if err := validateLoopbackListen(listen); err != nil {
		t.Fatalf("relay listens on %q: %v", listen, err)
	}
	req, err := http.NewRequest(http.MethodPost, "http://"+listen+"/v1/messages?beta=true", strings.NewReader(`{"x":1}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer sk-ant-oat01-launch")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK || string(body) != "event: one\n\nevent: two\n\n" {
		t.Fatalf("relay response = %d %q", resp.StatusCode, body)
	}
	got := <-calls
	want := seen{"/v1/messages", "beta=true", "Bearer sk-ant-oat01-launch", strings.TrimPrefix(hub.URL, "http://"), `{"x":1}`}
	if got != want {
		t.Fatalf("hub saw %#v, want %#v", got, want)
	}

	hub.Close()
	resp, err = http.Get("http://" + listen + "/subswapper/health")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("unreachable hub status = %d", resp.StatusCode)
	}
}

func TestHubRelayReachesClaudeProxyHealth(t *testing.T) {
	upstream := newProxyUpstream(t)
	cfg, proxy := setupProxyAccounts(t, upstream.server.URL)
	hub := httptest.NewServer(proxy)
	t.Cleanup(hub.Close)
	listen, stop, err := StartHubRelay(hub.URL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(stop)
	service := cfg.Services[0]
	service.ProxyListen = listen
	if !ClaudeProxyReachable(service, proxy.secret) {
		t.Fatal("hub proxy unreachable through the relay")
	}
	if ClaudeProxyReachable(service, "sk-ant-oat01-wrong") {
		t.Fatal("wrong secret accepted through the relay")
	}
}
