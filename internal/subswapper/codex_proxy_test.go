package subswapper

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func codexProxyTestConfig(dir, upstreamURL string) Config {
	cfg := Config{
		BackupRoot: filepath.Join(dir, "data", "accounts"),
		StatePath:  filepath.Join(dir, "data", "state.json"),
		Services: []ServiceConfig{{
			Name:          "codex",
			Kind:          "codex",
			AccountMode:   AccountModeHome,
			ProxyListen:   "127.0.0.1:1",
			ProxyUpstream: upstreamURL,
		}},
	}
	cfg.ApplyDefaults()
	return cfg
}

func writeCodexTestLogin(t *testing.T, cfg Config, accountName, token, accountID string) {
	t.Helper()
	home := AccountDir(cfg, "codex", accountName)
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	auth := map[string]any{
		"auth_mode":      "chatgpt",
		"OPENAI_API_KEY": nil,
		"tokens": map[string]string{
			"id_token": token, "access_token": token, "refresh_token": "refresh-" + accountName, "account_id": accountID,
		},
		"last_refresh": time.Now().UTC().Format(time.RFC3339),
	}
	data, err := json.Marshal(auth)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "auth.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func setupCodexProxyAccounts(t *testing.T, upstreamURL string) (Config, *CodexProxy) {
	t.Helper()
	cfg := codexProxyTestConfig(t.TempDir(), upstreamURL)
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	state := NewState()
	for _, name := range []string{"a", "b"} {
		state.Service("codex").Accounts[name] = AccountState{Name: name, AddedAt: time.Now().UTC()}
		writeCodexTestLogin(t, cfg, name, "chatgpt-token-"+name, "acct-"+name)
	}
	state.Service("codex").ActiveAccount = "a"
	if err := SaveState(cfg.StatePath, state); err != nil {
		t.Fatal(err)
	}
	proxy, err := NewCodexProxy(cfg, "codex", func(string, ...any) {})
	if err != nil {
		t.Fatal(err)
	}
	return cfg, proxy
}

func codexProxyRequest(t *testing.T, proxy *CodexProxy, token, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/backend-api/codex/responses", strings.NewReader(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set(codexProxyAccountHeader, codexProxyPlaceholderAccountID)
	}
	req.Header.Set("Accept", "text/event-stream")
	recorder := httptest.NewRecorder()
	proxy.ServeHTTP(recorder, req)
	return recorder
}

type codexUpstreamCall struct {
	Authorization string
	AccountID     string
	Path          string
	Body          string
}

// codexCallLog records upstream calls from server goroutines safely.
type codexCallLog struct {
	mu    sync.Mutex
	calls []codexUpstreamCall
}

func (l *codexCallLog) record(r *http.Request) codexUpstreamCall {
	body, _ := readAllString(r)
	call := codexUpstreamCall{r.Header.Get("Authorization"), codexAccountHeader(r), r.URL.RequestURI(), body}
	l.mu.Lock()
	l.calls = append(l.calls, call)
	l.mu.Unlock()
	return call
}

func (l *codexCallLog) all() []codexUpstreamCall {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]codexUpstreamCall(nil), l.calls...)
}

// responses drops the usage-endpoint calls, which run in the background.
func (l *codexCallLog) responses() []codexUpstreamCall {
	var result []codexUpstreamCall
	for _, call := range l.all() {
		if !strings.HasSuffix(call.Path, codexProxyUsagePath) {
			result = append(result, call)
		}
	}
	return result
}

func codexAccountHeader(r *http.Request) string {
	return r.Header.Get(codexProxyAccountHeader)
}

