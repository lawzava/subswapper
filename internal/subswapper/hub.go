package subswapper

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const (
	hubBundleVersion = 1
	hubBundlePath    = "/subswapper/hub/bundle"
	// defaultHubPort is the Claude proxy port the README configures; any
	// enrolled service's port returns the same bundle.
	defaultHubPort       = "7878"
	hubBundleFetchWindow = 15 * time.Second
	hubStatusPath        = "/subswapper/hub/status"
	// hubStatusWindow bounds the hub's status run; a client waits a little
	// longer so it sees the hub's answer rather than its own timeout.
	hubStatusWindow      = 30 * time.Second
	hubStatusFetchWindow = 45 * time.Second
)

// Tailscale assigns node addresses from these ranges. The hub listener
// carries the proxy secret over plain HTTP, so it is only safe where
// WireGuard already encrypts the link.
var tailscalePrefixes = []netip.Prefix{
	netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("fd7a:115c:a1e0::/48"),
}

// HubBundle is what a hub client needs: where the hub is and the credential
// its proxy accepts. It never contains an account token.
type HubBundle struct {
	Version  int                `json:"version"`
	Services []HubBundleService `json:"services"`
}

type HubBundleService struct {
	Name   string `json:"name"`
	Kind   string `json:"kind"`
	HubURL string `json:"hub_url"`
	// Secret is the Claude proxy secret or the Codex placeholder token.
	Secret string `json:"secret"`
}

func validateHubListen(listen string) error {
	host, port, err := net.SplitHostPort(listen)
	if err != nil {
		return errors.New("must be host:port")
	}
	number, err := strconv.Atoi(port)
	if err != nil || number <= 0 || number > 65535 {
		return errors.New("port must be a fixed number between 1 and 65535")
	}
	address, err := netip.ParseAddr(host)
	if err == nil && address.Zone() == "" {
		address = address.Unmap()
		if address.IsLoopback() {
			return nil
		}
		for _, prefix := range tailscalePrefixes {
			if prefix.Contains(address) {
				return nil
			}
		}
	}
	return errors.New("host must be this machine's Tailscale IP (100.64.0.0/10 or fd7a:115c:a1e0::/48); the hub relays the proxy secret over plain HTTP")
}

