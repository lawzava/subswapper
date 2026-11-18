package subswapper

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
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

func TestCodexProxyWebSocketSelectedIdentityAndDuplex(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == codexProxyUsagePath {
			_, _ = w.Write([]byte(`{}`))
			return
		}
		if r.Header.Get("Authorization") != "Bearer chatgpt-token-a" || codexAccountHeader(r) != "acct-a" || r.URL.RequestURI() != "/backend-api/codex/responses?test=1" {
			t.Error("WebSocket route did not use the selected identity and original path")
		}
		conn, rw, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		defer func() { _ = conn.Close() }()
		_, _ = rw.WriteString("HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: websocket\r\n\r\nready")
		_ = rw.Flush()
		data := make([]byte, 4)
		if _, err := io.ReadFull(rw, data); err != nil {
			t.Error(err)
			return
		}
		_, _ = conn.Write(data)
	}))
	t.Cleanup(upstream.Close)
	_, proxy := setupCodexProxyAccounts(t, upstream.URL)
	server := httptest.NewServer(proxy)
	t.Cleanup(server.Close)
	conn, err := net.DialTimeout("tcp", strings.TrimPrefix(server.URL, "http://"), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	_, _ = fmt.Fprintf(conn, "GET /backend-api/codex/responses?test=1 HTTP/1.1\r\nHost: synthetic\r\nConnection: Upgrade\r\nUpgrade: websocket\r\nAuthorization: Bearer %s\r\n\r\n", proxy.placeholder.Token)
	rw := bufio.NewReader(conn)
	resp, err := http.ReadResponse(rw, nil)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("upgrade status = %d", resp.StatusCode)
	}
	data := make([]byte, 5)
	if _, err := io.ReadFull(rw, data); err != nil || string(data) != "ready" {
		t.Fatalf("initial duplex bytes = %q, %v", data, err)
	}
	_, _ = conn.Write([]byte("ping"))
	if _, err := io.ReadFull(rw, data[:4]); err != nil || string(data[:4]) != "ping" {
		t.Fatalf("echo = %q, %v", data[:4], err)
	}
}

func TestCodexProxyServeCancelClosesWebSocket(t *testing.T) {
	upstreamDone := make(chan error, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == codexProxyUsagePath {
			_, _ = w.Write([]byte(`{}`))
			return
		}
		conn, rw, err := w.(http.Hijacker).Hijack()
		if err != nil {
			upstreamDone <- err
			return
		}
		defer func() { _ = conn.Close() }()
		_, _ = rw.WriteString("HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: websocket\r\n\r\nready")
		_ = rw.Flush()
		_, err = rw.ReadByte()
		upstreamDone <- err
	}))
	t.Cleanup(upstream.Close)
	_, proxy := setupCodexProxyAccounts(t, upstream.URL)
	proxy.service.ProxyListen = freeLoopbackAddress(t)
	ctx, cancel := context.WithCancel(context.Background())
	serveDone := make(chan struct{})
	var serveErr error
	go func() { serveErr = proxy.Serve(ctx); close(serveDone) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-serveDone:
		case <-time.After(5 * time.Second):
			t.Error("synthetic proxy did not stop during cleanup")
		}
	})

	var conn net.Conn
	deadline := time.Now().Add(5 * time.Second)
	for {
		var err error
		conn, err = net.DialTimeout("tcp", proxy.service.ProxyListen, 100*time.Millisecond)
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("synthetic proxy never became reachable")
		}
		time.Sleep(time.Millisecond)
	}
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := fmt.Fprintf(conn, "GET /backend-api/codex/responses HTTP/1.1\r\nHost: synthetic\r\nConnection: Upgrade\r\nUpgrade: websocket\r\nAuthorization: Bearer %s\r\n\r\n", proxy.placeholder.Token); err != nil {
		t.Fatal("synthetic upgrade request could not be sent")
	}
	rw := bufio.NewReader(conn)
	response, err := http.ReadResponse(rw, nil)
	if err != nil || response.StatusCode != http.StatusSwitchingProtocols {
		t.Fatal("synthetic WebSocket upgrade failed")
	}
	ready := make([]byte, 5)
	if _, err := io.ReadFull(rw, ready); err != nil || string(ready) != "ready" {
		t.Fatal("synthetic upstream did not establish the tunnel")
	}
	cancel()
	select {
	case <-serveDone:
		if serveErr != nil {
			t.Fatal("synthetic proxy returned a shutdown error")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not return after cancellation")
	}
	_ = conn.SetReadDeadline(time.Now().Add(time.Second))
	if _, err := rw.ReadByte(); err != io.EOF {
		t.Fatal("Serve cancellation must close the client tunnel without waiting for a read deadline")
	}
	select {
	case err := <-upstreamDone:
		if err != io.EOF {
			t.Fatal("Serve cancellation must close the upstream tunnel")
		}
	case <-time.After(time.Second):
		t.Fatal("upstream WebSocket handler did not return after Serve cancellation")
	}
}

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
	// Background usage writes must finish before TempDir removes their state.
	t.Cleanup(func() {
		deadline := time.Now().Add(5 * time.Second)
		for {
			proxy.usageMu.Lock()
			pending := len(proxy.usageInFlight)
			proxy.usageMu.Unlock()
			if pending == 0 {
				return
			}
			if time.Now().After(deadline) {
				t.Error("proxy usage refresh did not finish before cleanup")
				return
			}
			time.Sleep(time.Millisecond)
		}
	})
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
	// A backend that does not upgrade can still return an ordinary HTTP reply.
	if recorder.Code != http.StatusOK {
		t.Fatalf("websocket status = %d", recorder.Code)
	}
}