func TestCodexProxyPlaceholderIsJWTShapedAndPrivate(t *testing.T) {
	cfg := codexProxyTestConfig(t.TempDir(), "https://chatgpt.com")
	first, err := LoadOrCreateCodexProxyPlaceholder(cfg, "codex")
	if err != nil {
		t.Fatal(err)
	}
	second, err := LoadOrCreateCodexProxyPlaceholder(cfg, "codex")
	if err != nil {
		t.Fatal(err)
	}
	if first != second || strings.Count(first.Token, ".") != 2 || first.AccountID != codexProxyPlaceholderAccountID {
		t.Fatalf("placeholder = %#v / %#v", first, second)
	}
	info, err := os.Stat(codexProxyPlaceholderPath(cfg, "codex"))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("placeholder file mode = %v, err = %v", info.Mode(), err)
	}
	// Codex decodes the claims locally; they must parse as a JSON object.
	parts := strings.Split(first.Token, ".")
	payload, err := decodeBase64URL(parts[1])
	if err != nil {
		t.Fatal(err)
	}
	var claims map[string]any
	if err := json.Unmarshal(payload, &claims); err != nil {
		t.Fatal(err)
	}
	auth, _ := claims["https://api.openai.com/auth"].(map[string]any)
	if auth["chatgpt_account_id"] != codexProxyPlaceholderAccountID {
		t.Fatalf("claims = %#v", claims)
	}

	data, err := CodexProxyAuthFile(first, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if !IsCodexProxyAuthFile(data) {
		t.Fatalf("placeholder auth file not recognised: %s", data)
	}
	if IsCodexProxyAuthFile([]byte(`{"auth_mode":"chatgpt","tokens":{"access_token":"x","refresh_token":"real"}}`)) {
		t.Fatal("a real login was mistaken for the placeholder")
	}
}

func decodeBase64URL(value string) ([]byte, error) {
	return base64URLDecode(value)
}

func TestCodexProxyAuthInstallKeepsRealLoginUnlessReplaced(t *testing.T) {
	cfg := codexProxyTestConfig(t.TempDir(), "https://chatgpt.com")
	placeholder, err := LoadOrCreateCodexProxyPlaceholder(cfg, "codex")
	if err != nil {
		t.Fatal(err)
	}
	home := filepath.Join(t.TempDir(), "codex-home")
	if err := EnsureCodexProxyAuth(home, placeholder); err != nil {
		t.Fatal(err)
	}
	if err := EnsureCodexProxyAuth(home, placeholder); err != nil {
		t.Fatalf("second ensure = %v", err)
	}
	real := []byte(`{"auth_mode":"chatgpt","tokens":{"access_token":"x","refresh_token":"real","account_id":"acct"}}`)
	if err := os.WriteFile(filepath.Join(home, "auth.json"), real, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := EnsureCodexProxyAuth(home, placeholder); err != ErrCodexRuntimeAuthIsReal {
		t.Fatalf("ensure over a real login = %v", err)
	}
	backup, err := ReplaceCodexRuntimeAuth(home, placeholder)
	if err != nil {
		t.Fatal(err)
	}
	moved, err := os.ReadFile(backup)
	if err != nil || string(moved) != string(real) {
		t.Fatalf("backup = %q, err = %v", moved, err)
	}
	current, err := os.ReadFile(filepath.Join(home, "auth.json"))
	if err != nil || !IsCodexProxyAuthFile(current) {
		t.Fatalf("auth.json after replace = %s, err = %v", current, err)
	}
	if again, err := ReplaceCodexRuntimeAuth(home, placeholder); err != nil || again != "" {
		t.Fatalf("replace over placeholder = %q, %v", again, err)
	}
}

func TestCodexProxySwapsIdentityAndPassesAnonymousCallsThrough(t *testing.T) {
	log := &codexCallLog{}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		log.record(r)
		if r.URL.Path == codexProxyUsagePath {
			_, _ = w.Write([]byte(`{"rate_limit":{"primary_window":{"used_percent":26,"limit_window_seconds":604800,"reset_at":1788780407},"secondary_window":null}}`))
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: ok\n\n"))
	}))
	t.Cleanup(upstream.Close)
	cfg, proxy := setupCodexProxyAccounts(t, upstream.URL)

	recorder := codexProxyRequest(t, proxy, proxy.placeholder.Token, `{"model":"gpt"}`)
	if recorder.Code != http.StatusOK || recorder.Body.String() != "data: ok\n\n" {
		t.Fatalf("status = %d, body = %q", recorder.Code, recorder.Body.String())
	}
	if calls := log.responses(); len(calls) != 1 || calls[0].Authorization != "Bearer chatgpt-token-a" || calls[0].AccountID != "acct-a" || calls[0].Body != `{"model":"gpt"}` {
		t.Fatalf("upstream calls = %#v", calls)
	}
	// The usage refresh runs in the background; wait for it to land.
	deadline := time.Now().Add(2 * time.Second)
	for {
		state, err := LoadState(cfg.StatePath)
		if err != nil {
			t.Fatal(err)
		}
		usage := state.Service("codex").Accounts["a"].ProxyUsage
		if usage.Weekly.Pct != nil {
			if *usage.Weekly.Pct != 26 || usage.Weekly.ResetsAt.Unix() != 1788780407 || usage.Source != claudeUsageSourceProxy {
				t.Fatalf("proxy usage = %#v", usage)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("usage was not recorded: %#v", state.Service("codex").Accounts["a"])
		}
		time.Sleep(20 * time.Millisecond)
	}

	// Anonymous requests are relayed without any identity.
	recorder = codexProxyRequest(t, proxy, "", `{"jsonrpc":"2.0"}`)
	if recorder.Code != http.StatusOK {
		t.Fatalf("anonymous status = %d", recorder.Code)
	}
	responses := log.responses()
	last := responses[len(responses)-1]
	if last.Authorization != "" || last.AccountID != "" || last.Body != `{"jsonrpc":"2.0"}` {
		t.Fatalf("anonymous call = %#v", last)
	}

	// A wrong bearer never reaches upstream.
	before := len(log.responses())
	if recorder = codexProxyRequest(t, proxy, "not-the-placeholder", `{}`); recorder.Code != http.StatusUnauthorized || len(log.responses()) != before {
		t.Fatalf("wrong secret: status = %d, calls = %d", recorder.Code, len(log.responses())-before)
	}

	req := httptest.NewRequest(http.MethodGet, "/backend-api/codex/responses", nil)
	req.Header.Set("Authorization", "Bearer "+proxy.placeholder.Token)
	req.Header.Set("Upgrade", "websocket")
	req.Header.Set("Connection", "Upgrade")
	recorder = httptest.NewRecorder()
	proxy.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusNotImplemented {
		t.Fatalf("websocket status = %d", recorder.Code)
	}
}

