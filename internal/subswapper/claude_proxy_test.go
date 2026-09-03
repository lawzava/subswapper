package subswapper

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

type proxyUpstreamCall struct {
	Authorization string
	Path          string
	Body          string
	Accept        string
}

type proxyUpstream struct {
	t        *testing.T
	server   *httptest.Server
	mu       sync.Mutex
	calls    []proxyUpstreamCall
	handlers map[string]func(w http.ResponseWriter, r *http.Request)
}

func newProxyUpstream(t *testing.T) *proxyUpstream {
	t.Helper()
	upstream := &proxyUpstream{t: t, handlers: map[string]func(http.ResponseWriter, *http.Request){}}
	upstream.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		auth := r.Header.Get("Authorization")
		upstream.mu.Lock()
		upstream.calls = append(upstream.calls, proxyUpstreamCall{
			Authorization: auth,
			Path:          r.URL.RequestURI(),
			Body:          string(body),
			Accept:        r.Header.Get("Accept"),
		})
		handler := upstream.handlers[auth]
		upstream.mu.Unlock()
		if handler == nil {
			w.WriteHeader(http.StatusTeapot)
			return
		}
		handler(w, r)
	}))
	t.Cleanup(upstream.server.Close)
	return upstream
}

func (u *proxyUpstream) respond(token string, handler func(w http.ResponseWriter, r *http.Request)) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.handlers["Bearer "+token] = handler
}

func (u *proxyUpstream) recorded() []proxyUpstreamCall {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]proxyUpstreamCall(nil), u.calls...)
}

func rateLimitHeaders(w http.ResponseWriter, fiveHour, weekly float64, status string) {
	reset := time.Now().Add(time.Hour).Unix()
	header := w.Header()
	header.Set("anthropic-ratelimit-unified-status", status)
	header.Set("anthropic-ratelimit-unified-5h-utilization", strconv.FormatFloat(fiveHour, 'f', -1, 64))
	header.Set("anthropic-ratelimit-unified-5h-reset", strconv.FormatInt(reset, 10))
	header.Set("anthropic-ratelimit-unified-7d-utilization", strconv.FormatFloat(weekly, 'f', -1, 64))
	header.Set("anthropic-ratelimit-unified-7d-reset", strconv.FormatInt(reset+6*24*3600, 10))
}

func setupProxyAccounts(t *testing.T, upstreamURL string) (Config, *ClaudeProxy) {
	t.Helper()
	dir := t.TempDir()
	cfg := setupTokenTestConfig(dir)
	cfg.Services[0].ProxyListen = "127.0.0.1:1"
	cfg.Services[0].ProxyUpstream = upstreamURL
	cfg.ApplyDefaults()
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"a", "b"} {
		registerSetupTokenTestAccount(t, cfg, name)
		if _, err := ReplaceClaudeSetupToken(context.Background(), cfg, "claude", name, "setup-token-"+name, nil); err != nil {
			t.Fatal(err)
		}
	}
	selectProxyTestAccount(t, cfg, "a")
	proxy, err := NewClaudeProxy(cfg, "claude", func(string, ...any) {})
	if err != nil {
		t.Fatal(err)
	}
	return cfg, proxy
}

// selectProxyTestAccount changes only the route; setActiveTestAccount
// rebuilds the whole state and would drop the registered tokens.
func selectProxyTestAccount(t *testing.T, cfg Config, accountName string) {
	t.Helper()
	state, err := LoadState(cfg.StatePath)
	if err != nil {
		t.Fatal(err)
	}
	state.Service("claude").ActiveAccount = accountName
	if err := SaveState(cfg.StatePath, state); err != nil {
		t.Fatal(err)
	}
}

func proxyRequest(t *testing.T, proxy *ClaudeProxy, secret, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/messages?beta=true", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+secret)
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Connection", "keep-alive")
	recorder := httptest.NewRecorder()
	proxy.ServeHTTP(recorder, req)
	return recorder
}

