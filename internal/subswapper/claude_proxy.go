package subswapper

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
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
	"strconv"
	"strings"
	"time"
)

const (
	claudeUsageSourceProxy = "proxy"
	// claudeProxyRequestBodyLimit bounds how much of a request is buffered so
	// it can be replayed against another account after a rejection.
	claudeProxyRequestBodyLimit = 64 << 20
	// claudeProxySecretPrefix makes the placeholder look like a setup token so
	// Claude's auth-source detection treats it as an OAuth token.
	claudeProxySecretPrefix    = "sk-ant-oat01-"
	claudeProxyHealthPath      = "/subswapper/health"
	defaultClaudeProxyUpstream = "https://api.anthropic.com"
	claudeRateLimitHeaderBase  = "anthropic-ratelimit-unified-"
)

var claudeProxyNow = time.Now

var claudeProxyHopByHopHeaders = []string{
	"Connection",
	"Keep-Alive",
	"Proxy-Authenticate",
	"Proxy-Authorization",
	"Proxy-Connection",
	"Te",
	"Trailer",
	"Transfer-Encoding",
	"Upgrade",
}

// ClaudeProxy rewrites the bearer token of every request to the selected
// account's setup token and records the rate-limit headers Anthropic returns.
type ClaudeProxy struct {
	cfg      Config
	service  ServiceConfig
	secret   string
	upstream *url.URL
	client   *http.Client
	logf     func(format string, args ...any)
}

type claudeProxyRoute struct {
	Account   string
	Token     string
	Revision  string
	Active    bool
	Score     float64
	Exhausted bool
}

type claudeRateLimitObservation struct {
	Usage    UsageSnapshot
	HasUsage bool
	Rejected bool
}

func validateLoopbackListen(listen string) error {
	host, port, err := net.SplitHostPort(listen)
	if err != nil {
		return errors.New("must be host:port")
	}
	number, err := strconv.Atoi(port)
	if err != nil || number <= 0 || number > 65535 {
		return errors.New("port must be a fixed number between 1 and 65535")
	}
	if host == "localhost" {
		return nil
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return errors.New("host must be a loopback address; the proxy carries account tokens")
	}
	return nil
}

func parseProxyUpstream(raw string) (*url.URL, error) {
	parsed, err := url.Parse(raw)
	if err != nil {
		return nil, errors.New("must be a valid URL")
	}
	if (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return nil, errors.New("must be an http or https origin")
	}
	if parsed.Path != "" || parsed.RawQuery != "" || parsed.Fragment != "" || parsed.User != nil {
		return nil, errors.New("must be an origin without path, query, or credentials")
	}
	return parsed, nil
}

// ValidateClaudeProxyListen checks a listen address supplied on the command line.
func ValidateClaudeProxyListen(listen string) error {
	return validateLoopbackListen(listen)
}

// ClaudeProxyBaseURL is the ANTHROPIC_BASE_URL a launched process uses.
func ClaudeProxyBaseURL(listen string) string {
	host, port, err := net.SplitHostPort(listen)
	if err != nil {
		return "http://" + listen
	}
	return "http://" + net.JoinHostPort(host, port)
}

func claudeProxySecretPath(cfg Config, serviceName string) string {
	return filepath.Join(claudeSetupTokenRoot(cfg), safeName(serviceName), "proxy-secret.json")
}

// LoadOrCreateClaudeProxySecret returns the shared secret that launched
// processes present to the proxy. It lives beside the setup tokens with the
// same private-directory checks and never contains a real token.
func LoadOrCreateClaudeProxySecret(cfg Config, serviceName string) (string, error) {
	service, ok := cfg.Service(serviceName)
	if !ok {
		return "", fmt.Errorf("service %q not found", serviceName)
	}
	if !isClaudeService(service) || !service.UsesAccountHomes() {
		return "", fmt.Errorf("service %q does not support the Claude proxy", serviceName)
	}
	lock, err := AcquireStateLock(context.Background(), cfg)
	if err != nil {
		return "", err
	}
	defer lock.Release()

	root := claudeSetupTokenRoot(cfg)
	if err := os.MkdirAll(filepath.Dir(root), 0o700); err != nil {
		return "", err
	}
	for _, path := range []string{root, filepath.Join(root, safeName(serviceName))} {
		if err := ensurePrivateSetupTokenDirectory(path); err != nil {
			return "", err
		}
	}
	path := claudeProxySecretPath(cfg, serviceName)
	if secret, exists, err := readClaudeProxySecret(path); err != nil {
		return "", err
	} else if exists {
		return secret, nil
	}
	var value [32]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", errors.New("generate Claude proxy secret")
	}
	secret := claudeProxySecretPrefix + hex.EncodeToString(value[:])
	data, err := json.Marshal(struct {
		Secret string `json:"secret"`
	}{Secret: secret})
	if err != nil {
		return "", errors.New("encode Claude proxy secret")
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".proxy-secret-*")
	if err != nil {
		return "", err
	}
	tmpPath := tmp.Name()
	defer func() { _ = os.Remove(tmpPath) }()
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return "", err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return "", err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return "", err
	}
	if err := tmp.Close(); err != nil {
		return "", err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return "", errors.New("replace Claude proxy secret file")
	}
	return secret, nil
}