func TestCodexProxyRewritesWorkspaceDiscoveryToPlaceholder(t *testing.T) {
	var acceptEncoding atomic.Value
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != codexProxyAccountsCheckPath {
			http.NotFound(w, r)
			return
		}
		acceptEncoding.Store(r.Header.Get("Accept-Encoding"))
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"accounts":[
			{"id":"acct-team","plan_type":"team","workspace_backend_origin":"https://team.chatgpt.com","account_routing_override":"NO_CONSTRAINT"},
			{"id":%q,"plan_type":"pro","is_zdr":false,"workspace_backend_origin":"NO_CONSTRAINT","account_routing_override":"NO_CONSTRAINT"}
		],"default_account_id":%q,"account_ordering":["acct-team",%q]}`, codexAccountHeader(r), codexAccountHeader(r), codexAccountHeader(r))
	}))
	t.Cleanup(upstream.Close)
	_, proxy := setupCodexProxyAccounts(t, upstream.URL)

	req := httptest.NewRequest(http.MethodGet, codexProxyAccountsCheckPath, nil)
	req.Header.Set("Authorization", "Bearer "+proxy.placeholder.Token)
	req.Header.Set(codexProxyAccountHeader, codexProxyPlaceholderAccountID)
	req.Header.Set("Accept-Encoding", "gzip")
	recorder := httptest.NewRecorder()
	proxy.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	// The proxy must read the body, so upstream may not compress it.
	if got, _ := acceptEncoding.Load().(string); got != "" {
		t.Fatalf("upstream Accept-Encoding = %q", got)
	}
	var response struct {
		Accounts []struct {
			ID                     string `json:"id"`
			PlanType               string `json:"plan_type"`
			IsZDR                  *bool  `json:"is_zdr"`
			WorkspaceBackendOrigin string `json:"workspace_backend_origin"`
			AccountRoutingOverride string `json:"account_routing_override"`
		} `json:"accounts"`
		DefaultAccountID string   `json:"default_account_id"`
		AccountOrdering  []string `json:"account_ordering"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("body = %s: %v", recorder.Body.String(), err)
	}
	if len(response.Accounts) != 2 {
		t.Fatalf("accounts = %#v", response.Accounts)
	}
	other, selected := response.Accounts[0], response.Accounts[1]
	if other.ID != "acct-team" || other.WorkspaceBackendOrigin != "https://team.chatgpt.com" {
		t.Fatalf("unrelated workspace changed: %#v", other)
	}
	// Codex matches the placeholder account ID and needs an HTTPS origin; the
	// proxy's own http address would fail its origin check.
	if selected.ID != codexProxyPlaceholderAccountID || selected.PlanType != "pro" || selected.IsZDR == nil ||
		selected.WorkspaceBackendOrigin != defaultCodexProxyUpstream || selected.AccountRoutingOverride != "NO_CONSTRAINT" {
		t.Fatalf("selected workspace = %#v", selected)
	}
	if response.DefaultAccountID != codexProxyPlaceholderAccountID ||
		strings.Join(response.AccountOrdering, ",") != "acct-team,"+codexProxyPlaceholderAccountID {
		t.Fatalf("default = %q, ordering = %q", response.DefaultAccountID, response.AccountOrdering)
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

func TestCodexProxyRetriesUsageLimitBeforeSendingBytes(t *testing.T) {
	log := &codexCallLog{}
	// Exhaustion is meaningful only before the window resets.
	resetAt := time.Now().Add(24 * time.Hour).Unix()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		log.record(r)
		switch r.Header.Get("Authorization") {
		case "Bearer chatgpt-token-a":
			if r.URL.Path == codexProxyUsagePath {
				_, _ = fmt.Fprintf(w, `{"rate_limit":{"primary_window":{"used_percent":100,"limit_window_seconds":604800,"reset_at":%d}}}`, resetAt)
				return
			}
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = fmt.Fprintf(w, `{"error":{"type":"usage_limit_reached","message":"You've hit your usage limit.","resets_at":%d}}`, resetAt)
		case "Bearer chatgpt-token-b":
			if r.URL.Path == codexProxyUsagePath {
				_, _ = fmt.Fprintf(w, `{"rate_limit":{"primary_window":{"used_percent":10,"limit_window_seconds":604800,"reset_at":%d}}}`, resetAt)
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
	if responses := log.responses(); len(responses) != 2 || responses[0].Authorization != "Bearer chatgpt-token-a" || responses[1].Authorization != "Bearer chatgpt-token-b" || responses[0].Body != responses[1].Body {
		t.Fatalf("upstream responses calls = %#v", responses)
	}
	state, err := LoadState(cfg.StatePath)
	if err != nil {
		t.Fatal(err)
	}
	service := state.Service("codex")
	if service.ActiveAccount != "b" || service.LastSwitchedAt.IsZero() {
		t.Fatalf("a quota fallback did not select b: %#v", service)
	}
	if usage := service.Accounts["a"].ProxyUsage; !usage.Exhausted() || usage.Weekly.ResetsAt.Unix() != resetAt {
		t.Fatalf("account a usage = %#v", usage)
	}
}

func TestCodexProxyRelaysThrottleAndFailsOverOnRejectedToken(t *testing.T) {
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
	if recorder.Code != http.StatusTooManyRequests || !strings.Contains(recorder.Body.String(), "slow down") {
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
	// Codex moves a provider that shares the chatgpt_base_url origin onto the
	// discovered workspace origin, so the two must name the proxy differently.
	for _, want := range []string{
		"-c chatgpt_base_url=http://localhost:7879/backend-api/",
		"-c model_provider=subswapper",
		"-c model_providers.subswapper.base_url=http://127.0.0.1:7879/backend-api/codex",
		"-c model_providers.subswapper.requires_openai_auth=true",
		"-c model_providers.subswapper.supports_websockets=false",
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("launch args %q lack %q", joined, want)
		}
	}
	joined = strings.Join(CodexProxyLaunchArgs("localhost:7879"), " ")
	for _, want := range []string{
		"-c chatgpt_base_url=http://127.0.0.1:7879/backend-api/",
		"-c model_providers.subswapper.base_url=http://localhost:7879/backend-api/codex",
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("localhost launch args %q lack %q", joined, want)
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

func TestCodexProxyRoutesOrderFallbacksByWeeklyReset(t *testing.T) {
	cfg, _ := setupCodexProxyAccounts(t, "https://chatgpt.com")
	state, err := LoadState(cfg.StatePath)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	service := state.Service("codex")
	writeCodexTestLogin(t, cfg, "c", "chatgpt-token-c", "acct-c")
	b := service.Accounts["b"]
	b.Usage = weeklyResetStatusForTest("b", 0, 10, now.Add(120*time.Hour)).Account.Usage
	service.Accounts["b"] = b
	service.Accounts["c"] = AccountState{
		Name:    "c",
		AddedAt: now,
		Usage:   weeklyResetStatusForTest("c", 0, 50, now.Add(20*time.Hour)).Account.Usage,
	}
	if err := SaveState(cfg.StatePath, state); err != nil {
		t.Fatal(err)
	}
	routes, err := codexProxyRoutes(cfg, cfg.Services[0])
	if err != nil {
		t.Fatal(err)
	}
	var order []string
	for _, route := range routes {
		order = append(order, route.Account)
	}
	if strings.Join(order, ",") != "a,c,b" {
		t.Fatalf("route order = %v, want the active route, then the earliest weekly reset", order)
	}
}

func TestCodexProxyKeepsMonitorSwitchMadeDuringRequest(t *testing.T) {
	var cfg Config
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == codexProxyUsagePath {
			_, _ = w.Write([]byte(`{"rate_limit":{"primary_window":{"used_percent":10,"limit_window_seconds":604800,"reset_at":1788780407}}}`))
			return
		}
		// The monitor moves the route while a's response is still streaming.
		state, err := LoadState(cfg.StatePath)
		if err != nil {
			t.Error(err)
			return
		}
		state.Service("codex").ActiveAccount = "b"
		if err := SaveState(cfg.StatePath, state); err != nil {
			t.Error(err)
			return
		}
		_, _ = w.Write([]byte(`{"type":"message"}`))
	}))
	t.Cleanup(upstream.Close)
	cfg, proxy := setupCodexProxyAccounts(t, upstream.URL)

	if recorder := codexProxyRequest(t, proxy, proxy.placeholder.Token, `{}`); recorder.Code != http.StatusOK {
		t.Fatalf("status = %d", recorder.Code)
	}
	state, err := LoadState(cfg.StatePath)
	if err != nil {
		t.Fatal(err)
	}
	if service := state.Service("codex"); service.ActiveAccount != "b" {
		t.Fatalf("a finished request reverted the monitor's switch: active = %q", service.ActiveAccount)
	}
}

// codexSocketFrame encodes one WebSocket frame. A non-nil mask masks the
// payload, which servers must not do but the relay tolerates.
func codexSocketFrame(fin bool, opcode byte, payload []byte, mask []byte) []byte {
	first := opcode
	if fin {
		first |= 0x80
	}
	frame := []byte{first}
	maskBit := byte(0)
	if mask != nil {
		maskBit = 0x80
	}
	switch {
	case len(payload) < 126:
		frame = append(frame, maskBit|byte(len(payload)))
	case len(payload) <= 0xffff:
		frame = append(frame, maskBit|126)
		frame = binary.BigEndian.AppendUint16(frame, uint16(len(payload)))
	default:
		frame = append(frame, maskBit|127)
		frame = binary.BigEndian.AppendUint64(frame, uint64(len(payload)))
	}
	if mask == nil {
		return append(frame, payload...)
	}
	frame = append(frame, mask...)
	for index, value := range payload {
		frame = append(frame, value^mask[index%4])
	}
	return frame
}

func codexSocketText(payload string) []byte {
	return codexSocketFrame(true, 0x1, []byte(payload), nil)
}

type codexSocketHandshake struct {
	Token      string
	Extensions string
}

// codexSocketUpstream is a synthetic ChatGPT backend. Each handshake gets
// frames[token]; afterwards every 4 bytes from the client are answered with
// a "pong" text frame, which proves the socket is still open.
type codexSocketUpstream struct {
	frames     map[string][][]byte
	usage      func(token string) (int, string)
	extensions string
	// beforeFrames runs after the 101 and before frames[token] are sent.
	beforeFrames func(token string)

	mu         sync.Mutex
	handshakes []codexSocketHandshake
}

func (u *codexSocketUpstream) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if r.URL.Path == codexProxyUsagePath {
		status, body := http.StatusInternalServerError, `{"error":"synthetic"}`
		if u.usage != nil {
			status, body = u.usage(token)
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
		return
	}
	u.mu.Lock()
	u.handshakes = append(u.handshakes, codexSocketHandshake{token, r.Header.Get("Sec-WebSocket-Extensions")})
	u.mu.Unlock()
	conn, rw, err := w.(http.Hijacker).Hijack()
	if err != nil {
		return
	}
	defer func() { _ = conn.Close() }()
	_, _ = rw.WriteString("HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: websocket\r\n")
	if u.extensions != "" {
		_, _ = rw.WriteString("Sec-WebSocket-Extensions: " + u.extensions + "\r\n")
	}
	_, _ = rw.WriteString("\r\n")
	_ = rw.Flush()
	if u.beforeFrames != nil {
		u.beforeFrames(token)
	}
	for _, frame := range u.frames[token] {
		// Separate flushes exercise frames that arrive in pieces.
		_, _ = rw.Write(frame)
		_ = rw.Flush()
	}
	probe := make([]byte, 4)
	for {
		if _, err := io.ReadFull(rw, probe); err != nil {
			return
		}
		_, _ = rw.Write(codexSocketText("pong"))
		_ = rw.Flush()
	}
}

func (u *codexSocketUpstream) seen() []codexSocketHandshake {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]codexSocketHandshake(nil), u.handshakes...)
}