func TestClaudeProxyRejectsRequestsWithoutTheSharedSecret(t *testing.T) {
	upstream := newProxyUpstream(t)
	_, proxy := setupProxyAccounts(t, upstream.server.URL)

	recorder := proxyRequest(t, proxy, "setup-token-a", `{}`)
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	if calls := upstream.recorded(); len(calls) != 0 {
		t.Fatalf("upstream was called: %#v", calls)
	}
}

func TestClaudeProxyForwardsWithSelectedTokenAndRecordsUsage(t *testing.T) {
	upstream := newProxyUpstream(t)
	cfg, proxy := setupProxyAccounts(t, upstream.server.URL)
	upstream.respond("setup-token-a", func(w http.ResponseWriter, r *http.Request) {
		rateLimitHeaders(w, 0.46, 0.08, "allowed")
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("event: message_start\ndata: {}\n\n"))
	})

	recorder := proxyRequest(t, proxy, proxy.secret, `{"model":"claude"}`)
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), "message_start") {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	if got := recorder.Header().Get("anthropic-ratelimit-unified-5h-utilization"); got != "0.46" {
		t.Fatalf("rate-limit header was not relayed: %q", got)
	}
	calls := upstream.recorded()
	if len(calls) != 1 || calls[0].Authorization != "Bearer setup-token-a" || calls[0].Path != "/v1/messages?beta=true" ||
		calls[0].Body != `{"model":"claude"}` || calls[0].Accept != "text/event-stream" {
		t.Fatalf("upstream calls = %#v", calls)
	}

	state, err := LoadState(cfg.StatePath)
	if err != nil {
		t.Fatal(err)
	}
	account := state.Service("claude").Accounts["a"]
	usage := account.ProxyUsage
	if usage.Source != claudeUsageSourceProxy || usage.TokenRevision != account.SetupTokenRevision ||
		usage.FiveHour.Pct == nil || *usage.FiveHour.Pct != 46 || usage.Weekly.Pct == nil || *usage.Weekly.Pct != 8 ||
		!usage.FiveHour.ResetsAt.After(time.Now()) {
		t.Fatalf("proxy usage = %#v", usage)
	}
	if state.Service("claude").ActiveAccount != "a" {
		t.Fatalf("active account changed to %q", state.Service("claude").ActiveAccount)
	}
}

func TestClaudeProxyFailsOverOnRejectedRateLimitAndSwitchesRoute(t *testing.T) {
	upstream := newProxyUpstream(t)
	cfg, proxy := setupProxyAccounts(t, upstream.server.URL)
	upstream.respond("setup-token-a", func(w http.ResponseWriter, r *http.Request) {
		rateLimitHeaders(w, 1, 0.5, "rejected")
		w.Header().Set("anthropic-ratelimit-unified-5h-status", "rejected")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"type":"error"}`))
	})
	upstream.respond("setup-token-b", func(w http.ResponseWriter, r *http.Request) {
		rateLimitHeaders(w, 0.1, 0.2, "allowed")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"type":"message"}`))
	})

	recorder := proxyRequest(t, proxy, proxy.secret, `{"model":"claude"}`)
	if recorder.Code != http.StatusOK || recorder.Body.String() != `{"type":"message"}` {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	calls := upstream.recorded()
	if len(calls) != 2 || calls[0].Authorization != "Bearer setup-token-a" || calls[1].Authorization != "Bearer setup-token-b" ||
		calls[1].Body != `{"model":"claude"}` {
		t.Fatalf("upstream calls = %#v", calls)
	}

	state, err := LoadState(cfg.StatePath)
	if err != nil {
		t.Fatal(err)
	}
	service := state.Service("claude")
	if service.ActiveAccount != "b" || service.LastSwitchedAt.IsZero() {
		t.Fatalf("service state = %#v", service)
	}
	if usage := service.Accounts["a"].ProxyUsage; !usage.Exhausted() {
		t.Fatalf("account a usage = %#v", usage)
	}
	if usage := service.Accounts["b"].ProxyUsage; usage.FiveHour.Pct == nil || *usage.FiveHour.Pct != 10 {
		t.Fatalf("account b usage = %#v", usage)
	}

	// The next request goes straight to b without touching a.
	recorder = proxyRequest(t, proxy, proxy.secret, `{"model":"claude"}`)
	if recorder.Code != http.StatusOK {
		t.Fatalf("second status = %d", recorder.Code)
	}
	if calls := upstream.recorded(); len(calls) != 3 || calls[2].Authorization != "Bearer setup-token-b" {
		t.Fatalf("upstream calls = %#v", calls)
	}
}