// serveProxy serves handler on every address until ctx is cancelled. All
// listeners are bound before any request is served, so a bad hub address
// fails startup instead of leaving a half-running proxy.
func serveProxy(ctx context.Context, handler http.Handler, addresses ...string) error {
	listeners := make([]net.Listener, 0, len(addresses))
	for _, address := range addresses {
		listener, err := net.Listen("tcp", address)
		if err != nil {
			for _, open := range listeners {
				_ = open.Close()
			}
			return fmt.Errorf("listen on %s: %w", address, err)
		}
		listeners = append(listeners, listener)
	}
	server := &http.Server{Handler: handler, ReadHeaderTimeout: 10 * time.Second}
	served := make(chan error, len(listeners))
	for _, listener := range listeners {
		go func() { served <- server.Serve(listener) }()
	}
	var err error
	select {
	case err = <-served:
		// One listener failing stops the others so the caller sees the error.
		_ = server.Close()
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if shutdownErr := server.Shutdown(shutdownCtx); shutdownErr != nil {
			_ = server.Close()
		}
		err = <-served
	}
	for range len(listeners) - 1 {
		<-served
	}
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

func proxyListenAddresses(service ServiceConfig) []string {
	if service.HubListen == "" {
		return []string{service.ProxyListen}
	}
	return []string{service.ProxyListen, service.HubListen}
}

// ExportHubBundle collects the credentials of every service that serves a
// hub listener, or only serviceName when it is set. host replaces the
// listener's IP in the client URL, e.g. with a MagicDNS name.
func ExportHubBundle(cfg Config, serviceName, host string) (HubBundle, error) {
	bundle := HubBundle{Version: hubBundleVersion}
	for _, service := range cfg.Services {
		if (serviceName != "" && service.Name != serviceName) || service.HubListen == "" || service.Disabled {
			continue
		}
		listenHost, port, err := net.SplitHostPort(service.HubListen)
		if err != nil {
			return HubBundle{}, fmt.Errorf("service %q hub_listen: %w", service.Name, err)
		}
		if host != "" {
			listenHost = host
		}
		entry := HubBundleService{
			Name:   service.Name,
			Kind:   strings.ToLower(service.Kind),
			HubURL: "http://" + net.JoinHostPort(listenHost, port),
		}
		switch {
		case service.ClaudeProxyEnabled():
			entry.Kind = "claude"
			entry.Secret, err = LoadOrCreateClaudeProxySecret(cfg, service.Name)
		case service.CodexProxyEnabled():
			var placeholder CodexProxyPlaceholder
			placeholder, err = LoadOrCreateCodexProxyPlaceholder(cfg, service.Name)
			entry.Secret = placeholder.Token
		}
		if err != nil {
			return HubBundle{}, err
		}
		bundle.Services = append(bundle.Services, entry)
	}
	if len(bundle.Services) == 0 {
		if serviceName != "" {
			return HubBundle{}, fmt.Errorf("service %q has no hub_listen configured", serviceName)
		}
		return HubBundle{}, errors.New("no service has hub_listen configured")
	}
	return bundle, nil
}

// serveHubBundle answers enrollment on a service with hub_enroll and reports
// whether it handled r. The client reaches every service by the host it
// dialed; the service it asked keeps the exact address it used.
func serveHubBundle(w http.ResponseWriter, r *http.Request, cfg Config, service ServiceConfig) bool {
	if r.URL.Path != hubBundlePath || !service.HubEnroll {
		return false
	}
	if r.Method != http.MethodGet {
		writeClaudeProxyError(w, http.StatusMethodNotAllowed, "invalid_request_error", "hub enrollment accepts GET only")
		return true
	}
	host := r.Host
	if name, _, err := net.SplitHostPort(r.Host); err == nil {
		host = name
	}
	bundle, err := ExportHubBundle(cfg, "", host)
	if err != nil {
		writeClaudeProxyError(w, http.StatusInternalServerError, "api_error", "subswapper hub could not build the client bundle")
		return true
	}
	if r.Host != "" {
		for index := range bundle.Services {
			if bundle.Services[index].Name == service.Name {
				bundle.Services[index].HubURL = "http://" + r.Host
			}
		}
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(bundle)
	return true
}

// serveHubStatus answers an authenticated client with the hub's status run,
// the same probe `subswapper status` makes on the hub, for every service.
func serveHubStatus(w http.ResponseWriter, r *http.Request, cfg Config) bool {
	if r.URL.Path != hubStatusPath {
		return false
	}
	if r.Method != http.MethodGet {
		writeClaudeProxyError(w, http.StatusMethodNotAllowed, "invalid_request_error", "hub status accepts GET only")
		return true
	}
	ctx, cancel := context.WithTimeout(r.Context(), hubStatusWindow)
	defer cancel()
	cycle, err := StatusOnce(ctx, cfg)
	if err != nil {
		writeClaudeProxyError(w, http.StatusInternalServerError, "api_error", "subswapper hub status failed")
		return true
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(cycle.Results)
	return true
}

// FetchHubStatus asks the hub at hubURL for its status with a client credential.
func FetchHubStatus(ctx context.Context, hubURL, credential string) ([]ServiceStatus, error) {
	origin, err := parseProxyUpstream(hubURL)
	if err != nil {
		return nil, fmt.Errorf("hub_url: %w", err)
	}
	ctx, cancel := context.WithTimeout(ctx, hubStatusFetchWindow)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, origin.String()+hubStatusPath, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+credential)
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	resp, err := (&http.Client{Transport: transport}).Do(req)
	if err != nil {
		return nil, errors.New("hub did not answer")
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("hub answered %s", resp.Status)
	}
	var results []ServiceStatus
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&results); err != nil {
		return nil, errors.New("hub returned malformed status")
	}
	return results, nil
}

// hubBundleURL turns a hub address such as 100.67.68.117, box-box:7879, or
// http://box-box:7878 into its enrollment URL.
func hubBundleURL(address string) (string, error) {
	raw := strings.TrimSpace(address)
	if !strings.Contains(raw, "://") {
		if _, _, err := net.SplitHostPort(raw); err != nil {
			raw = net.JoinHostPort(raw, defaultHubPort)
		}
		raw = "http://" + raw
	}
	origin, err := parseProxyUpstream(raw)
	if err != nil {
		return "", fmt.Errorf("hub address: %w", err)
	}
	return origin.String() + hubBundlePath, nil
}

// FetchHubBundle asks a hub with hub_enroll for its client bundle.
func FetchHubBundle(ctx context.Context, address string) (HubBundle, error) {
	target, err := hubBundleURL(address)
	if err != nil {
		return HubBundle{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, hubBundleFetchWindow)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return HubBundle{}, err
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	resp, err := (&http.Client{Transport: transport}).Do(req)
	if err != nil {
		return HubBundle{}, fmt.Errorf("reach hub at %s: %w", address, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return HubBundle{}, fmt.Errorf("hub at %s answered %s; enable hub_enroll there, or use hub export and hub import", address, resp.Status)
	}
	var bundle HubBundle
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&bundle); err != nil {
		return HubBundle{}, errors.New("hub returned a malformed bundle")
	}
	return bundle, nil
}

func (b HubBundle) validate() error {
	if b.Version != hubBundleVersion {
		return fmt.Errorf("hub bundle version %d is not supported", b.Version)
	}
	if len(b.Services) == 0 {
		return errors.New("hub bundle lists no services")
	}
	for _, service := range b.Services {
		if strings.TrimSpace(service.Name) == "" {
			return errors.New("hub bundle service has no name")
		}
		if _, err := parseProxyUpstream(service.HubURL); err != nil {
			return fmt.Errorf("hub bundle service %q hub_url: %w", service.Name, err)
		}
		switch service.Kind {
		case "claude":
			if !strings.HasPrefix(service.Secret, claudeProxySecretPrefix) {
				return fmt.Errorf("hub bundle service %q secret is not a proxy secret", service.Name)
			}
		case "codex":
			if strings.Count(service.Secret, ".") != 2 {
				return fmt.Errorf("hub bundle service %q secret is not a proxy placeholder", service.Name)
			}
		default:
			return fmt.Errorf("hub bundle service %q kind %q is not claude or codex", service.Name, service.Kind)
		}
	}
	return nil
}

// ImportHubBundle turns the services in bundle into hub clients in the
// config at configPath, creating the file if needed, and stores their
// credentials. It returns the configured service names.
func ImportHubBundle(configPath string, bundle HubBundle) ([]string, error) {
	if err := bundle.validate(); err != nil {
		return nil, err
	}
	raw, err := readRawConfig(configPath)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(bundle.Services))
	for _, imported := range bundle.Services {
		service, err := rawService(&raw, imported.Name, imported.Kind)
		if err != nil {
			return nil, err
		}
		// Overwriting these would silently turn the hub itself, or a machine
		// with its own accounts, into a client. An existing client keeps its
		// relay address.
		if service.HubListen != "" || (service.ProxyListen != "" && service.HubURL == "") {
			return nil, fmt.Errorf("service %q serves its own proxy_listen here; remove it before importing a hub", service.Name)
		}
		service.HubURL = imported.HubURL
		if service.SharedRuntimeHome == "" {
			service.SharedRuntimeHome = NativeRuntimeHome
		}
		names = append(names, service.Name)
	}
	cfg, err := effectiveConfig(raw)
	if err != nil {
		return nil, err
	}
	// Credentials go first: a launch must never find hub_url without them.
	for _, imported := range bundle.Services {
		if err := storeHubCredential(cfg, imported); err != nil {
			return nil, err
		}
	}
	return names, writeRawConfig(configPath, raw)
}

// ConfigureHub makes the Claude and Codex services at configPath serve a hub
// on tailscaleIP, creating the file if needed. Existing proxy ports are kept;
// new services use 7878 and 7879. An empty tailscaleIP configures the
// local proxies only.
func ConfigureHub(configPath, tailscaleIP string, enroll bool) error {
	raw, err := readRawConfig(configPath)
	if err != nil {
		return err
	}
	for _, defaults := range []struct{ name, port string }{{"claude", "7878"}, {"codex", "7879"}} {
		service, err := rawService(&raw, defaults.name, defaults.name)
		if err != nil {
			return err
		}
		if service.HubURL != "" {
			return fmt.Errorf("service %q is a hub client of %s; remove hub_url before making this machine a hub", service.Name, service.HubURL)
		}
		if service.ProxyListen == "" {
			service.ProxyListen = net.JoinHostPort("127.0.0.1", defaults.port)
		}
		_, port, err := net.SplitHostPort(service.ProxyListen)
		if err != nil {
			return fmt.Errorf("service %q proxy_listen: %w", service.Name, err)
		}
		service.HubListen, service.HubEnroll = "", false
		if tailscaleIP != "" {
			service.HubListen = net.JoinHostPort(tailscaleIP, port)
			service.HubEnroll = enroll
		}
		if service.SharedRuntimeHome == "" {
			service.SharedRuntimeHome = NativeRuntimeHome
		}
	}
	if _, err := effectiveConfig(raw); err != nil {
		return err
	}
	return writeRawConfig(configPath, raw)
}

// readRawConfig reads a config without defaults, so writing it back keeps
// only what the user set. A missing file yields an empty config.
func readRawConfig(configPath string) (Config, error) {
	var raw Config
	path := ExpandPath(configPath)
	data, err := os.ReadFile(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		raw.Monitor.Interval.Duration = 5 * time.Minute
		return raw, nil
	case err != nil:
		return Config{}, err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&raw); err != nil {
		return Config{}, fmt.Errorf("parse %s: %w", filepath.Base(path), err)
	}
	return raw, nil
}

func writeRawConfig(configPath string, raw Config) error {
	path := ExpandPath(configPath)
	encoded, err := json.MarshalIndent(raw, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return writePrivateFile(path, append(encoded, '\n'))
}

// effectiveConfig validates raw as LoadConfig would see it.
func effectiveConfig(raw Config) (Config, error) {
	cfg := raw
	cfg.Services = append([]ServiceConfig(nil), raw.Services...)
	cfg.ApplyDefaults()
	return cfg, cfg.Validate()
}

// rawService returns the named service, appending it when missing, and
// checks that it is of kind ("claude" or "codex").
func rawService(raw *Config, name, kind string) (*ServiceConfig, error) {
	for index := range raw.Services {
		service := &raw.Services[index]
		if service.Name != name {
			continue
		}
		existing := strings.ToLower(service.Kind)
		if existing == "" {
			existing = strings.ToLower(service.Name)
		}
		if existing == "claude-code" {
			existing = "claude"
		}
		if existing != kind {
			return nil, fmt.Errorf("service %q is kind %q here, not %q", service.Name, existing, kind)
		}
		return service, nil
	}
	raw.Services = append(raw.Services, ServiceConfig{Name: name, Kind: kind})
	return &raw.Services[len(raw.Services)-1], nil
}

func storeHubCredential(cfg Config, imported HubBundleService) error {
	lock, err := AcquireStateLock(context.Background(), cfg)
	if err != nil {
		return err
	}
	defer lock.Release()
	root := claudeSetupTokenRoot(cfg)
	if err := os.MkdirAll(filepath.Dir(root), 0o700); err != nil {
		return err
	}
	for _, dir := range []string{root, filepath.Join(root, safeName(imported.Name))} {
		if err := ensurePrivateSetupTokenDirectory(dir); err != nil {
			return err
		}
	}
	var path string
	var payload any
	if imported.Kind == "claude" {
		path = claudeProxySecretPath(cfg, imported.Name)
		payload = struct {
			Secret string `json:"secret"`
		}{Secret: imported.Secret}
	} else {
		path = codexProxyPlaceholderPath(cfg, imported.Name)
		payload = CodexProxyPlaceholder{Token: imported.Secret, AccountID: codexProxyPlaceholderAccountID}
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	return writePrivateFile(path, data)
}

func hubClientService(cfg Config, serviceName string) (ServiceConfig, error) {
	service, ok := cfg.Service(serviceName)
	if !ok {
		return ServiceConfig{}, fmt.Errorf("service %q not found", serviceName)
	}
	if !service.HubClient() {
		return ServiceConfig{}, fmt.Errorf("service %q has no hub_url configured", serviceName)
	}
	return service, nil
}

func missingHubCredential(serviceName string) error {
	return fmt.Errorf("service %q has no hub credential; run `subswapper hub import` with a bundle from the hub", serviceName)
}

// LoadHubClaudeSecret reads the imported proxy secret. Unlike the hub, a
// client never mints one: a fresh secret would not match the hub's.
func LoadHubClaudeSecret(cfg Config, serviceName string) (string, error) {
	service, err := hubClientService(cfg, serviceName)
	if err != nil {
		return "", err
	}
	if !isClaudeService(service) {
		return "", fmt.Errorf("service %q is not a Claude service", serviceName)
	}
	secret, exists, err := readClaudeProxySecret(claudeProxySecretPath(cfg, serviceName))
	if err != nil {
		return "", err
	}
	if !exists {
		return "", missingHubCredential(serviceName)
	}
	return secret, nil
}

// LoadHubCodexPlaceholder reads the imported placeholder login.
func LoadHubCodexPlaceholder(cfg Config, serviceName string) (CodexProxyPlaceholder, error) {
	service, err := hubClientService(cfg, serviceName)
	if err != nil {
		return CodexProxyPlaceholder{}, err
	}
	if !isCodexService(service) {
		return CodexProxyPlaceholder{}, fmt.Errorf("service %q is not a Codex service", serviceName)
	}
	placeholder, exists, err := readCodexProxyPlaceholder(codexProxyPlaceholderPath(cfg, serviceName))
	if err != nil {
		return CodexProxyPlaceholder{}, err
	}
	if !exists {
		return CodexProxyPlaceholder{}, missingHubCredential(serviceName)
	}
	return placeholder, nil
}

// HubRelay serves a hub client's fixed proxy_listen address.
type HubRelay struct {
	listen  string
	handler http.Handler
}

func NewHubRelay(service ServiceConfig) (*HubRelay, error) {
	if !service.HubRelayEnabled() {
		return nil, fmt.Errorf("service %q has no hub_url and proxy_listen relay", service.Name)
	}
	handler, err := newHubRelayHandler(service.HubURL)
	if err != nil {
		return nil, err
	}
	return &HubRelay{listen: service.ProxyListen, handler: handler}, nil
}

// Serve relays until ctx is cancelled.
func (r *HubRelay) Serve(ctx context.Context) error {
	return serveProxy(ctx, r.handler, r.listen)
}

// StartHubRelay forwards a loopback port to the hub for the lifetime of one
// launch. The CLIs keep a loopback base URL, which Codex's workspace routing
// alias and the existing reachability checks rely on. stop closes the relay.
func StartHubRelay(hubURL string) (listen string, stop func(), err error) {
	relay, err := newHubRelayHandler(hubURL)
	if err != nil {
		return "", nil, err
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", nil, fmt.Errorf("start hub relay: %w", err)
	}
	server := &http.Server{Handler: relay, ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = server.Serve(listener) }()
	return listener.Addr().String(), func() { _ = server.Close() }, nil
}

func newHubRelayHandler(hubURL string) (http.Handler, error) {
	target, err := parseProxyUpstream(hubURL)
	if err != nil {
		return nil, fmt.Errorf("hub_url: %w", err)
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	// The hub is a tailnet peer; an HTTP(S)_PROXY meant for the internet
	// must not carry the proxy secret.
	transport.Proxy = nil
	transport.DisableCompression = true
	transport.ResponseHeaderTimeout = 5 * time.Minute
	return &httputil.ReverseProxy{
		Rewrite: func(request *httputil.ProxyRequest) {
			request.SetURL(target)
		},
		Transport: transport,
		// Server-sent events must reach the client as they arrive.
		FlushInterval: -1,
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			if r.Context().Err() != nil {
				return
			}
			writeClaudeProxyError(w, http.StatusBadGateway, "api_error", "subswapper hub at "+target.Host+" is unreachable")
		},
	}, nil
}

const (
	hubAccountsPath = "/subswapper/hub/accounts"
	hubSwitchPath   = "/subswapper/hub/switch"
	// hubManageWindow covers an automatic switch, which probes every account.
	hubManageWindow = 90 * time.Second
	hubRequestLimit = 1 << 20
)

// HubAccountRequest adds or replaces an account on the hub. A Claude service
// takes SetupToken; a Codex service takes AuthJSON, a ChatGPT login file.
type HubAccountRequest struct {
	Account    string          `json:"account"`
	Email      string          `json:"email,omitempty"`
	SetupToken string          `json:"setup_token,omitempty"`
	AuthJSON   json.RawMessage `json:"auth_json,omitempty"`
}

type HubAccountResult struct {
	Account string `json:"account"`
	Email   string `json:"email,omitempty"`
	Created bool   `json:"created"`
}

type HubSwitchResult struct {
	Active string `json:"active"`
}

// serveHubManagement lets a client holding the service credential add,
// remove, and select accounts on this machine. It reports whether it handled r.
func serveHubManagement(w http.ResponseWriter, r *http.Request, cfg Config, service ServiceConfig, lookup ClaudeSetupTokenIdentityLookup) bool {
	switch r.URL.Path {
	case hubAccountsPath:
		switch r.Method {
		case http.MethodPost:
			hubAddAccount(w, r, cfg, service, lookup)
		case http.MethodDelete:
			force := r.URL.Query().Get("force") == "1"
			if err := UnregisterAccount(r.Context(), cfg, service.Name, r.URL.Query().Get("account"), force, false); err != nil {
				writeClaudeProxyError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
				return true
			}
			writeHubJSON(w, struct{}{})
		default:
			writeClaudeProxyError(w, http.StatusMethodNotAllowed, "invalid_request_error", "accounts accept POST or DELETE")
		}
	case hubSwitchPath:
		if r.Method != http.MethodPost {
			writeClaudeProxyError(w, http.StatusMethodNotAllowed, "invalid_request_error", "switch accepts POST only")
			return true
		}
		var request struct {
			Account string `json:"account"`
		}
		if err := json.NewDecoder(io.LimitReader(r.Body, hubRequestLimit)).Decode(&request); err != nil {
			writeClaudeProxyError(w, http.StatusBadRequest, "invalid_request_error", "switch request is malformed")
			return true
		}
		active, err := switchServiceAccount(r.Context(), cfg, service.Name, request.Account)
		if err != nil {
			writeClaudeProxyError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
			return true
		}
		writeHubJSON(w, HubSwitchResult{Active: active})
	default:
		return false
	}
	return true
}

func hubAddAccount(w http.ResponseWriter, r *http.Request, cfg Config, service ServiceConfig, lookup ClaudeSetupTokenIdentityLookup) {
	var request HubAccountRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, hubRequestLimit)).Decode(&request); err != nil {
		writeClaudeProxyError(w, http.StatusBadRequest, "invalid_request_error", "account request is malformed")
		return
	}
	result := HubAccountResult{Account: request.Account, Email: request.Email}
	var err error
	switch {
	case isClaudeService(service) && request.SetupToken != "":
		result.Created, err = AddClaudeAccount(r.Context(), cfg, service.Name, request.Account, request.Email, request.SetupToken, lookup)
	case isCodexService(service) && len(request.AuthJSON) != 0:
		if result.Email == "" {
			result.Email = CodexLoginEmail(request.AuthJSON)
		}
		result.Created, err = AddCodexAccount(cfg, service.Name, request.Account, request.Email, request.AuthJSON)
	default:
		err = fmt.Errorf("service %q needs a %s", service.Name, map[bool]string{true: "setup token", false: "ChatGPT login"}[isClaudeService(service)])
	}
	if err != nil {
		writeClaudeProxyError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	writeHubJSON(w, result)
}

// switchServiceAccount selects accountName, or the best account for "auto",
// and returns the selected account.
func switchServiceAccount(ctx context.Context, cfg Config, serviceName, accountName string) (string, error) {
	if accountName == "auto" {
		if _, err := SwitchBest(ctx, cfg, serviceName); err != nil {
			return "", err
		}
	} else if err := SwitchAccount(cfg, serviceName, accountName); err != nil {
		return "", err
	}
	state, err := LoadState(cfg.StatePath)
	if err != nil {
		return "", err
	}
	return state.Service(serviceName).ActiveAccount, nil
}

func writeHubJSON(w http.ResponseWriter, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(value)
}

// hubCall sends one management request to the hub and decodes its answer.
// The hub's error message is returned as the error.
func hubCall(ctx context.Context, hubURL, credential, method, path string, body, out any) error {
	origin, err := parseProxyUpstream(hubURL)
	if err != nil {
		return fmt.Errorf("hub_url: %w", err)
	}
	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(data)
	}
	ctx, cancel := context.WithTimeout(ctx, hubManageWindow)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, origin.String()+path, reader)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+credential)
	req.Header.Set("Content-Type", "application/json")
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	resp, err := (&http.Client{Transport: transport}).Do(req)
	if err != nil {
		return fmt.Errorf("hub at %s did not answer", origin.Host)
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(resp.Body, hubRequestLimit))
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		var failure struct {
			Error struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if json.Unmarshal(data, &failure) == nil && failure.Error.Message != "" {
			return fmt.Errorf("hub: %s", failure.Error.Message)
		}
		return fmt.Errorf("hub answered %s", resp.Status)
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(data, out); err != nil {
		return errors.New("hub returned a malformed answer")
	}
	return nil
}