// startCodexSocketProxy serves proxy over HTTP. wait blocks until every
// proxy handler, including its post-close usage refresh, has returned.
func startCodexSocketProxy(t *testing.T, upstream *codexSocketUpstream) (cfg Config, addr string, wait func()) {
	t.Helper()
	harness := startCodexSocketHarness(t, upstream, nil)
	return harness.cfg, harness.addr, func() {
		if !harness.handlersDone(10 * time.Second) {
			t.Error("proxy WebSocket handler did not return")
		}
	}
}

type codexSocketHarness struct {
	cfg          Config
	addr         string
	proxy        *CodexProxy
	handlersDone func(timeout time.Duration) bool
}

// startCodexSocketHarness seeds fresh usage for a and b, lets adjust change
// that seed, and serves the proxy. handlersDone reports whether every proxy
// handler returned within timeout.
func startCodexSocketHarness(t *testing.T, upstream *codexSocketUpstream, adjust func(*ServiceState)) codexSocketHarness {
	t.Helper()
	backend := httptest.NewServer(upstream)
	t.Cleanup(backend.Close)
	cfg, proxy := setupCodexProxyAccounts(t, backend.URL)
	// Fresh usage for both accounts keeps the background refresh quiet, so
	// only the in-socket handling can write usage.
	state, err := LoadState(cfg.StatePath)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	for _, name := range []string{"a", "b"} {
		account := state.Service("codex").Accounts[name]
		account.ProxyUsage = UsageSnapshot{
			ObservedAt: now,
			Source:     claudeUsageSourceProxy,
			Weekly:     LimitWindow{Pct: PtrFloat64(10), ResetsAt: now.Add(72 * time.Hour)},
		}
		state.Service("codex").Accounts[name] = account
	}
	if adjust != nil {
		adjust(state.Service("codex"))
	}
	if err := SaveState(cfg.StatePath, state); err != nil {
		t.Fatal(err)
	}
	var handlers sync.WaitGroup
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handlers.Add(1)
		defer handlers.Done()
		proxy.ServeHTTP(w, r)
	}))
	handlersDone := func(timeout time.Duration) bool {
		finished := make(chan struct{})
		go func() { handlers.Wait(); close(finished) }()
		select {
		case <-finished:
			return true
		case <-time.After(timeout):
			return false
		}
	}
	t.Cleanup(func() {
		server.Close()
		if !handlersDone(10 * time.Second) {
			t.Error("proxy WebSocket handler did not return")
		}
	})
	return codexSocketHarness{cfg, strings.TrimPrefix(server.URL, "http://"), proxy, handlersDone}
}