func TestClaudeProxyMarksRejectedTokenAndRelaysLastFailure(t *testing.T) {
	upstream := newProxyUpstream(t)
	cfg, proxy := setupProxyAccounts(t, upstream.server.URL)
	upstream.respond("setup-token-a", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"type":"error","error":{"type":"authentication_error"}}`))
	})
	upstream.respond("setup-token-b", func(w http.ResponseWriter, r *http.Request) {
		rateLimitHeaders(w, 1, 0.9, "rejected")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"type":"error","error":{"type":"rate_limit_error"}}`))
	})

	recorder := proxyRequest(t, proxy, proxy.secret, `{}`)
	if recorder.Code != http.StatusTooManyRequests || !strings.Contains(recorder.Body.String(), "rate_limit_error") {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	state, err := LoadState(cfg.StatePath)
	if err != nil {
		t.Fatal(err)
	}
	service := state.Service("claude")
	if service.Accounts["a"].CredentialsError == "" || service.Accounts["a"].FetchBackoffUntil.IsZero() {
		t.Fatalf("account a was not marked rejected: %#v", service.Accounts["a"])
	}
	if service.ActiveAccount != "a" {
		t.Fatalf("active account changed to %q on total failure", service.ActiveAccount)
	}

	// The rejected token is skipped until its backoff passes.
	recorder = proxyRequest(t, proxy, proxy.secret, `{}`)
	if recorder.Code != http.StatusTooManyRequests {
		t.Fatalf("second status = %d", recorder.Code)
	}
	if calls := upstream.recorded(); len(calls) != 3 || calls[2].Authorization != "Bearer setup-token-b" {
		t.Fatalf("upstream calls = %#v", calls)
	}
}

func TestClaudeProxyPrefersLeastUsedAlternativeOnFailover(t *testing.T) {
	upstream := newProxyUpstream(t)
	cfg, proxy := setupProxyAccounts(t, upstream.server.URL)
	registerSetupTokenTestAccount(t, cfg, "c")
	if _, err := ReplaceClaudeSetupToken(context.Background(), cfg, "claude", "c", "setup-token-c", nil); err != nil {
		t.Fatal(err)
	}
	state, err := LoadState(cfg.StatePath)
	if err != nil {
		t.Fatal(err)
	}
	for name, pct := range map[string]float64{"b": 70, "c": 20} {
		account := state.Service("claude").Accounts[name]
		account.ProxyUsage = UsageSnapshot{
			FiveHour:      LimitWindow{Pct: PtrFloat64(pct), ResetsAt: time.Now().Add(time.Hour)},
			Weekly:        LimitWindow{Pct: PtrFloat64(pct), ResetsAt: time.Now().Add(24 * time.Hour)},
			ObservedAt:    time.Now().UTC().Add(-48 * time.Hour),
			Source:        claudeUsageSourceProxy,
			TokenRevision: account.SetupTokenRevision,
		}
		state.Service("claude").Accounts[name] = account
	}
	if err := SaveState(cfg.StatePath, state); err != nil {
		t.Fatal(err)
	}
	upstream.respond("setup-token-a", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	})
	upstream.respond("setup-token-c", func(w http.ResponseWriter, r *http.Request) {
		rateLimitHeaders(w, 0.2, 0.2, "allowed")
		w.WriteHeader(http.StatusOK)
	})

	recorder := proxyRequest(t, proxy, proxy.secret, `{}`)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	calls := upstream.recorded()
	if len(calls) != 2 || calls[1].Authorization != "Bearer setup-token-c" {
		t.Fatalf("upstream calls = %#v", calls)
	}
}