// HubAddAccount adds or replaces an account on the hub.
func HubAddAccount(ctx context.Context, hubURL, credential string, request HubAccountRequest) (HubAccountResult, error) {
	var result HubAccountResult
	err := hubCall(ctx, hubURL, credential, http.MethodPost, hubAccountsPath, request, &result)
	return result, err
}

// HubSwitch selects an account on the hub; "auto" picks the best one.
func HubSwitch(ctx context.Context, hubURL, credential, account string) (HubSwitchResult, error) {
	var result HubSwitchResult
	err := hubCall(ctx, hubURL, credential, http.MethodPost, hubSwitchPath, map[string]string{"account": account}, &result)
	return result, err
}

// HubRemoveAccount unregisters an account on the hub and deletes its token.
func HubRemoveAccount(ctx context.Context, hubURL, credential, account string, force bool) error {
	query := url.Values{"account": {account}}
	if force {
		query.Set("force", "1")
	}
	return hubCall(ctx, hubURL, credential, http.MethodDelete, hubAccountsPath+"?"+query.Encode(), nil, nil)
}

// HubHealthInfo is what a hub's health check reports.
type HubHealthInfo struct {
	Service string `json:"service"`
	Version string `json:"version"`
}

// writeProxyHealth answers an authenticated health check.
func writeProxyHealth(w http.ResponseWriter, service ServiceConfig) {
	writeHubJSON(w, struct {
		HubHealthInfo
		Proxy string `json:"proxy"`
	}{HubHealthInfo{Service: service.Name, Version: Version()}, "subswapper"})
}