func readAllString(r *http.Request) (string, error) {
	var b strings.Builder
	buf := make([]byte, 4096)
	for {
		n, err := r.Body.Read(buf)
		b.Write(buf[:n])
		if err != nil {
			return b.String(), nil
		}
	}
}

func TestCodexProxyFailsOverOnUsageLimitAndSwitchesRoute(t *testing.T) {
	log := &codexCallLog{}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		log.record(r)
		switch r.Header.Get("Authorization") {
		case "Bearer chatgpt-token-a":
			if r.URL.Path == codexProxyUsagePath {
				_, _ = w.Write([]byte(`{"rate_limit":{"primary_window":{"used_percent":100,"limit_window_seconds":604800,"reset_at":1788780407}}}`))
				return
			}
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"error":{"type":"usage_limit_reached","message":"You've hit your usage limit.","resets_at":1788780407}}`))
		case "Bearer chatgpt-token-b":
			if r.URL.Path == codexProxyUsagePath {
				_, _ = w.Write([]byte(`{"rate_limit":{"primary_window":{"used_percent":10,"limit_window_seconds":604800,"reset_at":1788780407}}}`))
				return
			}
			_, _ = w.Write([]byte(`{"type":"message"}`))
		default:
			w.WriteHeader(http.StatusTeapot)
		}
	}))
	t.Cleanup(upstream.Close)
	cfg, proxy := setupCodexProxyAccounts(t, upstream.URL)

	recorder := codexProxyRequest(t, proxy, proxy.placeholder.Token, `{"model":"gpt"}`)
	if recorder.Code != http.StatusOK || recorder.Body.String() != `{"type":"message"}` {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	responses := log.responses()
	if len(responses) != 2 || responses[0].Authorization != "Bearer chatgpt-token-a" || responses[1].Authorization != "Bearer chatgpt-token-b" ||
		responses[1].AccountID != "acct-b" || responses[1].Body != `{"model":"gpt"}` {
		t.Fatalf("upstream responses calls = %#v", responses)
	}
	state, err := LoadState(cfg.StatePath)
	if err != nil {
		t.Fatal(err)
	}
	service := state.Service("codex")
	if service.ActiveAccount != "b" || service.LastSwitchedAt.IsZero() {
		t.Fatalf("service state = %#v", service)
	}
	if usage := service.Accounts["a"].ProxyUsage; !usage.Exhausted() || usage.Weekly.ResetsAt.Unix() != 1788780407 {
		t.Fatalf("account a usage = %#v", usage)
	}

	// The exhausted account ranks last, so the next request goes to b first.
	recorder = codexProxyRequest(t, proxy, proxy.placeholder.Token, `{}`)
	if recorder.Code != http.StatusOK {
		t.Fatalf("second status = %d", recorder.Code)
	}
	responses = log.responses()
	if len(responses) != 3 || responses[2].Authorization != "Bearer chatgpt-token-b" {
		t.Fatalf("upstream responses calls = %#v", responses)
	}
}

func TestCodexProxyThrottleDoesNotSwitchAndRejectedTokenIsMarked(t *testing.T) {
	// tokenARevoked flips the active account from throttled to dead without
	// swapping the handler under running server goroutines.
	var tokenARevoked atomic.Bool
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Header.Get("Authorization") {
		case "Bearer chatgpt-token-a":
			if tokenARevoked.Load() {
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = w.Write([]byte(`{"error":{"code":"token_revoked"}}`))
				return
			}
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"error":{"type":"rate_limit_exceeded","message":"slow down"}}`))
		case "Bearer chatgpt-token-b":
			if r.URL.Path == codexProxyUsagePath {
				_, _ = w.Write([]byte(`{"rate_limit":{"primary_window":{"used_percent":10,"limit_window_seconds":604800,"reset_at":1788780407}}}`))
				return
			}
			_, _ = w.Write([]byte(`{"type":"message"}`))
		default:
			w.WriteHeader(http.StatusTeapot)
		}
	}))
	t.Cleanup(upstream.Close)
	cfg, proxy := setupCodexProxyAccounts(t, upstream.URL)

	recorder := codexProxyRequest(t, proxy, proxy.placeholder.Token, `{}`)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	state, err := LoadState(cfg.StatePath)
	if err != nil {
		t.Fatal(err)
	}
	if service := state.Service("codex"); service.ActiveAccount != "a" || !service.LastSwitchedAt.IsZero() {
		t.Fatalf("a bare throttle changed the route: %#v", service)
	}

	// A 401 on the active token marks it and the next request skips it.
	tokenARevoked.Store(true)
	if recorder = codexProxyRequest(t, proxy, proxy.placeholder.Token, `{}`); recorder.Code != http.StatusOK {
		t.Fatalf("status after 401 = %d", recorder.Code)
	}
	state, err = LoadState(cfg.StatePath)
	if err != nil {
		t.Fatal(err)
	}
	service := state.Service("codex")
	if service.Accounts["a"].CredentialsError == "" || service.Accounts["a"].FetchBackoffUntil.IsZero() {
		t.Fatalf("account a was not marked rejected: %#v", service.Accounts["a"])
	}
	if service.ActiveAccount != "b" {
		t.Fatalf("dead token did not hand the route to b: %#v", service)
	}
	routes, err := codexProxyRoutes(cfg, cfg.Services[0])
	if err != nil {
		t.Fatal(err)
	}
	if len(routes) != 1 || routes[0].Account != "b" {
		t.Fatalf("routes = %#v", routes)
	}
}