func TestClaudeProxyKeepsFableWindowAcrossNonFableResponses(t *testing.T) {
	upstream := newProxyUpstream(t)
	cfg, proxy := setupProxyAccounts(t, upstream.server.URL)
	fableReset := time.Now().Add(6 * 24 * time.Hour).Unix()
	withFable := true
	upstream.respond("setup-token-a", func(w http.ResponseWriter, r *http.Request) {
		rateLimitHeaders(w, 0.1, 0.2, "allowed")
		if withFable {
			w.Header().Set("anthropic-ratelimit-unified-7d_oi-utilization", "0.28")
			w.Header().Set("anthropic-ratelimit-unified-7d_oi-reset", strconv.FormatInt(fableReset, 10))
			w.Header().Set("anthropic-ratelimit-unified-7d_oi-status", "allowed")
		}
		w.WriteHeader(http.StatusOK)
	})

	if recorder := proxyRequest(t, proxy, proxy.secret, `{"model":"claude-fable-5-1"}`); recorder.Code != http.StatusOK {
		t.Fatalf("fable status = %d", recorder.Code)
	}
	state, err := LoadState(cfg.StatePath)
	if err != nil {
		t.Fatal(err)
	}
	usage := state.Service("claude").Accounts["a"].ProxyUsage
	if usage.FableWeekly.Pct == nil || *usage.FableWeekly.Pct != 28 || usage.FableWeekly.ResetsAt.Unix() != fableReset {
		t.Fatalf("fable window after fable response = %#v", usage.FableWeekly)
	}
	if usage.Score() != 0.28 {
		t.Fatalf("score = %v, want the fable window", usage.Score())
	}

	// A Haiku response has no 7d_oi headers and does not consume the window.
	withFable = false
	if recorder := proxyRequest(t, proxy, proxy.secret, `{"model":"claude-haiku-4-5"}`); recorder.Code != http.StatusOK {
		t.Fatalf("haiku status = %d", recorder.Code)
	}
	state, err = LoadState(cfg.StatePath)
	if err != nil {
		t.Fatal(err)
	}
	usage = state.Service("claude").Accounts["a"].ProxyUsage
	if usage.FableWeekly.Pct == nil || *usage.FableWeekly.Pct != 28 || *usage.FiveHour.Pct != 10 {
		t.Fatalf("fable window after haiku response = %#v", usage)
	}

	// The window is scoped to the token revision that produced it.
	upstream.respond("setup-token-a-rotated", func(w http.ResponseWriter, r *http.Request) {
		rateLimitHeaders(w, 0.1, 0.2, "allowed")
		w.WriteHeader(http.StatusOK)
	})
	if _, err := ReplaceClaudeSetupToken(context.Background(), cfg, "claude", "a", "setup-token-a-rotated", nil); err != nil {
		t.Fatal(err)
	}
	if recorder := proxyRequest(t, proxy, proxy.secret, `{"model":"claude-haiku-4-5"}`); recorder.Code != http.StatusOK {
		t.Fatalf("post-rotation status = %d", recorder.Code)
	}
	state, err = LoadState(cfg.StatePath)
	if err != nil {
		t.Fatal(err)
	}
	if usage = state.Service("claude").Accounts["a"].ProxyUsage; usage.FableWeekly.Pct != nil {
		t.Fatalf("fable window survived a token rotation: %#v", usage.FableWeekly)
	}
}