// HubHealth checks that the hub at hubURL answers and accepts credential.
func HubHealth(ctx context.Context, hubURL, credential string) (HubHealthInfo, error) {
	origin, err := parseProxyUpstream(hubURL)
	if err != nil {
		return HubHealthInfo{}, fmt.Errorf("hub_url: %w", err)
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, origin.String()+claudeProxyHealthPath, nil)
	if err != nil {
		return HubHealthInfo{}, err
	}
	req.Header.Set("Authorization", "Bearer "+credential)
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	resp, err := (&http.Client{Transport: transport}).Do(req)
	if err != nil {
		return HubHealthInfo{}, fmt.Errorf("hub at %s did not answer", hubURL)
	}
	defer func() { _ = resp.Body.Close() }()
	switch {
	case resp.StatusCode == http.StatusUnauthorized:
		return HubHealthInfo{}, fmt.Errorf("hub at %s rejected this machine's credential", hubURL)
	case resp.StatusCode != http.StatusOK:
		return HubHealthInfo{}, fmt.Errorf("hub at %s answered %s", hubURL, resp.Status)
	}
	var health HubHealthInfo
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<10)).Decode(&health); err != nil {
		return HubHealthInfo{}, fmt.Errorf("hub at %s returned a malformed health check", hubURL)
	}
	return health, nil
}