func dialCodexSocket(t *testing.T, addr, token string) (net.Conn, *bufio.Reader) {
	t.Helper()
	conn, err := net.DialTimeout("tcp", addr, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	_, _ = fmt.Fprintf(conn, "GET /backend-api/codex/responses HTTP/1.1\r\nHost: synthetic\r\nConnection: Upgrade\r\nUpgrade: websocket\r\n"+
		"Sec-WebSocket-Version: 13\r\nSec-WebSocket-Key: c3Vic3dhcHBlci10ZXN0IQ==\r\n"+
		"Sec-WebSocket-Extensions: permessage-deflate; client_max_window_bits\r\nAuthorization: Bearer %s\r\n\r\n", token)
	reader := bufio.NewReader(conn)
	resp, err := http.ReadResponse(reader, nil)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("upgrade status = %d", resp.StatusCode)
	}
	return conn, reader
}

func expectCodexSocketBytes(t *testing.T, reader *bufio.Reader, want []byte) {
	t.Helper()
	got := make([]byte, len(want))
	if _, err := io.ReadFull(reader, got); err != nil {
		t.Fatalf("read relayed frames: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("relayed frames differ from upstream bytes:\n got %q\nwant %q", truncateForTest(got), truncateForTest(want))
	}
}

func truncateForTest(data []byte) []byte {
	if len(data) > 200 {
		return data[:200]
	}
	return data
}

// expectCodexSocketOpen proves the relay still forwards in both directions.
func expectCodexSocketOpen(t *testing.T, conn net.Conn, reader *bufio.Reader) {
	t.Helper()
	if _, err := conn.Write([]byte("ping")); err != nil {
		t.Fatalf("socket closed: %v", err)
	}
	expectCodexSocketBytes(t, reader, codexSocketText("pong"))
}

// expectCodexSocketClosed requires a server close frame, then EOF.
func expectCodexSocketClosed(t *testing.T, conn net.Conn, reader *bufio.Reader) {
	t.Helper()
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	header := make([]byte, 2)
	if _, err := io.ReadFull(reader, header); err != nil {
		t.Fatalf("no close frame: %v", err)
	}
	if header[0] != 0x88 || header[1]&0x80 != 0 || header[1] < 2 || header[1] > 125 {
		t.Fatalf("close frame header = %#x %#x", header[0], header[1])
	}
	payload := make([]byte, header[1])
	if _, err := io.ReadFull(reader, payload); err != nil {
		t.Fatalf("close payload: %v", err)
	}
	if code := binary.BigEndian.Uint16(payload); code != 1013 {
		t.Fatalf("close code = %d, want 1013", code)
	}
	if _, err := reader.ReadByte(); err != io.EOF {
		t.Fatalf("after close frame: %v, want EOF", err)
	}
}

func loadCodexTestAccount(t *testing.T, cfg Config, name string) AccountState {
	t.Helper()
	state, err := LoadState(cfg.StatePath)
	if err != nil {
		t.Fatal(err)
	}
	return state.Service("codex").Accounts[name]
}

// expectCodexSocketFailover opens a second socket and requires it on b.
func expectCodexSocketFailover(t *testing.T, cfg Config, upstream *codexSocketUpstream, addr, token string) {
	t.Helper()
	conn, reader := dialCodexSocket(t, addr, token)
	expectCodexSocketBytes(t, reader, codexSocketText(`{"type":"response.created"}`))
	expectCodexSocketOpen(t, conn, reader)
	handshakes := upstream.seen()
	if len(handshakes) != 2 || handshakes[0].Token != "chatgpt-token-a" || handshakes[1].Token != "chatgpt-token-b" {
		t.Fatalf("handshakes = %#v, want a then b", handshakes)
	}
	routes, err := codexProxyRoutes(cfg, cfg.Services[0])
	if err != nil {
		t.Fatal(err)
	}
	if len(routes) == 0 || routes[0].Account != "b" {
		t.Fatalf("route order after rejection = %#v", routes)
	}
}

func TestCodexProxyWebSocketQuotaFrameClosesAndFailsOver(t *testing.T) {
	resetAt := time.Now().Add(36 * time.Hour).Unix()
	quota := fmt.Sprintf(`{"type":"error","status":429,"error":{"type":"usage_limit_reached","message":"You've hit your usage limit.","plan_type":"pro","resets_at":%d},"headers":{"x-codex-primary-used-percent":"100.0","x-codex-primary-window-minutes":15}}`, resetAt)
	aliased := strings.Replace(quota, `"status":429`, `"status_code":429`, 1)
	split := len(quota) / 2
	tests := []struct {
		name   string
		frames [][]byte
	}{
		{"unfragmented", [][]byte{codexSocketText(`{"type":"response.created"}`), codexSocketText(quota)}},
		{"status_code alias", [][]byte{codexSocketText(aliased)}},
		{"fragmented with ping", [][]byte{
			codexSocketFrame(false, 0x1, []byte(quota[:split]), nil),
			codexSocketFrame(true, 0x9, []byte("keepalive"), nil),
			codexSocketFrame(true, 0x0, []byte(quota[split:]), nil),
		}},
		{"masked", [][]byte{codexSocketFrame(true, 0x1, []byte(quota), []byte{1, 2, 3, 4})}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			upstream := &codexSocketUpstream{
				frames: map[string][][]byte{
					"chatgpt-token-a": test.frames,
					"chatgpt-token-b": {codexSocketText(`{"type":"response.created"}`)},
				},
				usage: func(token string) (int, string) {
					if token != "chatgpt-token-a" {
						return http.StatusInternalServerError, `{}`
					}
					return http.StatusOK, fmt.Sprintf(`{"rate_limit":{"primary_window":{"used_percent":100,"limit_window_seconds":604800,"reset_at":%d}}}`, resetAt)
				},
			}
			cfg, addr, wait := startCodexSocketProxy(t, upstream)
			conn, reader := dialCodexSocket(t, addr, codexProxyTestPlaceholder(t, cfg))
			expectCodexSocketBytes(t, reader, bytes.Join(test.frames, nil))
			expectCodexSocketClosed(t, conn, reader)
			wait()
			account := loadCodexTestAccount(t, cfg, "a")
			if usage := account.ProxyUsage; !usage.Exhausted() || usage.Weekly.ResetsAt.Unix() != resetAt || account.LastProbeStartedAt.IsZero() {
				t.Fatalf("account a after in-socket quota = %#v", account)
			}
			expectCodexSocketFailover(t, cfg, upstream, addr, codexProxyTestPlaceholder(t, cfg))
		})
	}
}

