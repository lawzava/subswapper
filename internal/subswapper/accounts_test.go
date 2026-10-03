package subswapper

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func codexTestLogin(t *testing.T, email, access string) []byte {
	t.Helper()
	claims, err := json.Marshal(map[string]any{"email": email})
	if err != nil {
		t.Fatal(err)
	}
	idToken := base64URL([]byte(`{"alg":"none"}`)) + "." + base64URL(claims) + ".sig"
	data, err := json.Marshal(map[string]any{
		"auth_mode": "chatgpt",
		"tokens": map[string]string{
			"id_token": idToken, "access_token": access, "refresh_token": "refresh-" + access, "account_id": "acct-" + access,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestAddCodexAccountInstallsLoginAndUpdatesIt(t *testing.T) {
	cfg := codexProxyTestConfig(t.TempDir(), "https://chatgpt.com")
	first := codexTestLogin(t, "work@example.com", "one")
	created, err := AddCodexAccount(cfg, "codex", "work", "", first)
	if err != nil || !created {
		t.Fatalf("AddCodexAccount = %v, %v", created, err)
	}
	stored, err := os.ReadFile(filepath.Join(AccountDir(cfg, "codex", "work"), "auth.json"))
	if err != nil || string(stored) != string(first) {
		t.Fatalf("stored login = %s, %v", stored, err)
	}
	account, ok := mustLoadState(t, cfg).Account("codex", "work")
	if !ok || account.Email != "work@example.com" {
		t.Fatalf("account = %#v", account)
	}
	if active := mustLoadState(t, cfg).Service("codex").ActiveAccount; active != "work" {
		t.Fatalf("first account is not selected: %q", active)
	}

	// Adding an existing account replaces its login, e.g. after it expired.
	second := codexTestLogin(t, "work@example.com", "two")
	created, err = AddCodexAccount(cfg, "codex", "work", "", second)
	if err != nil || created {
		t.Fatalf("re-add = %v, %v", created, err)
	}
	stored, _ = os.ReadFile(filepath.Join(AccountDir(cfg, "codex", "work"), "auth.json"))
	if string(stored) != string(second) {
		t.Fatal("re-add did not replace the login")
	}

	placeholder, err := CodexProxyAuthFile(CodexProxyPlaceholder{Token: "a.b.c", AccountID: "x"}, codexProxyNow())
	if err != nil {
		t.Fatal(err)
	}
	for name, login := range map[string][]byte{
		"placeholder": placeholder,
		"api key":     []byte(`{"auth_mode":"apikey","OPENAI_API_KEY":"sk-x"}`),
		"garbage":     []byte(`not json`),
	} {
		if _, err := AddCodexAccount(cfg, "codex", "other", "", login); err == nil {
			t.Fatalf("%s login accepted", name)
		}
	}
	if _, ok := mustLoadState(t, cfg).Account("codex", "other"); ok {
		t.Fatal("a rejected login registered an account")
	}
}

func mustLoadState(t *testing.T, cfg Config) *State {
	t.Helper()
	state, err := LoadState(cfg.StatePath)
	if err != nil {
		t.Fatal(err)
	}
	return state
}

func TestAddClaudeAccountStoresTokenAndRemoveDeletesIt(t *testing.T) {
	cfg := setupTokenTestConfig(t.TempDir())
	cfg.ApplyDefaults()
	lookup := func(context.Context, string) (ClaudeSetupTokenIdentity, error) {
		return ClaudeSetupTokenIdentity{AccountUUID: "uuid-work"}, nil
	}
	created, err := AddClaudeAccount(context.Background(), cfg, "claude", "work", "work@example.com", "sk-ant-oat01-work", lookup)
	if err != nil || !created {
		t.Fatalf("AddClaudeAccount = %v, %v", created, err)
	}
	token, status, err := LoadClaudeSetupTokenWithStatus(cfg, "claude", "work")
	if err != nil || !status.Usable || token != "sk-ant-oat01-work" {
		t.Fatalf("token = %q, %#v, %v", token, status, err)
	}
	if created, err = AddClaudeAccount(context.Background(), cfg, "claude", "work", "", "sk-ant-oat01-new", lookup); err != nil || created {
		t.Fatalf("re-add = %v, %v", created, err)
	}
	if _, err := AddClaudeAccount(context.Background(), cfg, "claude", "bad", "", "", lookup); err == nil {
		t.Fatal("empty token accepted")
	}
	if _, ok := mustLoadState(t, cfg).Account("claude", "bad"); ok {
		t.Fatal("a rejected token registered an account")
	}

	if err := UnregisterAccount(context.Background(), cfg, "claude", "work", false, false); err == nil || !strings.Contains(err.Error(), "active") {
		t.Fatalf("removing the active account without force: %v", err)
	}
	if err := UnregisterAccount(context.Background(), cfg, "claude", "work", true, false); err != nil {
		t.Fatal(err)
	}
	if _, ok := mustLoadState(t, cfg).Account("claude", "work"); ok {
		t.Fatal("account still registered")
	}
	if _, status, _ := LoadClaudeSetupTokenWithStatus(cfg, "claude", "work"); status.Configured {
		t.Fatal("setup token still stored")
	}
}

func TestHubManagesAccountsForClients(t *testing.T) {
	upstream := newProxyUpstream(t)
	cfg, proxy := setupProxyAccounts(t, upstream.server.URL)
	proxy.IdentityLookup = func(context.Context, string) (ClaudeSetupTokenIdentity, error) {
		return ClaudeSetupTokenIdentity{AccountUUID: "uuid-new"}, nil
	}
	hub := httptest.NewServer(proxy)
	t.Cleanup(hub.Close)
	ctx := context.Background()

	if _, err := HubAddAccount(ctx, hub.URL, "sk-ant-oat01-wrong", HubAccountRequest{Account: "new", SetupToken: "sk-ant-oat01-new"}); err == nil {
		t.Fatal("hub accepted an account from a client without its credential")
	}
	result, err := HubAddAccount(ctx, hub.URL, proxy.secret, HubAccountRequest{Account: "new", Email: "new@example.com", SetupToken: "sk-ant-oat01-new"})
	if err != nil || !result.Created {
		t.Fatalf("HubAddAccount = %#v, %v", result, err)
	}
	if token, status, err := LoadClaudeSetupTokenWithStatus(cfg, "claude", "new"); err != nil || !status.Usable || token != "sk-ant-oat01-new" {
		t.Fatalf("hub token = %q, %#v, %v", token, status, err)
	}

	switched, err := HubSwitch(ctx, hub.URL, proxy.secret, "new")
	if err != nil || switched.Active != "new" {
		t.Fatalf("HubSwitch = %#v, %v", switched, err)
	}
	if _, err := HubSwitch(ctx, hub.URL, proxy.secret, "missing"); err == nil || !strings.Contains(err.Error(), "missing") {
		t.Fatalf("switch to a missing account: %v", err)
	}

	if err := HubRemoveAccount(ctx, hub.URL, proxy.secret, "b", false); err != nil {
		t.Fatal(err)
	}
	if _, ok := mustLoadState(t, cfg).Account("claude", "b"); ok {
		t.Fatal("hub kept the removed account")
	}
	if err := HubRemoveAccount(ctx, hub.URL, proxy.secret, "new", false); err == nil || !strings.Contains(err.Error(), "active") {
		t.Fatalf("remove the active account without force: %v", err)
	}
}

func TestCodexHubAddsLoginFromClient(t *testing.T) {
	cfg, proxy := setupCodexProxyAccounts(t, "https://chatgpt.com")
	hub := httptest.NewServer(proxy)
	t.Cleanup(hub.Close)
	login := codexTestLogin(t, "c@example.com", "client")
	result, err := HubAddAccount(context.Background(), hub.URL, proxy.placeholder.Token, HubAccountRequest{Account: "c", AuthJSON: login})
	if err != nil || !result.Created || result.Email != "c@example.com" {
		t.Fatalf("HubAddAccount = %#v, %v", result, err)
	}
	stored, err := os.ReadFile(filepath.Join(AccountDir(cfg, "codex", "c"), "auth.json"))
	if err != nil || string(stored) != string(login) {
		t.Fatalf("hub login = %s, %v", stored, err)
	}
	// A Codex service takes logins, not Claude tokens.
	if _, err := HubAddAccount(context.Background(), hub.URL, proxy.placeholder.Token, HubAccountRequest{Account: "d", SetupToken: "sk-ant-oat01-x"}); err == nil {
		t.Fatal("codex hub accepted a setup token")
	}
	req, _ := http.NewRequest(http.MethodGet, hub.URL+hubAccountsPath, nil)
	req.Header.Set("Authorization", "Bearer "+proxy.placeholder.Token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("GET accounts: %d", resp.StatusCode)
	}
}
