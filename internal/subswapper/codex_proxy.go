package subswapper

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	// codexProxyPlaceholderAccountID is the identity a proxied Codex process
	// believes it has. Codex reads it from the placeholder JWT and sends it as
	// chatgpt-account-id; the proxy replaces it with the real account.
	codexProxyPlaceholderAccountID = "subswapper-proxy"
	// codexProxyPlaceholderRefresh marks an auth.json that holds no real
	// tokens, so a refresh attempt fails instead of touching a real session.
	codexProxyPlaceholderRefresh = "subswapper-proxy-placeholder"
	// CodexProxyProviderID is the model provider a proxied launch selects.
	// The built-in "openai" provider cannot be overridden, so a custom one
	// with requires_openai_auth carries the ChatGPT identity to the proxy.
	CodexProxyProviderID       = "subswapper"
	defaultCodexProxyUpstream  = "https://chatgpt.com"
	codexProxyAccountHeader    = "Chatgpt-Account-Id"
	codexProxyUsagePath        = "/backend-api/wham/usage"
	codexProxyErrorBodyLimit   = 1 << 20
	codexProxyUsageTTL         = 5 * time.Minute
	codexProxyPlaceholderFile  = "proxy-placeholder.json"
	codexProxyUsageFetchLimit  = 1 << 20
	codexProxyUsageFetchWindow = 10 * time.Second
)

var codexProxyNow = time.Now

// codexQuotaMarkers are the error codes the ChatGPT backend attaches to a
// 429 that means a subscription window is used up. Any other 429 is a
// transient throttle that another account may not share.
var codexQuotaMarkers = []string{"usage_limit_reached", "rate_limit_reached", "usage_not_included"}

// ErrCodexRuntimeAuthIsReal reports that the runtime home's auth.json holds a
// real ChatGPT login, which a proxied launch must not keep: the process would
// refresh that session on its own and invalidate the registered copy.
var ErrCodexRuntimeAuthIsReal = errors.New("codex runtime home auth.json holds a real ChatGPT login")

// CodexProxyPlaceholder is the JWT-shaped token a proxied Codex process
// presents. It authenticates the process to the proxy and nothing else.
type CodexProxyPlaceholder struct {
	Token     string `json:"token"`
	AccountID string `json:"account_id"`
}

// CodexProxy rewrites the ChatGPT identity of every authenticated request to
// the selected account's tokens and fails over on quota rejections.
type CodexProxy struct {
	cfg         Config
	service     ServiceConfig
	placeholder CodexProxyPlaceholder
	upstream    *url.URL
	client      *http.Client
	logf        func(format string, args ...any)

	usageMu       sync.Mutex
	usageInFlight map[string]struct{}
}

type codexProxyRoute struct {
	Account   string
	Token     string
	AccountID string
	Active    bool
	Score     float64
	Exhausted bool
}

type codexProxyAuthFile struct {
	AuthMode     string  `json:"auth_mode"`
	OpenAIAPIKey *string `json:"OPENAI_API_KEY"`
	Tokens       struct {
		IDToken      string `json:"id_token"`
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		AccountID    string `json:"account_id"`
	} `json:"tokens"`
	LastRefresh string `json:"last_refresh"`
}

type codexWhamUsage struct {
	RateLimit struct {
		PrimaryWindow   *codexWhamWindow `json:"primary_window"`
		SecondaryWindow *codexWhamWindow `json:"secondary_window"`
	} `json:"rate_limit"`
}

type codexWhamWindow struct {
	UsedPercent        float64 `json:"used_percent"`
	LimitWindowSeconds int64   `json:"limit_window_seconds"`
	ResetAt            int64   `json:"reset_at"`
}

func codexProxyPlaceholderPath(cfg Config, serviceName string) string {
	return filepath.Join(claudeSetupTokenRoot(cfg), safeName(serviceName), codexProxyPlaceholderFile)
}

func base64URL(data []byte) string {
	return base64.RawURLEncoding.EncodeToString(data)
}

func base64URLDecode(value string) ([]byte, error) {
	return base64.RawURLEncoding.DecodeString(value)
}