func codexProxyTestPlaceholder(t *testing.T, cfg Config) string {
	t.Helper()
	placeholder, err := LoadOrCreateCodexProxyPlaceholder(cfg, "codex")
	if err != nil {
		t.Fatal(err)
	}
	return placeholder.Token
}

func TestCodexProxyWebSocketQuotaWithoutUsageMarksUntilReset(t *testing.T) {
	now := time.Now().UTC()
	tests := []struct {
		name     string
		resetsAt string
		check    func(UsageSnapshot) bool
	}{
		{"short reset", fmt.Sprintf(`,"resets_at":%d`, now.Add(2*time.Hour).Unix()), func(usage UsageSnapshot) bool {
			return usage.FiveHour.ResetsAt.Unix() == now.Add(2*time.Hour).Unix()
		}},
		{"weekly reset", fmt.Sprintf(`,"resets_at":%d`, now.Add(80*time.Hour).Unix()), func(usage UsageSnapshot) bool {
			return usage.Weekly.ResetsAt.Unix() == now.Add(80*time.Hour).Unix() && usage.FiveHour.Pct == nil
		}},
		{"no reset", "", func(usage UsageSnapshot) bool {
			reset := usage.FiveHour.ResetsAt
			return reset.After(now) && !reset.After(time.Now().Add(5*time.Minute))
		}},
		{"past reset", fmt.Sprintf(`,"resets_at":%d`, now.Add(-time.Hour).Unix()), func(usage UsageSnapshot) bool {
			reset := usage.FiveHour.ResetsAt
			return reset.After(now) && !reset.After(time.Now().Add(5*time.Minute))
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			frame := codexSocketText(`{"type":"error","status":429,"error":{"type":"usage_not_included","message":"no"` + test.resetsAt + `}}`)
			upstream := &codexSocketUpstream{frames: map[string][][]byte{
				"chatgpt-token-a": {frame},
				"chatgpt-token-b": {codexSocketText(`{"type":"response.created"}`)},
			}}
			cfg, addr, wait := startCodexSocketProxy(t, upstream)
			conn, reader := dialCodexSocket(t, addr, codexProxyTestPlaceholder(t, cfg))
			expectCodexSocketBytes(t, reader, frame)
			expectCodexSocketClosed(t, conn, reader)
			wait()
			usage := loadCodexTestAccount(t, cfg, "a").ProxyUsage
			if !usage.Exhausted() || usage.Source != claudeUsageSourceProxy || !test.check(usage) {
				t.Fatalf("account a usage after failed fetch = %#v", usage)
			}
			expectCodexSocketFailover(t, cfg, upstream, addr, codexProxyTestPlaceholder(t, cfg))
		})
	}
}

