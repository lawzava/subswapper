package subswapper

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httputil"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const hubBundleVersion = 1

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
	path := ExpandPath(configPath)
	var raw Config
	data, err := os.ReadFile(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		raw.Monitor.Interval.Duration = 5 * time.Minute
	case err != nil:
		return nil, err
	default:
		decoder := json.NewDecoder(bytes.NewReader(data))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&raw); err != nil {
			return nil, fmt.Errorf("parse %s: %w", filepath.Base(path), err)
		}
	}
	names := make([]string, 0, len(bundle.Services))
	for _, imported := range bundle.Services {
		index := -1
		for candidate := range raw.Services {
			if raw.Services[candidate].Name == imported.Name {
				index = candidate
			}
		}
		if index < 0 {
			raw.Services = append(raw.Services, ServiceConfig{Name: imported.Name, Kind: imported.Kind})
			index = len(raw.Services) - 1
		}
		service := &raw.Services[index]
		kind := strings.ToLower(service.Kind)
		if kind == "" {
			kind = strings.ToLower(service.Name)
		}
		if kind == "claude-code" {
			kind = "claude"
		}
		if kind != imported.Kind {
			return nil, fmt.Errorf("service %q is kind %q here but %q on the hub", service.Name, kind, imported.Kind)
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
	cfg := raw
	cfg.Services = append([]ServiceConfig(nil), raw.Services...)
	cfg.ApplyDefaults()
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	// Credentials go first: a launch must never find hub_url without them.
	for _, imported := range bundle.Services {
		if err := storeHubCredential(cfg, imported); err != nil {
			return nil, err
		}
	}
	encoded, err := json.MarshalIndent(raw, "", "  ")
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	if err := writePrivateFile(path, append(encoded, '\n')); err != nil {
		return nil, err
	}
	return names, nil
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