// newCodexProxyPlaceholderToken builds an unsigned JWT with the claims Codex
// reads locally. The signature is random so the token doubles as the shared
// secret; upstream never sees it.
func newCodexProxyPlaceholderToken(now time.Time) (string, error) {
	header, err := json.Marshal(map[string]string{"alg": "RS256", "typ": "JWT"})
	if err != nil {
		return "", err
	}
	claims, err := json.Marshal(map[string]any{
		"iss":   "https://subswapper.invalid",
		"sub":   codexProxyPlaceholderAccountID,
		"email": "subswapper-proxy@localhost",
		"iat":   now.Unix(),
		"exp":   now.AddDate(10, 0, 0).Unix(),
		"https://api.openai.com/auth": map[string]string{
			"chatgpt_account_id": codexProxyPlaceholderAccountID,
			"chatgpt_user_id":    codexProxyPlaceholderAccountID,
			"chatgpt_plan_type":  "pro",
		},
	})
	if err != nil {
		return "", err
	}
	var signature [32]byte
	if _, err := rand.Read(signature[:]); err != nil {
		return "", errors.New("generate Codex proxy placeholder")
	}
	return base64URL(header) + "." + base64URL(claims) + "." + base64URL(signature[:]), nil
}

// LoadOrCreateCodexProxyPlaceholder returns the placeholder a proxied Codex
// process presents. It lives beside the Claude proxy secret with the same
// private-directory checks.
func LoadOrCreateCodexProxyPlaceholder(cfg Config, serviceName string) (CodexProxyPlaceholder, error) {
	service, ok := cfg.Service(serviceName)
	if !ok {
		return CodexProxyPlaceholder{}, fmt.Errorf("service %q not found", serviceName)
	}
	if !isCodexService(service) || !service.UsesAccountHomes() {
		return CodexProxyPlaceholder{}, fmt.Errorf("service %q does not support the Codex proxy", serviceName)
	}
	lock, err := AcquireStateLock(context.Background(), cfg)
	if err != nil {
		return CodexProxyPlaceholder{}, err
	}
	defer lock.Release()

	root := claudeSetupTokenRoot(cfg)
	if err := os.MkdirAll(filepath.Dir(root), 0o700); err != nil {
		return CodexProxyPlaceholder{}, err
	}
	for _, path := range []string{root, filepath.Join(root, safeName(serviceName))} {
		if err := ensurePrivateSetupTokenDirectory(path); err != nil {
			return CodexProxyPlaceholder{}, err
		}
	}
	path := codexProxyPlaceholderPath(cfg, serviceName)
	if placeholder, exists, err := readCodexProxyPlaceholder(path); err != nil {
		return CodexProxyPlaceholder{}, err
	} else if exists {
		return placeholder, nil
	}
	token, err := newCodexProxyPlaceholderToken(codexProxyNow().UTC())
	if err != nil {
		return CodexProxyPlaceholder{}, err
	}
	placeholder := CodexProxyPlaceholder{Token: token, AccountID: codexProxyPlaceholderAccountID}
	data, err := json.Marshal(placeholder)
	if err != nil {
		return CodexProxyPlaceholder{}, errors.New("encode Codex proxy placeholder")
	}
	if err := writePrivateFile(path, data); err != nil {
		return CodexProxyPlaceholder{}, err
	}
	return placeholder, nil
}

func readCodexProxyPlaceholder(path string) (CodexProxyPlaceholder, bool, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return CodexProxyPlaceholder{}, false, nil
	}
	if err != nil {
		return CodexProxyPlaceholder{}, false, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
		return CodexProxyPlaceholder{}, false, ErrClaudeSetupTokenStorageUnsafe
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return CodexProxyPlaceholder{}, false, err
	}
	var placeholder CodexProxyPlaceholder
	if json.Unmarshal(data, &placeholder) != nil || strings.Count(placeholder.Token, ".") != 2 || placeholder.AccountID == "" {
		return CodexProxyPlaceholder{}, false, errors.New("codex proxy placeholder file is malformed")
	}
	return placeholder, true, nil
}