func TestCodexProxyWebSocketUnauthorizedFrameClosesAndFailsOver(t *testing.T) {
	frame := codexSocketText(`{"type":"error","status":401,"error":{"code":"token_revoked","message":"expired"}}`)
	upstream := &codexSocketUpstream{frames: map[string][][]byte{
		"chatgpt-token-a": {frame},
		"chatgpt-token-b": {codexSocketText(`{"type":"response.created"}`)},
	}}
	cfg, addr, wait := startCodexSocketProxy(t, upstream)
	conn, reader := dialCodexSocket(t, addr, codexProxyTestPlaceholder(t, cfg))
	expectCodexSocketBytes(t, reader, frame)
	expectCodexSocketClosed(t, conn, reader)
	wait()
	if account := loadCodexTestAccount(t, cfg, "a"); account.CredentialsError == "" || !account.FetchBackoffUntil.After(time.Now()) {
		t.Fatalf("account a after in-socket 401 = %#v", account)
	}
	expectCodexSocketFailover(t, cfg, upstream, addr, codexProxyTestPlaceholder(t, cfg))
}

// expectCodexSocketUnrecorded requires account a to keep its seeded state.
func expectCodexSocketUnrecorded(t *testing.T, cfg Config) {
	t.Helper()
	account := loadCodexTestAccount(t, cfg, "a")
	if usage := account.ProxyUsage; usage.Weekly.Pct == nil || *usage.Weekly.Pct != 10 || usage.FiveHour.Pct != nil ||
		account.CredentialsError != "" || !account.LastProbeStartedAt.IsZero() {
		t.Fatalf("account a changed without a rejection: %#v", account)
	}
}

func TestCodexProxyWebSocketRelaysNormalFramesUnchanged(t *testing.T) {
	large := `{"type":"response.output_text.delta","delta":"` + strings.Repeat("x", 100<<10) + `"}`
	// A quota-shaped message over the inspection limit is not classified.
	largeQuota := `{"type":"error","status":429,"error":{"type":"usage_limit_reached","pad":"` + strings.Repeat("y", 70<<10) + `"}}`
	binaryPayload := make([]byte, 300)
	for index := range binaryPayload {
		binaryPayload[index] = byte(index)
	}
	frames := [][]byte{
		codexSocketText(`{"type":"response.created"}`),
		codexSocketFrame(true, 0x2, binaryPayload, nil),
		codexSocketText(large),
		codexSocketFrame(false, 0x1, []byte(largeQuota[:40<<10]), nil),
		codexSocketFrame(true, 0xA, nil, nil),
		codexSocketFrame(true, 0x0, []byte(largeQuota[40<<10:]), nil),
		codexSocketText(`{"type":"response.completed"}`),
	}
	upstream := &codexSocketUpstream{frames: map[string][][]byte{"chatgpt-token-a": frames}}
	cfg, addr, _ := startCodexSocketProxy(t, upstream)
	conn, reader := dialCodexSocket(t, addr, codexProxyTestPlaceholder(t, cfg))
	expectCodexSocketBytes(t, reader, bytes.Join(frames, nil))
	expectCodexSocketOpen(t, conn, reader)
	expectCodexSocketUnrecorded(t, cfg)
}