func TestCodexProxyRoutesSkipPlaceholderAndApiKeyHomes(t *testing.T) {
	cfg, proxy := setupCodexProxyAccounts(t, "https://chatgpt.com")
	data, err := CodexProxyAuthFile(proxy.placeholder, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(AccountDir(cfg, "codex", "a"), "auth.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(AccountDir(cfg, "codex", "b"), "auth.json"), []byte(`{"auth_mode":"apikey","OPENAI_API_KEY":"sk-x"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	routes, err := codexProxyRoutes(cfg, cfg.Services[0])
	if err != nil {
		t.Fatal(err)
	}
	if len(routes) != 0 {
		t.Fatalf("routes = %#v", routes)
	}
	recorder := codexProxyRequest(t, proxy, proxy.placeholder.Token, `{}`)
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("status without logins = %d", recorder.Code)
	}
}

func TestParseCodexWhamUsage(t *testing.T) {
	observed := time.Date(2026, 9, 4, 0, 0, 0, 0, time.UTC)
	usage, err := parseCodexWhamUsage([]byte(`{"rate_limit":{"allowed":true,"primary_window":{"used_percent":12,"limit_window_seconds":18000,"reset_at":1788489484},"secondary_window":{"used_percent":26,"limit_window_seconds":604800,"reset_at":1788780407}}}`), observed)
	if err != nil {
		t.Fatal(err)
	}
	if *usage.FiveHour.Pct != 12 || usage.FiveHour.ResetsAt.Unix() != 1788489484 || *usage.Weekly.Pct != 26 || usage.Weekly.ResetsAt.Unix() != 1788780407 ||
		!usage.ObservedAt.Equal(observed) || usage.Source != claudeUsageSourceProxy {
		t.Fatalf("usage = %#v", usage)
	}
	usage, err = parseCodexWhamUsage([]byte(`{"rate_limit":{"primary_window":{"used_percent":26,"limit_window_seconds":604800,"reset_at":1788780407},"secondary_window":null}}`), observed)
	if err != nil || usage.FiveHour.Pct != nil || *usage.Weekly.Pct != 26 {
		t.Fatalf("weekly-only usage = %#v, err = %v", usage, err)
	}
	if _, err := parseCodexWhamUsage([]byte(`{"rate_limit":{}}`), observed); err == nil {
		t.Fatal("empty usage accepted")
	}
}

func TestCodexProxyLaunchArgsAndEnvironment(t *testing.T) {
	args := CodexProxyLaunchArgs("127.0.0.1:7879")
	joined := strings.Join(args, " ")
	for _, want := range []string{
		"-c chatgpt_base_url=http://127.0.0.1:7879/backend-api/",
		"-c model_provider=subswapper",
		"-c model_providers.subswapper.base_url=http://127.0.0.1:7879/backend-api/codex",
		"-c model_providers.subswapper.requires_openai_auth=true",
		"-c model_providers.subswapper.supports_websockets=false",
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("launch args %q lack %q", joined, want)
		}
	}
	env := BuildCodexProxyLaunchEnvironment([]string{"PATH=/bin", "CODEX_API_KEY=sk-x", "CODEX_HOME=/old", "SUBSWAPPER_PROXY=stale"}, "", map[string]string{"SUBSWAPPER_PROXY": "1"})
	if strings.Join(env, " ") != "PATH=/bin CODEX_HOME=/old SUBSWAPPER_PROXY=1" {
		t.Fatalf("native env = %q", env)
	}
	env = BuildCodexProxyLaunchEnvironment([]string{"PATH=/bin", "CODEX_HOME="}, "", map[string]string{"SUBSWAPPER_PROXY": "1"})
	if strings.Join(env, " ") != "PATH=/bin SUBSWAPPER_PROXY=1" {
		t.Fatalf("native env with empty CODEX_HOME = %q", env)
	}
	env = BuildCodexProxyLaunchEnvironment([]string{"CODEX_HOME=/old"}, "/shared", nil)
	if strings.Join(env, " ") != "CODEX_HOME=/shared" {
		t.Fatalf("shared env = %q", env)
	}
}