// writePrivateFile replaces path atomically with a 0600 regular file.
func writePrivateFile(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".subswapper-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer func() { _ = os.Remove(tmpPath) }()
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("replace %s", filepath.Base(path))
	}
	return nil
}

// CodexProxyAuthFile renders the auth.json a proxied Codex process reads.
func CodexProxyAuthFile(placeholder CodexProxyPlaceholder, now time.Time) ([]byte, error) {
	var auth codexProxyAuthFile
	auth.AuthMode = "chatgpt"
	auth.Tokens.IDToken = placeholder.Token
	auth.Tokens.AccessToken = placeholder.Token
	auth.Tokens.RefreshToken = codexProxyPlaceholderRefresh
	auth.Tokens.AccountID = placeholder.AccountID
	auth.LastRefresh = now.UTC().Format(time.RFC3339Nano)
	return json.MarshalIndent(auth, "", "  ")
}

// IsCodexProxyAuthFile reports whether data is a placeholder auth.json.
func IsCodexProxyAuthFile(data []byte) bool {
	var auth codexProxyAuthFile
	return json.Unmarshal(data, &auth) == nil && auth.Tokens.RefreshToken == codexProxyPlaceholderRefresh
}

// EnsureCodexProxyAuth installs the placeholder into runtimeHome/auth.json
// when the file is missing or already a placeholder. A real login is left
// alone and reported, because replacing it silently would look like a logout.
func EnsureCodexProxyAuth(runtimeHome string, placeholder CodexProxyPlaceholder) error {
	if runtimeHome == "" {
		return errors.New("codex runtime home is required")
	}
	if err := os.MkdirAll(runtimeHome, 0o700); err != nil {
		return err
	}
	path := filepath.Join(runtimeHome, "auth.json")
	existing, err := os.ReadFile(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
	case err != nil:
		return err
	case !IsCodexProxyAuthFile(existing):
		return ErrCodexRuntimeAuthIsReal
	default:
		var auth codexProxyAuthFile
		if json.Unmarshal(existing, &auth) == nil && auth.Tokens.AccessToken == placeholder.Token {
			return nil
		}
	}
	data, err := CodexProxyAuthFile(placeholder, codexProxyNow())
	if err != nil {
		return err
	}
	return writePrivateFile(path, data)
}

// ReplaceCodexRuntimeAuth moves a real auth.json aside and installs the
// placeholder. It returns the backup path, or "" when nothing was moved.
func ReplaceCodexRuntimeAuth(runtimeHome string, placeholder CodexProxyPlaceholder) (string, error) {
	if runtimeHome == "" {
		return "", errors.New("codex runtime home is required")
	}
	if err := os.MkdirAll(runtimeHome, 0o700); err != nil {
		return "", err
	}
	path := filepath.Join(runtimeHome, "auth.json")
	backup := ""
	existing, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	if err == nil && !IsCodexProxyAuthFile(existing) {
		backup = path + ".subswapper-backup-" + codexProxyNow().UTC().Format("20060102-150405")
		if err := writePrivateFile(backup, existing); err != nil {
			return "", err
		}
	}
	data, err := CodexProxyAuthFile(placeholder, codexProxyNow())
	if err != nil {
		return "", err
	}
	if err := writePrivateFile(path, data); err != nil {
		return "", err
	}
	return backup, nil
}

// CodexProxyReachable reports whether a proxy that knows this placeholder is
// serving the configured listen address.
func CodexProxyReachable(service ServiceConfig, placeholder CodexProxyPlaceholder) bool {
	if !service.CodexProxyEnabled() {
		return false
	}
	return ClaudeProxyReachable(ServiceConfig{Name: service.Name, Kind: "claude", AccountMode: AccountModeHome, ProxyListen: service.ProxyListen}, placeholder.Token)
}