func TestCodexProxyWebSocketKeepsNonQuotaErrorsOpen(t *testing.T) {
	frames := [][]byte{
		codexSocketText(`{"type":"error","status":429,"error":{"type":"rate_limit_exceeded","message":"slow down"}}`),
		codexSocketText(`{"type":"error","status_code":429,"error":{"code":"websocket_connection_limit_reached","message":"too many sockets"}}`),
		codexSocketText(`{"type":"error","status":400,"error":{"code":"previous_response_not_found","message":"usage_limit_reached is not the code"}}`),
		// Codex maps an error frame only when it carries a status.
		codexSocketText(`{"type":"error","error":{"type":"usage_limit_reached"}}`),
		// response.failed stays unclassified, as inside an HTTP 200 stream.
		codexSocketText(`{"type":"response.failed","response":{"error":{"code":"usage_limit_reached"}}}`),
		codexSocketText(`{"type":"error","status":"429","error":{"type":"usage_limit_reached"}}`),
		codexSocketText(`not json "error"`),
	}
	upstream := &codexSocketUpstream{frames: map[string][][]byte{"chatgpt-token-a": frames}}
	cfg, addr, _ := startCodexSocketProxy(t, upstream)
	conn, reader := dialCodexSocket(t, addr, codexProxyTestPlaceholder(t, cfg))
	expectCodexSocketBytes(t, reader, bytes.Join(frames, nil))
	expectCodexSocketOpen(t, conn, reader)
	expectCodexSocketUnrecorded(t, cfg)
}

func TestCodexProxyWebSocketSkipsInspectionForExtensions(t *testing.T) {
	quota := codexSocketText(`{"type":"error","status":429,"error":{"type":"usage_limit_reached"}}`)
	compressed := codexSocketText(`compressed bytes`)
	compressed[0] |= 0x40
	tests := []struct {
		name       string
		extensions string
		frames     [][]byte
	}{
		{"negotiated extension", "permessage-deflate", [][]byte{quota}},
		{"rsv1 frame", "", [][]byte{compressed, quota}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			upstream := &codexSocketUpstream{extensions: test.extensions, frames: map[string][][]byte{"chatgpt-token-a": test.frames}}
			cfg, addr, _ := startCodexSocketProxy(t, upstream)
			conn, reader := dialCodexSocket(t, addr, codexProxyTestPlaceholder(t, cfg))
			expectCodexSocketBytes(t, reader, bytes.Join(test.frames, nil))
			expectCodexSocketOpen(t, conn, reader)
			expectCodexSocketUnrecorded(t, cfg)
		})
	}
}

func TestCodexProxyWebSocketWithdrawsCompressionOffer(t *testing.T) {
	upstream := &codexSocketUpstream{}
	cfg, addr, _ := startCodexSocketProxy(t, upstream)
	conn, reader := dialCodexSocket(t, addr, codexProxyTestPlaceholder(t, cfg))
	expectCodexSocketOpen(t, conn, reader)
	if handshakes := upstream.seen(); len(handshakes) != 1 || handshakes[0].Token != "chatgpt-token-a" || handshakes[0].Extensions != "" {
		t.Fatalf("upstream handshakes = %#v, want no Sec-WebSocket-Extensions", handshakes)
	}
}