func TestClaudeProxyThrottleWithoutQuotaHeadersDoesNotSwitchRoute(t *testing.T) {
	upstream := newProxyUpstream(t)
	cfg, proxy := setupProxyAccounts(t, upstream.server.URL)
	upstream.respond("setup-token-a", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"type":"error","error":{"type":"rate_limit_error","message":"Error"}}`))
	})
	upstream.respond("setup-token-b", func(w http.ResponseWriter, r *http.Request) {
		rateLimitHeaders(w, 0.1, 0.2, "allowed")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"type":"message"}`))
	})

	recorder := proxyRequest(t, proxy, proxy.secret, `{}`)
	if recorder.Code != http.StatusOK || recorder.Body.String() != `{"type":"message"}` {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	if calls := upstream.recorded(); len(calls) != 2 || calls[1].Authorization != "Bearer setup-token-b" {
		t.Fatalf("upstream calls = %#v", calls)
	}
	state, err := LoadState(cfg.StatePath)
	if err != nil {
		t.Fatal(err)
	}
	service := state.Service("claude")
	if service.ActiveAccount != "a" || !service.LastSwitchedAt.IsZero() {
		t.Fatalf("a bare 429 changed the route: %#v", service)
	}
	if usage := service.Accounts["a"].ProxyUsage; usage.HasLimits() || service.Accounts["a"].CredentialsError != "" {
		t.Fatalf("a bare 429 recorded usage for a: %#v", service.Accounts["a"])
	}
	if usage := service.Accounts["b"].ProxyUsage; usage.FiveHour.Pct == nil || *usage.FiveHour.Pct != 10 {
		t.Fatalf("account b usage = %#v", usage)
	}
}

func TestClaudeProxyRelaysConnectivityCheckWithoutCredentials(t *testing.T) {
	upstream := newProxyUpstream(t)
	_, proxy := setupProxyAccounts(t, upstream.server.URL)
	upstream.mu.Lock()
	upstream.handlers[""] = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Upstream", "hello")
		w.WriteHeader(http.StatusOK)
	}
	upstream.mu.Unlock()

	req := httptest.NewRequest(http.MethodHead, claudeConnectivityPath, nil)
	recorder := httptest.NewRecorder()
	proxy.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusOK || recorder.Header().Get("X-Upstream") != "hello" {
		t.Fatalf("status = %d, headers = %v", recorder.Code, recorder.Header())
	}
	calls := upstream.recorded()
	if len(calls) != 1 || calls[0].Authorization != "" || calls[0].Path != claudeConnectivityPath {
		t.Fatalf("upstream calls = %#v", calls)
	}

	// Any other unauthenticated path still needs the shared secret.
	req = httptest.NewRequest(http.MethodPost, claudeConnectivityPath, strings.NewReader("{}"))
	recorder = httptest.NewRecorder()
	proxy.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("POST status = %d", recorder.Code)
	}
}

func TestClaudeProxyRejectsOversizedBodiesBeforeForwarding(t *testing.T) {
	upstream := newProxyUpstream(t)
	_, proxy := setupProxyAccounts(t, upstream.server.URL)
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", io.LimitReader(zeroReader{}, claudeProxyRequestBodyLimit+1))
	req.Header.Set("Authorization", "Bearer "+proxy.secret)
	recorder := httptest.NewRecorder()
	proxy.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d", recorder.Code)
	}
	if calls := upstream.recorded(); len(calls) != 0 {
		t.Fatalf("upstream was called: %#v", calls)
	}
}

type zeroReader struct{}

func (zeroReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 0
	}
	return len(p), nil
}