func NewCodexProxy(cfg Config, serviceName string, logf func(format string, args ...any)) (*CodexProxy, error) {
	service, ok := cfg.Service(serviceName)
	if !ok {
		return nil, fmt.Errorf("service %q not found", serviceName)
	}
	if !service.CodexProxyEnabled() {
		return nil, fmt.Errorf("service %q has no proxy_listen configured", serviceName)
	}
	upstreamRaw := service.ProxyUpstream
	if upstreamRaw == "" {
		upstreamRaw = defaultCodexProxyUpstream
	}
	upstream, err := parseProxyUpstream(upstreamRaw)
	if err != nil {
		return nil, fmt.Errorf("proxy upstream: %w", err)
	}
	placeholder, err := LoadOrCreateCodexProxyPlaceholder(cfg, serviceName)
	if err != nil {
		return nil, err
	}
	if logf == nil {
		logf = func(string, ...any) {}
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.DisableCompression = true
	transport.ResponseHeaderTimeout = 5 * time.Minute
	return &CodexProxy{
		cfg:         cfg,
		service:     service,
		placeholder: placeholder,
		upstream:    upstream,
		client: &http.Client{
			Transport: transport,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
		logf:          logf,
		usageInFlight: map[string]struct{}{},
	}, nil
}

// Serve listens until ctx is cancelled.
func (p *CodexProxy) Serve(ctx context.Context) error {
	listener, err := net.Listen("tcp", p.service.ProxyListen)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", p.service.ProxyListen, err)
	}
	server := &http.Server{Handler: p, ReadHeaderTimeout: 10 * time.Second}
	served := make(chan error, 1)
	go func() { served <- server.Serve(listener) }()
	select {
	case err := <-served:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			_ = server.Close()
		}
		<-served
		return nil
	}
}