func TestCodexProxyWebSocketQuotaMarkerSurvivesSlowRefresh(t *testing.T) {
	resetAt := time.Now().Add(2 * time.Hour).Unix()
	frame := codexSocketText(fmt.Sprintf(`{"type":"error","status":429,"error":{"type":"usage_limit_reached","resets_at":%d}}`, resetAt))
	refreshStarted, releaseRefresh := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(releaseRefresh) }) }
	var usageCalls atomic.Int32
	upstream := &codexSocketUpstream{
		frames: map[string][][]byte{
			"chatgpt-token-a": {frame},
			"chatgpt-token-b": {codexSocketText(`{"type":"response.created"}`)},
		},
		usage: func(token string) (int, string) {
			if token != "chatgpt-token-a" {
				return http.StatusInternalServerError, `{}`
			}
			if usageCalls.Add(1) > 1 {
				// The refinement after the rejection fails.
				return http.StatusInternalServerError, `{}`
			}
			// The handshake's background refresh reads healthy usage, but
			// only after the in-socket rejection has been recorded.
			close(refreshStarted)
			<-releaseRefresh
			return http.StatusOK, `{"rate_limit":{"primary_window":{"used_percent":10,"limit_window_seconds":604800,"reset_at":1788780407}}}`
		},
		beforeFrames: func(token string) {
			if token != "chatgpt-token-a" {
				return
			}
			select {
			case <-refreshStarted:
			case <-time.After(5 * time.Second):
			}
		},
	}
	harness := startCodexSocketHarness(t, upstream, func(service *ServiceState) {
		// Stale usage makes the handshake start a background refresh.
		account := service.Accounts["a"]
		account.ProxyUsage.ObservedAt = time.Now().UTC().Add(-2 * codexProxyUsageTTL)
		service.Accounts["a"] = account
	})
	// Runs before the backend closes, which waits for the held request.
	t.Cleanup(release)
	token := codexProxyTestPlaceholder(t, harness.cfg)
	conn, reader := dialCodexSocket(t, harness.addr, token)
	expectCodexSocketBytes(t, reader, frame)
	expectCodexSocketClosed(t, conn, reader)
	if !harness.handlersDone(10 * time.Second) {
		t.Fatal("proxy WebSocket handler did not return")
	}
	if calls := usageCalls.Load(); calls != 2 {
		t.Fatalf("usage calls = %d, want the held refresh and the failed refinement", calls)
	}
	release()
	deadline := time.Now().Add(5 * time.Second)
	for {
		harness.proxy.usageMu.Lock()
		pending := len(harness.proxy.usageInFlight)
		harness.proxy.usageMu.Unlock()
		if pending == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("background refresh did not finish")
		}
		time.Sleep(time.Millisecond)
	}
	if usage := loadCodexTestAccount(t, harness.cfg, "a").ProxyUsage; !usage.Exhausted() || usage.FiveHour.ResetsAt.Unix() != resetAt {
		t.Fatalf("a refresh that started before the rejection replaced its marker: %#v", usage)
	}
	expectCodexSocketFailover(t, harness.cfg, upstream, harness.addr, token)
}

func TestCodexProxyOlderUsageKeepsNewerTokenRejection(t *testing.T) {
	cfg, _ := setupCodexProxyAccounts(t, "http://127.0.0.1:1")
	routes, err := codexProxyRoutes(cfg, cfg.Services[0])
	if err != nil {
		t.Fatal(err)
	}
	route := routes[0]
	started := time.Now().UTC().Add(-time.Second)
	// An in-socket 401 is recorded while a usage fetch that started earlier
	// is still in flight.
	if err := recordCodexProxyState(context.Background(), cfg, ServiceConfig{Name: "codex"}, route, nil, true, false, false, false); err != nil {
		t.Fatal(err)
	}
	healthy := UsageSnapshot{
		ObservedAt: started,
		Source:     claudeUsageSourceProxy,
		Weekly:     LimitWindow{Pct: PtrFloat64(10), ResetsAt: started.Add(72 * time.Hour)},
	}
	if err := recordCodexProxyObservation(cfg, ServiceConfig{Name: "codex"}, route, &healthy, false, false); err != nil {
		t.Fatal(err)
	}
	if account := loadCodexTestAccount(t, cfg, "a"); account.CredentialsError == "" || account.FetchBackoffUntil.IsZero() {
		t.Fatalf("a fetch that started before the token rejection cleared it: %#v", account)
	}
	// A fetch that starts after the rejection proves the token works again.
	healthy.ObservedAt = time.Now().UTC().Add(time.Second)
	if err := recordCodexProxyObservation(cfg, ServiceConfig{Name: "codex"}, route, &healthy, false, false); err != nil {
		t.Fatal(err)
	}
	if account := loadCodexTestAccount(t, cfg, "a"); account.CredentialsError != "" || !account.FetchBackoffUntil.IsZero() {
		t.Fatalf("a newer healthy fetch did not clear the token rejection: %#v", account)
	}
}

func TestCodexProxyWebSocketRejectionBoundsStateLockWait(t *testing.T) {
	previous := codexSocketRejectionWindow
	codexSocketRejectionWindow = 300 * time.Millisecond
	// Registered first, so it runs after the harness waited for handlers.
	t.Cleanup(func() { codexSocketRejectionWindow = previous })
	frame := codexSocketText(`{"type":"error","status":429,"error":{"type":"usage_limit_reached"}}`)
	send := make(chan struct{})
	var sendOnce sync.Once
	upstream := &codexSocketUpstream{
		frames: map[string][][]byte{"chatgpt-token-a": {frame}},
		beforeFrames: func(string) {
			select {
			case <-send:
			case <-time.After(10 * time.Second):
			}
		},
	}
	harness := startCodexSocketHarness(t, upstream, nil)
	t.Cleanup(func() { sendOnce.Do(func() { close(send) }) })
	// The placeholder load takes the state lock, so dial before holding it.
	conn, reader := dialCodexSocket(t, harness.addr, codexProxyTestPlaceholder(t, harness.cfg))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	lock, err := AcquireStateLock(ctx, harness.cfg)
	cancel()
	if err != nil {
		t.Fatal(err)
	}
	var unlockOnce sync.Once
	unlock := func() { unlockOnce.Do(lock.Release) }
	// Runs before the harness cleanup waits for the handler.
	t.Cleanup(unlock)
	sendOnce.Do(func() { close(send) })
	expectCodexSocketBytes(t, reader, frame)
	_ = conn.Close()
	started := time.Now()
	if !harness.handlersDone(3 * time.Second) {
		t.Fatalf("handler still waits for the state lock %s after the client left", time.Since(started).Round(time.Millisecond))
	}
	unlock()
}