func TestClaudeProxySecretIsStablePrivateAndTokenShaped(t *testing.T) {
	dir := t.TempDir()
	cfg := setupTokenTestConfig(dir)
	cfg.ApplyDefaults()
	first, err := LoadOrCreateClaudeProxySecret(cfg, "claude")
	if err != nil {
		t.Fatal(err)
	}
	second, err := LoadOrCreateClaudeProxySecret(cfg, "claude")
	if err != nil {
		t.Fatal(err)
	}
	if first != second || !strings.HasPrefix(first, claudeProxySecretPrefix) || len(first) < len(claudeProxySecretPrefix)+64 {
		t.Fatalf("secret = %q, second = %q", first, second)
	}
	info, err := os.Stat(claudeProxySecretPath(cfg, "claude"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("secret mode = %v", info.Mode().Perm())
	}
	if err := os.Chmod(claudeProxySecretPath(cfg, "claude"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadOrCreateClaudeProxySecret(cfg, "claude"); err == nil {
		t.Fatal("world-readable secret was accepted")
	}
}

func TestClaudeProxyHealthAndReachability(t *testing.T) {
	upstream := newProxyUpstream(t)
	cfg, proxy := setupProxyAccounts(t, upstream.server.URL)
	server := httptest.NewServer(proxy)
	t.Cleanup(server.Close)
	service := cfg.Services[0]
	service.ProxyListen = strings.TrimPrefix(server.URL, "http://")
	if !ClaudeProxyReachable(service, proxy.secret) {
		t.Fatal("proxy reported unreachable")
	}
	if ClaudeProxyReachable(service, "sk-ant-oat01-wrong") {
		t.Fatal("wrong secret reported reachable")
	}
	service.ProxyListen = "127.0.0.1:1"
	if ClaudeProxyReachable(service, proxy.secret) {
		t.Fatal("closed port reported reachable")
	}
}

func TestClaudeProxyServeStopsOnContextCancel(t *testing.T) {
	upstream := newProxyUpstream(t)
	cfg, _ := setupProxyAccounts(t, upstream.server.URL)
	listener := httptest.NewUnstartedServer(nil)
	listen := listener.Listener.Addr().String()
	_ = listener.Listener.Close()
	cfg.Services[0].ProxyListen = listen
	proxy, err := NewClaudeProxy(cfg, "claude", func(string, ...any) {})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- proxy.Serve(ctx) }()
	deadline := time.Now().Add(5 * time.Second)
	for !ClaudeProxyReachable(cfg.Services[0], proxy.secret) {
		if time.Now().After(deadline) {
			t.Fatal("proxy never became reachable")
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
}

func TestParseClaudeRateLimitHeaders(t *testing.T) {
	header := http.Header{}
	header.Set("anthropic-ratelimit-unified-status", "allowed_warning")
	header.Set("anthropic-ratelimit-unified-5h-utilization", "0.46")
	header.Set("anthropic-ratelimit-unified-5h-reset", "1788306600")
	header.Set("anthropic-ratelimit-unified-7d-utilization", "1.2")
	header.Set("anthropic-ratelimit-unified-7d-reset", "1788674400")
	observed := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	observation := parseClaudeRateLimitHeaders(header, observed)
	if !observation.HasUsage || !observation.HasHeaders || observation.Rejected {
		t.Fatalf("observation = %#v", observation)
	}
	usage := observation.Usage
	if *usage.FiveHour.Pct != 46 || *usage.Weekly.Pct != 100 || usage.Source != claudeUsageSourceProxy ||
		!usage.ObservedAt.Equal(observed) || usage.FiveHour.ResetsAt.Unix() != 1788306600 || usage.Weekly.ResetsAt.Unix() != 1788674400 {
		t.Fatalf("usage = %#v", usage)
	}
	if usage.FableWeekly.Pct != nil {
		t.Fatalf("fable window reported without its headers: %#v", usage.FableWeekly)
	}

	// Responses served by a Fable model add the 7d_oi window; the float
	// product 0.14*100 must not leak into the stored percentage.
	header.Set("anthropic-ratelimit-unified-7d_oi-utilization", "0.14")
	header.Set("anthropic-ratelimit-unified-7d_oi-reset", "1788674400")
	header.Set("anthropic-ratelimit-unified-7d_oi-status", "allowed")
	observation = parseClaudeRateLimitHeaders(header, observed)
	if !observation.HasUsage || observation.Rejected || observation.Usage.FableWeekly.Pct == nil ||
		*observation.Usage.FableWeekly.Pct != 14 || observation.Usage.FableWeekly.ResetsAt.Unix() != 1788674400 {
		t.Fatalf("fable usage = %#v", observation.Usage.FableWeekly)
	}
	if observation.Usage.Score() != 1 {
		t.Fatalf("score = %v", observation.Usage.Score())
	}
	header.Set("anthropic-ratelimit-unified-7d_oi-status", "rejected")
	if observation = parseClaudeRateLimitHeaders(header, observed); !observation.Rejected || *observation.Usage.FableWeekly.Pct != 100 {
		t.Fatalf("rejected fable observation = %#v", observation)
	}
	header.Del("anthropic-ratelimit-unified-7d_oi-status")
	header.Del("anthropic-ratelimit-unified-7d_oi-utilization")
	header.Del("anthropic-ratelimit-unified-7d_oi-reset")

	if bare := parseClaudeRateLimitHeaders(http.Header{"Content-Type": {"application/json"}}, observed); bare.HasHeaders || bare.HasUsage || bare.Rejected {
		t.Fatalf("bare observation = %#v", bare)
	}

	// A rejection can arrive while the utilization still reads below 100%;
	// the representative claim names the window that is really exhausted.
	header.Set("anthropic-ratelimit-unified-status", "rejected")
	header.Set("anthropic-ratelimit-unified-5h-utilization", "0.91")
	header.Set("anthropic-ratelimit-unified-7d-reset", "1788674400")
	header.Set("anthropic-ratelimit-unified-7d-utilization", "0.3")
	header.Set("anthropic-ratelimit-unified-representative-claim", "five_hour")
	observation = parseClaudeRateLimitHeaders(header, observed)
	if !observation.Rejected || *observation.Usage.FiveHour.Pct != 100 || *observation.Usage.Weekly.Pct != 30 {
		t.Fatalf("five_hour rejection = %#v", observation.Usage)
	}
	header.Set("anthropic-ratelimit-unified-representative-claim", "seven_day_overage_included")
	header.Set("anthropic-ratelimit-unified-7d_oi-utilization", "0.99")
	header.Set("anthropic-ratelimit-unified-7d_oi-reset", "1788674400")
	observation = parseClaudeRateLimitHeaders(header, observed)
	if *observation.Usage.FiveHour.Pct != 91 || *observation.Usage.Weekly.Pct != 30 || *observation.Usage.FableWeekly.Pct != 100 {
		t.Fatalf("fable rejection = %#v", observation.Usage)
	}
	header.Del("anthropic-ratelimit-unified-representative-claim")
	observation = parseClaudeRateLimitHeaders(header, observed)
	if *observation.Usage.FiveHour.Pct != 100 || *observation.Usage.FableWeekly.Pct != 99 {
		t.Fatalf("unattributed rejection = %#v", observation.Usage)
	}
	header.Del("anthropic-ratelimit-unified-7d_oi-utilization")
	header.Del("anthropic-ratelimit-unified-7d_oi-reset")
	header.Set("anthropic-ratelimit-unified-status", "allowed")
	header.Set("anthropic-ratelimit-unified-5h-utilization", "0.46")
	header.Set("anthropic-ratelimit-unified-7d-utilization", "1.2")

	header.Del("anthropic-ratelimit-unified-7d-reset")
	header.Set("anthropic-ratelimit-unified-7d-status", "rejected")
	observation = parseClaudeRateLimitHeaders(header, observed)
	if observation.HasUsage || !observation.Rejected {
		t.Fatalf("partial observation = %#v", observation)
	}
}

func TestClaudeProxyUsageMakesSetupTokenAccountsSelectableWithoutProbes(t *testing.T) {
	cfg, account := setupTokenUsageAccount(t)
	selectProxyTestAccount(t, cfg, account)
	server := setupTokenUsageServer(t, http.StatusForbidden, `{"error":"missing user:profile"}`)
	useClaudeUsageServer(t, server.URL)

	state, err := LoadState(cfg.StatePath)
	if err != nil {
		t.Fatal(err)
	}
	stored := state.Service("claude").Accounts[account]
	stored.ProxyUsage = UsageSnapshot{
		FiveHour:      LimitWindow{Pct: PtrFloat64(95), ResetsAt: time.Now().Add(-time.Minute)},
		Weekly:        LimitWindow{Pct: PtrFloat64(35), ResetsAt: time.Now().Add(24 * time.Hour)},
		ObservedAt:    time.Now().UTC().Add(-3 * 24 * time.Hour),
		Source:        claudeUsageSourceProxy,
		TokenRevision: stored.SetupTokenRevision,
	}
	state.Service("claude").Accounts[account] = stored
	if err := SaveState(cfg.StatePath, state); err != nil {
		t.Fatal(err)
	}

	status := collectSetupTokenAccount(t, cfg, account)
	if !status.Selectable || status.Reason != "ready" {
		t.Fatalf("status = %#v", status)
	}
	// The five-hour window has reset since the sample; only the weekly counts.
	if status.Score != 0.35 {
		t.Fatalf("score = %v", status.Score)
	}
	if !strings.Contains(status.Account.LastProbeError, "proxy") {
		t.Fatalf("probe error = %q", status.Account.LastProbeError)
	}

	// A second cycle inside the backoff keeps routing on the proxy sample.
	status = collectSetupTokenAccount(t, cfg, account)
	if !status.Selectable {
		t.Fatalf("backoff status = %#v", status)
	}

	// A replaced token invalidates the sample.
	if _, err := ReplaceClaudeSetupToken(context.Background(), cfg, "claude", account, "setup-token-work-2", nil); err != nil {
		t.Fatal(err)
	}
	server2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	t.Cleanup(server2.Close)
	useClaudeUsageServer(t, server2.URL)
	status = collectSetupTokenAccount(t, cfg, account)
	if status.Selectable {
		t.Fatalf("stale proxy sample was trusted: %#v", status)
	}
}

func TestMonitorMergeKeepsProxyUsageAndSwitchesOnIt(t *testing.T) {
	upstream := newProxyUpstream(t)
	cfg, proxy := setupProxyAccounts(t, upstream.server.URL)
	forbidden := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	t.Cleanup(forbidden.Close)
	useClaudeUsageServer(t, forbidden.URL)
	upstream.respond("setup-token-a", func(w http.ResponseWriter, r *http.Request) {
		rateLimitHeaders(w, 0.95, 0.3, "allowed_warning")
		w.WriteHeader(http.StatusOK)
	})
	upstream.respond("setup-token-b", func(w http.ResponseWriter, r *http.Request) {
		rateLimitHeaders(w, 0.1, 0.1, "allowed")
		w.WriteHeader(http.StatusOK)
	})
	if recorder := proxyRequest(t, proxy, proxy.secret, `{}`); recorder.Code != http.StatusOK {
		t.Fatalf("status = %d", recorder.Code)
	}
	// Seed b's sample directly: the monitor must switch to it on a's 95%.
	state, err := LoadState(cfg.StatePath)
	if err != nil {
		t.Fatal(err)
	}
	b := state.Service("claude").Accounts["b"]
	b.ProxyUsage = UsageSnapshot{
		FiveHour:      LimitWindow{Pct: PtrFloat64(10), ResetsAt: time.Now().Add(time.Hour)},
		Weekly:        LimitWindow{Pct: PtrFloat64(10), ResetsAt: time.Now().Add(24 * time.Hour)},
		ObservedAt:    time.Now().UTC(),
		Source:        claudeUsageSourceProxy,
		TokenRevision: b.SetupTokenRevision,
	}
	state.Service("claude").Accounts["b"] = b
	if err := SaveState(cfg.StatePath, state); err != nil {
		t.Fatal(err)
	}

	cycle := MonitorOnce(context.Background(), cfg, true)
	if len(cycle.Errors) != 0 {
		t.Fatalf("cycle errors = %v", cycle.Errors)
	}
	if len(cycle.Switches) != 1 || cycle.Switches[0].Account != "b" {
		t.Fatalf("switches = %#v", cycle.Switches)
	}
	state, err = LoadState(cfg.StatePath)
	if err != nil {
		t.Fatal(err)
	}
	if state.Service("claude").ActiveAccount != "b" {
		t.Fatalf("active = %q", state.Service("claude").ActiveAccount)
	}
	if usage := state.Service("claude").Accounts["a"].ProxyUsage; usage.FiveHour.Pct == nil || *usage.FiveHour.Pct != 95 {
		t.Fatalf("proxy usage for a was lost: %#v", usage)
	}
}
