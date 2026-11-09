package subswapper

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"regexp"
	"slices"
	"time"
)

const (
	codexProxyReadinessPath       = "/subswapper/codex/readiness"
	codexProxyReadinessCapability = "codex-readiness-v1"
)

var openCodePlaceholderPattern = regexp.MustCompile(`^[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+$`)

// OpenCodeRoute contains locations, never credential values. OpenCode needs a
// persistent loopback proxy/relay because no launcher owns its server lifetime.
type OpenCodeRoute struct {
	Origin          string
	PlaceholderPath string
	Service         string
}

func OpenCodeCodexRoute(cfg Config, serviceName string) (OpenCodeRoute, error) {
	service, ok := cfg.Service(serviceName)
	host, _, _ := net.SplitHostPort(service.ProxyListen)
	ip := net.ParseIP(host)
	if !ok || service.Disabled || !isCodexService(service) || ip == nil || !ip.IsLoopback() || validateLoopbackListen(service.ProxyListen) != nil {
		return OpenCodeRoute{}, errors.New("OpenCode requires a configured Codex loopback proxy_listen or fixed relay")
	}
	return OpenCodeRoute{Origin: "http://" + service.ProxyListen, PlaceholderPath: codexProxyPlaceholderPath(cfg, serviceName), Service: serviceName}, nil
}

// LoadExistingCodexProxyPlaceholder never creates a credential or lock file.
// It checks trusted ancestors and pins the descriptor before reading.
func LoadExistingCodexProxyPlaceholder(cfg Config, serviceName string) (CodexProxyPlaceholder, error) {
	unsafe := errors.New("unsafe, unreadable, or malformed Codex placeholder")
	path := codexProxyPlaceholderPath(cfg, serviceName)
	if !filepath.IsAbs(path) {
		return CodexProxyPlaceholder{}, unsafe
	}
	file, err := openOpenCodePlaceholder(path)
	if err != nil {
		return CodexProxyPlaceholder{}, unsafe
	}
	defer func() { _ = file.Close() }()
	data, err := io.ReadAll(io.LimitReader(file, 16*1024+1))
	var value CodexProxyPlaceholder
	if err != nil || len(data) > 16*1024 || json.Unmarshal(data, &value) != nil || value.AccountID != codexProxyPlaceholderAccountID || !openCodePlaceholderPattern.MatchString(value.Token) {
		return CodexProxyPlaceholder{}, unsafe
	}
	return value, nil
}

// CheckOpenCodeProxy performs authenticated, read-only local checks. Unlike
// status, readiness does not run probes, refresh logins, or persist state.
func CheckOpenCodeProxy(ctx context.Context, route OpenCodeRoute, placeholder CodexProxyPlaceholder) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	get := func(path string, target any) bool {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, route.Origin+path, nil)
		if err != nil {
			return false
		}
		req.Header.Set("Authorization", "Bearer "+placeholder.Token)
		resp, err := client.Do(req)
		if err != nil {
			return false
		}
		defer func() { _ = resp.Body.Close() }()
		return resp.StatusCode == http.StatusOK && json.NewDecoder(io.LimitReader(resp.Body, 4096)).Decode(target) == nil
	}
	var health struct {
		Service      string   `json:"service"`
		Proxy        string   `json:"proxy"`
		Capabilities []string `json:"capabilities"`
	}
	if !get(claudeProxyHealthPath, &health) || health.Proxy != "subswapper" || health.Service != route.Service {
		return errors.New("authenticated proxy health failed")
	}
	// Older proxies forward unknown routes with real account credentials and
	// can persist observations. Never probe readiness without this capability.
	if !slices.Contains(health.Capabilities, codexProxyReadinessCapability) {
		return errors.New("upgrade the Codex proxy or hub for read-only readiness support")
	}
	var readiness struct {
		Service string `json:"service"`
		Ready   bool   `json:"selected_ready"`
	}
	if !get(codexProxyReadinessPath, &readiness) || readiness.Service != route.Service || !readiness.Ready {
		return errors.New("selected account not ready or read-only readiness check failed")
	}
	return nil
}

func codexSelectedReady(cfg Config, service ServiceConfig) bool {
	state, err := LoadState(cfg.StatePath)
	if err != nil {
		return false
	}
	ss := state.Service(service.Name)
	account, ok := ss.Accounts[ss.ActiveAccount]
	if !ok || (account.CredentialsError != "" && time.Now().Before(account.FetchBackoffUntil)) {
		return false
	}
	usage, known := codexProxyKnownUsage(account)
	if !known || usage.Exhausted() {
		return false
	}
	_, _, usable := readCodexProxyCredentials(filepath.Join(AccountDir(cfg, service.Name, ss.ActiveAccount), "auth.json"))
	return usable
}