func TestCodexProxyConfigValidation(t *testing.T) {
	cfg := codexProxyTestConfig(t.TempDir(), "https://chatgpt.com")
	cfg.Services[0].SharedRuntimeHome = NativeRuntimeHome
	if err := cfg.Validate(); err != nil {
		t.Fatalf("codex proxy with native runtime home rejected: %v", err)
	}
	if !cfg.Services[0].CodexProxyEnabled() || !cfg.Services[0].ProxyEnabled() || cfg.Services[0].ClaudeProxyEnabled() {
		t.Fatalf("proxy flags = %#v", cfg.Services[0])
	}
	if got := RuntimeHome(cfg, cfg.Services[0], "a"); got != NativeCodexHome() {
		t.Fatalf("runtime home = %q", got)
	}
	cfg.Services[0].ProxyListen = ""
	if err := cfg.Validate(); err == nil {
		t.Fatal("native runtime home without proxy_listen accepted")
	}
	_, err := NewCodexProxy(cfg, "codex", nil)
	if err == nil {
		t.Fatal("proxy without proxy_listen constructed")
	}
	if _, err := LoadOrCreateCodexProxyPlaceholder(Config{Services: []ServiceConfig{{Name: "claude", Kind: "claude", AccountMode: AccountModeHome}}, StatePath: filepath.Join(t.TempDir(), "s.json")}, "claude"); err == nil {
		t.Fatal("placeholder created for a Claude service")
	}
}