func readClaudeProxySecret(path string) (string, bool, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
		return "", false, ErrClaudeSetupTokenStorageUnsafe
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", false, err
	}
	var envelope struct {
		Secret string `json:"secret"`
	}
	if json.Unmarshal(data, &envelope) != nil || !strings.HasPrefix(envelope.Secret, claudeProxySecretPrefix) {
		return "", false, errors.New("claude proxy secret file is malformed")
	}
	return envelope.Secret, true, nil
}

// ClaudeProxyReachable reports whether a proxy that knows this secret is
// serving the configured listen address.
func ClaudeProxyReachable(service ServiceConfig, secret string) bool {
	if !service.ClaudeProxyEnabled() {
		return false
	}
	client := &http.Client{Timeout: time.Second}
	req, err := http.NewRequest(http.MethodGet, ClaudeProxyBaseURL(service.ProxyListen)+claudeProxyHealthPath, nil)
	if err != nil {
		return false
	}
	req.Header.Set("Authorization", "Bearer "+secret)
	resp, err := client.Do(req)
	if err != nil {
		return false
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<10))
	return resp.StatusCode == http.StatusOK
}

func NewClaudeProxy(cfg Config, serviceName string, logf func(format string, args ...any)) (*ClaudeProxy, error) {
	service, ok := cfg.Service(serviceName)
	if !ok {
		return nil, fmt.Errorf("service %q not found", serviceName)
	}
	if !service.ClaudeProxyEnabled() {
		return nil, fmt.Errorf("service %q has no proxy_listen configured", serviceName)
	}
	upstreamRaw := service.ProxyUpstream
	if upstreamRaw == "" {
		upstreamRaw = defaultClaudeProxyUpstream
	}
	upstream, err := parseProxyUpstream(upstreamRaw)
	if err != nil {
		return nil, fmt.Errorf("proxy upstream: %w", err)
	}
	secret, err := LoadOrCreateClaudeProxySecret(cfg, serviceName)
	if err != nil {
		return nil, err
	}
	if logf == nil {
		logf = func(string, ...any) {}
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	// The client's Accept-Encoding is forwarded verbatim, so the body must be
	// relayed untouched rather than decompressed here.
	transport.DisableCompression = true
	transport.ResponseHeaderTimeout = 5 * time.Minute
	return &ClaudeProxy{
		cfg:      cfg,
		service:  service,
		secret:   secret,
		upstream: upstream,
		client: &http.Client{
			Transport: transport,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
		logf: logf,
	}, nil
}

// Serve listens until ctx is cancelled. Streaming responses get a short grace
// period before the listener is closed.
func (p *ClaudeProxy) Serve(ctx context.Context) error {
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

func (p *ClaudeProxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !p.authorized(r) {
		writeClaudeProxyError(w, http.StatusUnauthorized, "authentication_error", "subswapper proxy secret mismatch")
		return
	}
	if r.URL.Path == claudeProxyHealthPath {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"service":"` + p.service.Name + `","proxy":"subswapper"}` + "\n"))
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
	routes, err := claudeProxyRoutes(p.cfg, p.service)
	if err != nil {
		p.logf("claude proxy: route lookup failed: %v", err)
		writeClaudeProxyError(w, http.StatusServiceUnavailable, "api_error", "subswapper route lookup failed")
		return
	}
	if len(routes) == 0 {
		writeClaudeProxyError(w, http.StatusServiceUnavailable, "api_error", "no Claude account has a usable setup token")
		return
	}

	for index, route := range routes {
		resp, err := p.forward(r, route, body)
		if err != nil {
			if r.Context().Err() != nil {
				return
			}
			p.logf("claude proxy: upstream request failed for %s: %v", route.Account, sanitizeProbeError(err))
			writeClaudeProxyError(w, http.StatusBadGateway, "api_error", "subswapper proxy could not reach the Anthropic API")
			return
		}
		observation := parseClaudeRateLimitHeaders(resp.Header, claudeProxyNow().UTC())
		unauthorized := resp.StatusCode == http.StatusUnauthorized
		rejected := resp.StatusCode == http.StatusTooManyRequests || observation.Rejected
		retry := (unauthorized || rejected) && index < len(routes)-1
		if err := recordClaudeProxyObservation(p.cfg, p.service, route, observation, unauthorized, !retry && !unauthorized && !rejected); err != nil {
			p.logf("claude proxy: record usage for %s: %v", route.Account, err)
		}
		if retry {
			reason := "rate limit rejected"
			if unauthorized {
				reason = "token rejected"
			}
			p.logf("claude proxy: %s for %s; retrying with %s", reason, route.Account, routes[index+1].Account)
			_ = resp.Body.Close()
			continue
		}
		if !route.Active && !unauthorized && !rejected {
			p.logf("claude proxy: switched %s to %s", p.service.Name, route.Account)
		}
		relayClaudeProxyResponse(w, resp)
		return
	}
}

func (p *ClaudeProxy) authorized(r *http.Request) bool {
	header := r.Header.Get("Authorization")
	const prefix = "Bearer "
	if len(header) <= len(prefix) || !strings.EqualFold(header[:len(prefix)], prefix) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(header[len(prefix):]), []byte(p.secret)) == 1
}

func (p *ClaudeProxy) forward(r *http.Request, route claudeProxyRoute, body []byte) (*http.Response, error) {
	target := *p.upstream
	target.Path = r.URL.Path
	target.RawPath = r.URL.RawPath
	target.RawQuery = r.URL.RawQuery
	req, err := http.NewRequestWithContext(r.Context(), r.Method, target.String(), bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.ContentLength = int64(len(body))
	req.Header = cloneClaudeProxyHeaders(r.Header)
	req.Header.Del("Authorization")
	req.Header.Del("X-Api-Key")
	req.Header.Set("Authorization", "Bearer "+route.Token)
	req.Host = p.upstream.Host
	return p.client.Do(req)
}

func cloneClaudeProxyHeaders(source http.Header) http.Header {
	result := make(http.Header, len(source))
	for key, values := range source {
		if isClaudeProxyHopByHop(key) {
			continue
		}
		result[key] = append([]string(nil), values...)
	}
	return result
}

func isClaudeProxyHopByHop(key string) bool {
	for _, header := range claudeProxyHopByHopHeaders {
		if strings.EqualFold(key, header) {
			return true
		}
	}
	return false
}

func relayClaudeProxyResponse(w http.ResponseWriter, resp *http.Response) {
	defer func() { _ = resp.Body.Close() }()
	header := w.Header()
	for key, values := range resp.Header {
		if isClaudeProxyHopByHop(key) {
			continue
		}
		header[key] = append([]string(nil), values...)
	}
	w.WriteHeader(resp.StatusCode)
	controller := http.NewResponseController(w)
	buffer := make([]byte, 32<<10)
	for {
		n, err := resp.Body.Read(buffer)
		if n > 0 {
			if _, writeErr := w.Write(buffer[:n]); writeErr != nil {
				return
			}
			// Server-sent events must reach the client as they arrive.
			_ = controller.Flush()
		}
		if err != nil {
			return
		}
	}
}

func writeClaudeProxyError(w http.ResponseWriter, status int, kind, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	payload, _ := json.Marshal(map[string]any{
		"type":  "error",
		"error": map[string]string{"type": kind, "message": message},
	})
	_, _ = w.Write(append(payload, '\n'))
}

// parseClaudeRateLimitHeaders reads the unified rate-limit headers Anthropic
// attaches to subscription responses. Utilization is a 0-1 fraction and
// resets are Unix seconds.
func parseClaudeRateLimitHeaders(header http.Header, observedAt time.Time) claudeRateLimitObservation {
	observation := claudeRateLimitObservation{}
	for _, name := range []string{"status", "5h-status", "7d-status"} {
		if strings.EqualFold(strings.TrimSpace(header.Get(claudeRateLimitHeaderBase+name)), "rejected") {
			observation.Rejected = true
		}
	}
	fiveHour, fiveHourOK := parseClaudeRateLimitWindow(header, "5h")
	weekly, weeklyOK := parseClaudeRateLimitWindow(header, "7d")
	if fiveHourOK && weeklyOK {
		observation.HasUsage = true
		observation.Usage = UsageSnapshot{
			FiveHour:   fiveHour,
			Weekly:     weekly,
			ObservedAt: observedAt,
			Source:     claudeUsageSourceProxy,
		}
	}
	return observation
}

func parseClaudeRateLimitWindow(header http.Header, name string) (LimitWindow, bool) {
	utilizationRaw := strings.TrimSpace(header.Get(claudeRateLimitHeaderBase + name + "-utilization"))
	resetRaw := strings.TrimSpace(header.Get(claudeRateLimitHeaderBase + name + "-reset"))
	if utilizationRaw == "" || resetRaw == "" {
		return LimitWindow{}, false
	}
	utilization, err := strconv.ParseFloat(utilizationRaw, 64)
	if err != nil || math.IsNaN(utilization) || math.IsInf(utilization, 0) || utilization < 0 {
		return LimitWindow{}, false
	}
	resetSeconds, err := strconv.ParseInt(resetRaw, 10, 64)
	if err != nil || resetSeconds <= 0 {
		return LimitWindow{}, false
	}
	percent := math.Min(utilization*100, 100)
	if strings.EqualFold(strings.TrimSpace(header.Get(claudeRateLimitHeaderBase+name+"-status")), "rejected") {
		percent = 100
	}
	return LimitWindow{Pct: &percent, ResetsAt: time.Unix(resetSeconds, 0).UTC()}, true
}

// claudeProxyRoutes lists accounts with usable setup tokens: the selected
// account first, then the least-used alternatives, exhausted accounts last.
func claudeProxyRoutes(cfg Config, service ServiceConfig) ([]claudeProxyRoute, error) {
	lock, err := AcquireStateLock(context.Background(), cfg)
	if err != nil {
		return nil, err
	}
	state, err := LoadState(cfg.StatePath)
	lock.Release()
	if err != nil {
		return nil, err
	}
	now := claudeProxyNow().UTC()
	serviceState := state.Service(service.Name)
	routes := make([]claudeProxyRoute, 0, len(serviceState.Accounts))
	for name, account := range serviceState.Accounts {
		if account.SetupTokenRevision == "" {
			continue
		}
		if account.CredentialsError != "" && now.Before(account.FetchBackoffUntil) {
			continue
		}
		token, status, err := LoadClaudeSetupTokenWithStatus(cfg, service.Name, name)
		if err != nil || !status.Usable {
			continue
		}
		route := claudeProxyRoute{
			Account:  name,
			Token:    token,
			Revision: status.Revision,
			Active:   serviceState.ActiveAccount == name,
			Score:    math.Inf(1),
		}
		if usage, ok := claudeProxyKnownUsage(account, status.Revision); ok {
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

func claudeProxyKnownUsage(account AccountState, revision string) (UsageSnapshot, bool) {
	if trustedClaudeProxyUsage(account.ProxyUsage, revision) {
		return account.ProxyUsage, true
	}
	usage := account.Usage
	if usage.TokenRevision != "" && usage.TokenRevision == revision && usage.HasCoreLimits() {
		return usage, true
	}
	return UsageSnapshot{}, false
}

// trustedClaudeProxyUsage has no age limit: an unused account's windows can
// only fall, LimitWindow.Ratio zeroes a window once its reset passes, and the
// proxy re-verifies on the first real response.
func trustedClaudeProxyUsage(usage UsageSnapshot, revision string) bool {
	return usage.Source == claudeUsageSourceProxy &&
		usage.TokenRevision != "" && usage.TokenRevision == revision &&
		!usage.ObservedAt.IsZero() && usage.HasCoreLimits() &&
		!usage.FiveHour.ResetsAt.IsZero() && !usage.Weekly.ResetsAt.IsZero()
}

// recordClaudeProxyObservation stores what one real response revealed about
// an account. switchTo makes the account the selected route after a failover.
func recordClaudeProxyObservation(cfg Config, service ServiceConfig, route claudeProxyRoute, observation claudeRateLimitObservation, unauthorized, switchTo bool) error {
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
	if !ok || account.SetupTokenRevision != route.Revision {
		return errors.New("claude account changed during the proxied request")
	}
	now := claudeProxyNow().UTC()
	changed := false
	if observation.HasUsage {
		usage := observation.Usage
		usage.TokenRevision = route.Revision
		account.ProxyUsage = usage
		changed = true
	}
	if unauthorized {
		account.CredentialsError = "setup token authentication rejected"
		account.FetchBackoffUntil = now.Add(credentialsErrorBackoff)
		account.LastProbeError = ""
		// A newer probe timestamp stops an in-flight monitor merge from
		// clearing the rejection with its older result.
		account.LastProbeStartedAt = now
		changed = true
	} else if account.CredentialsError != "" && observation.HasUsage {
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