func (p *CodexProxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Authorization") == "" {
		// Codex sends some backend calls (plugin MCP, connectivity) without
		// credentials. They carry no identity to swap, so relay them as-is.
		p.passThrough(w, r)
		return
	}
	if !p.authorized(r) {
		writeClaudeProxyError(w, http.StatusUnauthorized, "authentication_error", "subswapper proxy placeholder mismatch")
		return
	}
	if r.URL.Path == claudeProxyHealthPath {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"service":"` + p.service.Name + `","proxy":"subswapper"}` + "\n"))
		return
	}
	if strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
		writeClaudeProxyError(w, http.StatusNotImplemented, "invalid_request_error",
			"subswapper codex proxy relays HTTP only; proxied launches set supports_websockets=false")
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, claudeProxyRequestBodyLimit+1))
	if err != nil {
		writeClaudeProxyError(w, http.StatusBadRequest, "invalid_request_error", "request body could not be read")
		return
	}
	if len(body) > claudeProxyRequestBodyLimit {
		writeClaudeProxyError(w, http.StatusRequestEntityTooLarge, "invalid_request_error", "request body exceeds the proxy replay limit")
		return
	}
	routes, err := codexProxyRoutes(p.cfg, p.service)
	if err != nil {
		p.logf("codex proxy: route lookup failed: %v", err)
		writeClaudeProxyError(w, http.StatusServiceUnavailable, "api_error", "subswapper route lookup failed")
		return
	}
	if len(routes) == 0 {
		writeClaudeProxyError(w, http.StatusServiceUnavailable, "api_error", "no Codex account has a usable ChatGPT login")
		return
	}

	// Same policy as the Claude proxy: only a quota rejection or a dead
	// token on the active account makes the fallback the selected route.
	stickyFailover := true
	for index, route := range routes {
		resp, err := p.forward(r, route, body)
		if err != nil {
			if r.Context().Err() != nil {
				return
			}
			p.logf("codex proxy: upstream request failed for %s: %v", route.Account, sanitizeProbeError(err))
			writeClaudeProxyError(w, http.StatusBadGateway, "api_error", "subswapper proxy could not reach the ChatGPT backend")
			return
		}
		unauthorized := resp.StatusCode == http.StatusUnauthorized
		quotaRejected, throttled := false, false
		if resp.StatusCode == http.StatusTooManyRequests {
			errorBody, _ := io.ReadAll(io.LimitReader(resp.Body, codexProxyErrorBodyLimit))
			_ = resp.Body.Close()
			resp.Body = io.NopCloser(bytes.NewReader(errorBody))
			quotaRejected = codexQuotaRejected(errorBody)
			throttled = !quotaRejected
		}
		rejected := quotaRejected || throttled
		retry := (unauthorized || rejected) && index < len(routes)-1
		if route.Active && throttled {
			stickyFailover = false
		}
		switchTo := !retry && !unauthorized && !rejected && stickyFailover
		var usage *UsageSnapshot
		if quotaRejected {
			// The rejection carries no numbers; ask the usage endpoint so the
			// account ranks last with a real reset time.
			if fetched, err := p.fetchUsage(r.Context(), route); err == nil {
				usage = &fetched
			}
		}
		if err := recordCodexProxyObservation(p.cfg, p.service, route, usage, unauthorized, switchTo); err != nil {
			p.logf("codex proxy: record state for %s: %v", route.Account, err)
		}
		if retry {
			reason := "usage limit reached"
			switch {
			case unauthorized:
				reason = "token rejected"
			case throttled:
				reason = "throttled without a usage-limit error"
			}
			p.logf("codex proxy: %s for %s; retrying with %s", reason, route.Account, routes[index+1].Account)
			_ = resp.Body.Close()
			continue
		}
		if switchTo && !route.Active {
			p.logf("codex proxy: switched %s to %s", p.service.Name, route.Account)
		}
		if !unauthorized && !rejected {
			p.refreshUsageIfStale(route)
		}
		relayClaudeProxyResponse(w, resp)
		return
	}
}

func (p *CodexProxy) authorized(r *http.Request) bool {
	header := r.Header.Get("Authorization")
	const prefix = "Bearer "
	if len(header) <= len(prefix) || !strings.EqualFold(header[:len(prefix)], prefix) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(header[len(prefix):]), []byte(p.placeholder.Token)) == 1
}

func (p *CodexProxy) target(r *http.Request) string {
	target := *p.upstream
	target.Path = r.URL.Path
	target.RawPath = r.URL.RawPath
	target.RawQuery = r.URL.RawQuery
	return target.String()
}

func (p *CodexProxy) forward(r *http.Request, route codexProxyRoute, body []byte) (*http.Response, error) {
	req, err := http.NewRequestWithContext(r.Context(), r.Method, p.target(r), bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.ContentLength = int64(len(body))
	req.Header = cloneClaudeProxyHeaders(r.Header)
	req.Header.Del("Authorization")
	req.Header.Del(codexProxyAccountHeader)
	req.Header.Set("Authorization", "Bearer "+route.Token)
	if route.AccountID != "" {
		req.Header.Set(codexProxyAccountHeader, route.AccountID)
	}
	req.Host = p.upstream.Host
	return p.client.Do(req)
}

// passThrough relays a request that carries no credentials. Nothing is
// injected, so the answer is whatever upstream gives an anonymous caller.
func (p *CodexProxy) passThrough(w http.ResponseWriter, r *http.Request) {
	if strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
		writeClaudeProxyError(w, http.StatusNotImplemented, "invalid_request_error", "subswapper codex proxy relays HTTP only")
		return
	}
	req, err := http.NewRequestWithContext(r.Context(), r.Method, p.target(r), r.Body)
	if err != nil {
		writeClaudeProxyError(w, http.StatusBadGateway, "api_error", "subswapper proxy could not build the request")
		return
	}
	req.ContentLength = r.ContentLength
	req.Header = cloneClaudeProxyHeaders(r.Header)
	req.Header.Del(codexProxyAccountHeader)
	req.Host = p.upstream.Host
	resp, err := p.client.Do(req)
	if err != nil {
		if r.Context().Err() != nil {
			return
		}
		writeClaudeProxyError(w, http.StatusBadGateway, "api_error", "subswapper proxy could not reach the ChatGPT backend")
		return
	}
	relayClaudeProxyResponse(w, resp)
}

func codexQuotaRejected(body []byte) bool {
	lower := strings.ToLower(string(body))
	for _, marker := range codexQuotaMarkers {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

// codexProxyRoutes lists accounts whose home holds a ChatGPT login: the
// selected account first, then the least-used alternatives, exhausted last.
func codexProxyRoutes(cfg Config, service ServiceConfig) ([]codexProxyRoute, error) {
	lock, err := AcquireStateLock(context.Background(), cfg)
	if err != nil {
		return nil, err
	}
	state, err := LoadState(cfg.StatePath)
	lock.Release()
	if err != nil {
		return nil, err
	}
	now := codexProxyNow().UTC()
	serviceState := state.Service(service.Name)
	routes := make([]codexProxyRoute, 0, len(serviceState.Accounts))
	for name, account := range serviceState.Accounts {
		if account.CredentialsError != "" && now.Before(account.FetchBackoffUntil) {
			continue
		}
		token, accountID, ok := readCodexProxyCredentials(filepath.Join(AccountDir(cfg, service.Name, name), "auth.json"))
		if !ok {
			continue
		}
		route := codexProxyRoute{
			Account:   name,
			Token:     token,
			AccountID: accountID,
			Active:    serviceState.ActiveAccount == name,
			Score:     math.Inf(1),
		}
		if usage, ok := codexProxyKnownUsage(account); ok {
			route.Score = usage.Score()
			route.Exhausted = usage.Exhausted()
		}
		routes = append(routes, route)
	}
	sort.SliceStable(routes, func(i, j int) bool {
		left, right := routes[i], routes[j]
		if left.Exhausted != right.Exhausted {
			return !left.Exhausted
		}
		if left.Active != right.Active {
			return left.Active
		}
		if left.Score != right.Score {
			return left.Score < right.Score
		}
		return left.Account < right.Account
	})
	return routes, nil
}

func readCodexProxyCredentials(path string) (token, accountID string, ok bool) {
	data, err := os.ReadFile(path)
	if err != nil || IsCodexProxyAuthFile(data) {
		return "", "", false
	}
	var auth codexProxyAuthFile
	if json.Unmarshal(data, &auth) != nil || (auth.AuthMode != "" && auth.AuthMode != "chatgpt") || auth.Tokens.AccessToken == "" {
		return "", "", false
	}
	return auth.Tokens.AccessToken, auth.Tokens.AccountID, true
}

// codexProxyKnownUsage prefers the proxy's own sample when it is at least as
// new as the monitor's app-server probe.
func codexProxyKnownUsage(account AccountState) (UsageSnapshot, bool) {
	proxy, usage := account.ProxyUsage, account.Usage
	if proxy.Source == claudeUsageSourceProxy && proxy.HasLimits() && !proxy.ObservedAt.Before(usage.ObservedAt) {
		return proxy, true
	}
	if usage.HasLimits() {
		return usage, true
	}
	return UsageSnapshot{}, false
}

func (p *CodexProxy) fetchUsage(ctx context.Context, route codexProxyRoute) (UsageSnapshot, error) {
	ctx, cancel := context.WithTimeout(ctx, codexProxyUsageFetchWindow)
	defer cancel()
	target := *p.upstream
	target.Path = codexProxyUsagePath
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target.String(), nil)
	if err != nil {
		return UsageSnapshot{}, err
	}
	req.Header.Set("Authorization", "Bearer "+route.Token)
	if route.AccountID != "" {
		req.Header.Set(codexProxyAccountHeader, route.AccountID)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "subswapper")
	resp, err := p.client.Do(req)
	if err != nil {
		return UsageSnapshot{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(resp.Body, codexProxyUsageFetchLimit))
	if err != nil {
		return UsageSnapshot{}, err
	}
	if resp.StatusCode != http.StatusOK {
		return UsageSnapshot{}, fmt.Errorf("codex usage endpoint returned %d", resp.StatusCode)
	}
	return parseCodexWhamUsage(data, codexProxyNow().UTC())
}

// parseCodexWhamUsage maps the ChatGPT usage windows onto the five-hour and
// weekly slots by their length; a plan may expose only one of them.
func parseCodexWhamUsage(data []byte, observedAt time.Time) (UsageSnapshot, error) {
	var payload codexWhamUsage
	if err := json.Unmarshal(data, &payload); err != nil {
		return UsageSnapshot{}, errors.New("codex usage response is malformed")
	}
	usage := UsageSnapshot{ObservedAt: observedAt, Source: claudeUsageSourceProxy}
	for _, window := range []*codexWhamWindow{payload.RateLimit.PrimaryWindow, payload.RateLimit.SecondaryWindow} {
		if window == nil || window.LimitWindowSeconds <= 0 || window.UsedPercent < 0 || math.IsNaN(window.UsedPercent) {
			continue
		}
		limit := LimitWindow{Pct: PtrFloat64(math.Min(window.UsedPercent, 100))}
		if window.ResetAt > 0 {
			limit.ResetsAt = time.Unix(window.ResetAt, 0).UTC()
		}
		if window.LimitWindowSeconds <= 6*3600 {
			if usage.FiveHour.Pct == nil {
				usage.FiveHour = limit
			}
		} else if usage.Weekly.Pct == nil {
			usage.Weekly = limit
		}
	}
	if !usage.HasLimits() {
		return UsageSnapshot{}, errors.New("codex usage response has no windows")
	}
	return usage, nil
}

// refreshUsageIfStale samples the usage endpoint in the background at most
// once per TTL per account, so routing scores stay current without probes.
func (p *CodexProxy) refreshUsageIfStale(route codexProxyRoute) {
	lock, err := AcquireStateLock(context.Background(), p.cfg)
	if err != nil {
		return
	}
	state, err := LoadState(p.cfg.StatePath)
	lock.Release()
	if err != nil {
		return
	}
	account, ok := state.Service(p.service.Name).Accounts[route.Account]
	if !ok {
		return
	}
	if usage, known := codexProxyKnownUsage(account); known && codexProxyNow().UTC().Sub(usage.ObservedAt) < codexProxyUsageTTL {
		return
	}
	p.usageMu.Lock()
	if _, busy := p.usageInFlight[route.Account]; busy {
		p.usageMu.Unlock()
		return
	}
	p.usageInFlight[route.Account] = struct{}{}
	p.usageMu.Unlock()
	go func() {
		defer func() {
			p.usageMu.Lock()
			delete(p.usageInFlight, route.Account)
			p.usageMu.Unlock()
		}()
		usage, err := p.fetchUsage(context.Background(), route)
		if err != nil {
			p.logf("codex proxy: usage for %s unavailable: %v", route.Account, sanitizeProbeError(err))
			return
		}
		if err := recordCodexProxyObservation(p.cfg, p.service, route, &usage, false, false); err != nil {
			p.logf("codex proxy: record usage for %s: %v", route.Account, err)
		}
	}()
}

// recordCodexProxyObservation stores what one real response revealed about an
// account. switchTo makes the account the selected route after a failover.
func recordCodexProxyObservation(cfg Config, service ServiceConfig, route codexProxyRoute, usage *UsageSnapshot, unauthorized, switchTo bool) error {
	lock, err := AcquireStateLock(context.Background(), cfg)
	if err != nil {
		return err
	}
	defer lock.Release()
	state, err := LoadState(cfg.StatePath)
	if err != nil {
		return err
	}
	serviceState := state.Service(service.Name)
	account, ok := serviceState.Accounts[route.Account]
	if !ok {
		return errors.New("codex account removed during the proxied request")
	}
	now := codexProxyNow().UTC()
	changed := false
	if usage != nil {
		account.ProxyUsage = *usage
		changed = true
	}
	if unauthorized {
		account.CredentialsError = "chatgpt token rejected"
		account.FetchBackoffUntil = now.Add(credentialsErrorBackoff)
		account.LastProbeError = ""
		// A newer probe timestamp stops an in-flight monitor merge from
		// clearing the rejection with its older result.
		account.LastProbeStartedAt = now
		changed = true
	} else if account.CredentialsError != "" && usage != nil {
		account.CredentialsError = ""
		account.FetchBackoffUntil = time.Time{}
		changed = true
	}
	if changed {
		serviceState.Accounts[route.Account] = account
	}
	if switchTo && serviceState.ActiveAccount != route.Account {
		if err := switchServiceFiles(cfg, service, state, route.Account, now); err != nil {
			return err
		}
		changed = true
	}
	if !changed {
		return nil
	}
	return SaveState(cfg.StatePath, state)
}
