package subswapper

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestBuiltInServicesDefaultToAccountHomes(t *testing.T) {
	dir := t.TempDir()
	cfg := Config{
		BackupRoot: filepath.Join(dir, "accounts"),
		StatePath:  filepath.Join(dir, "state.json"),
		Services: []ServiceConfig{
			{Name: "claude", Kind: "claude"},
			{Name: "codex", Kind: "codex"},
		},
	}

	cfg.ApplyDefaults()

	for _, service := range cfg.Services {
		if !service.UsesAccountHomes() {
			t.Fatalf("service %s did not default to account-home mode", service.Name)
		}
	}
	if got := cfg.Services[0].Files[0].BackupName; got != ".credentials.json" {
		t.Fatalf("Claude credential filename = %q, want .credentials.json", got)
	}
	if got := cfg.Services[1].Files[0].BackupName; got != "auth.json" {
		t.Fatalf("Codex credential filename = %q, want auth.json", got)
	}
}

func TestExplicitManagedFilesRemainBundleMode(t *testing.T) {
	cfg := Config{Services: []ServiceConfig{{
		Name:  "codex",
		Kind:  "codex",
		Files: []ManagedFile{requiredFile("/tmp/live-auth.json", "auth.json")},
	}}}
	cfg.ApplyDefaults()
	if cfg.Services[0].UsesAccountHomes() {
		t.Fatal("an explicit managed-file service unexpectedly changed to account-home mode")
	}
}

func TestCreateAccountHomeRegistersEmptyPrivateHome(t *testing.T) {
	dir := t.TempDir()
	cfg := Config{
		BackupRoot: filepath.Join(dir, "accounts"),
		StatePath:  filepath.Join(dir, "state.json"),
		Services:   []ServiceConfig{{Name: "claude", Kind: "claude"}},
	}
	cfg.ApplyDefaults()

	account, home, err := CreateAccountHome(cfg, "claude", "work", "work@example.com")
	if err != nil {
		t.Fatal(err)
	}
	if account.Name != "work" || account.Email != "work@example.com" {
		t.Fatalf("unexpected account: %#v", account)
	}
	if home != AccountDir(cfg, "claude", "work") {
		t.Fatalf("home = %q, want %q", home, AccountDir(cfg, "claude", "work"))
	}
	info, err := os.Stat(home)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o700 {
		t.Fatalf("home permissions = %o, want 700", info.Mode().Perm())
	}
	state, err := LoadState(cfg.StatePath)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := state.Account("claude", "work"); !ok {
		t.Fatal("created account was not registered")
	}
	if state.Service("claude").ActiveAccount != "work" {
		t.Fatalf("active account = %q, want work", state.Service("claude").ActiveAccount)
	}
	if _, err := os.Stat(filepath.Join(home, ".credentials.json")); !os.IsNotExist(err) {
		t.Fatalf("creating a home unexpectedly created credentials: %v", err)
	}
}

func TestResetAccountProbeStateMakesReloggedHomeImmediatelyProbeable(t *testing.T) {
	dir := t.TempDir()
	cfg := Config{StatePath: filepath.Join(dir, "state.json")}
	state := NewState()
	state.Service("claude").Accounts["work"] = AccountState{
		Name:              "work",
		CredentialsError:  "stored credentials unusable",
		LastProbeError:    "old failure",
		FetchBackoffUntil: time.Now().Add(time.Hour),
	}
	if err := SaveState(cfg.StatePath, state); err != nil {
		t.Fatal(err)
	}
	if err := ResetAccountProbeState(cfg, "claude", "work"); err != nil {
		t.Fatal(err)
	}
	state, err := LoadState(cfg.StatePath)
	if err != nil {
		t.Fatal(err)
	}
	account := state.Service("claude").Accounts["work"]
	if account.CredentialsError != "" || account.LastProbeError != "" || !account.FetchBackoffUntil.IsZero() {
		t.Fatalf("probe state was not reset: %#v", account)
	}
}

func TestAccountEnvironmentUsesProviderHome(t *testing.T) {
	dir := t.TempDir()
	cfg := Config{BackupRoot: filepath.Join(dir, "accounts")}
	claude := ServiceConfig{Name: "claude", Kind: "claude", AccountMode: AccountModeHome}
	codex := ServiceConfig{Name: "codex", Kind: "codex", AccountMode: AccountModeHome}

	if got := AccountEnvironment(cfg, claude, "work"); got["CLAUDE_CONFIG_DIR"] != AccountDir(cfg, "claude", "work") {
		t.Fatalf("Claude environment = %#v", got)
	}
	if got := AccountEnvironment(cfg, codex, "personal"); got["CODEX_HOME"] != AccountDir(cfg, "codex", "personal") {
		t.Fatalf("Codex environment = %#v", got)
	}
}

func TestHomeModeSwitchDoesNotReplaceLiveCredentials(t *testing.T) {
	dir := t.TempDir()
	live := filepath.Join(dir, "live-auth.json")
	if err := os.WriteFile(live, []byte("live-account"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := Config{
		BackupRoot: filepath.Join(dir, "accounts"),
		StatePath:  filepath.Join(dir, "state.json"),
		Services: []ServiceConfig{{
			Name:        "codex",
			Kind:        "codex",
			AccountMode: AccountModeHome,
			Files:       []ManagedFile{requiredFile(live, "auth.json")},
		}},
	}
	state := NewState()
	state.Service("codex").Accounts["a"] = AccountState{Name: "a"}
	state.Service("codex").Accounts["b"] = AccountState{Name: "b"}
	state.Service("codex").ActiveAccount = "a"
	for _, account := range []string{"a", "b"} {
		home := AccountDir(cfg, "codex", account)
		if err := os.MkdirAll(home, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(home, "auth.json"), []byte(account), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := SaveState(cfg.StatePath, state); err != nil {
		t.Fatal(err)
	}

	if err := SwitchAccount(cfg, "codex", "b"); err != nil {
		t.Fatal(err)
	}
	assertFileContent(t, live, "live-account")
	state, err := LoadState(cfg.StatePath)
	if err != nil {
		t.Fatal(err)
	}
	if state.Service("codex").ActiveAccount != "b" {
		t.Fatalf("active account = %q, want b", state.Service("codex").ActiveAccount)
	}
}

func TestHomeCredentialRefreshNeverTouchesLiveCredentials(t *testing.T) {
	dir := t.TempDir()
	live := filepath.Join(dir, "live-auth.json")
	liveData := `{"auth_mode":"chatgpt","tokens":{"access_token":"live","account_id":"live-account"}}`
	if err := os.WriteFile(live, []byte(liveData), 0o600); err != nil {
		t.Fatal(err)
	}
	service := ServiceConfig{
		Name:        "codex",
		Kind:        "codex",
		AccountMode: AccountModeHome,
		Files:       []ManagedFile{requiredFile(live, "auth.json")},
	}
	cfg := Config{
		BackupRoot: filepath.Join(dir, "accounts"),
		StatePath:  filepath.Join(dir, "state.json"),
		Services:   []ServiceConfig{service},
	}
	home := AccountDir(cfg, "codex", "work")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	original := `{"auth_mode":"chatgpt","tokens":{"access_token":"old","account_id":"work-account"}}`
	if err := os.WriteFile(filepath.Join(home, "auth.json"), []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	state := NewState()
	account := AccountState{Name: "work"}
	state.Service("codex").Accounts["work"] = account
	state.Service("codex").ActiveAccount = "work"
	if err := SaveState(cfg.StatePath, state); err != nil {
		t.Fatal(err)
	}
	source, err := findCodexAuth(cfg, service, account, true)
	if err != nil {
		t.Fatal(err)
	}
	updated := `{"auth_mode":"chatgpt","tokens":{"access_token":"refreshed","account_id":"work-account"}}`
	if err := applyCredentialUpdate(testContext(t), cfg, service, account, source, []byte(updated), true); err != nil {
		t.Fatal(err)
	}
	assertFileContent(t, filepath.Join(home, "auth.json"), updated)
	assertFileContent(t, live, liveData)
}

func TestClaudeHomeProbeNeverRefreshesOAuthItself(t *testing.T) {
	refreshCalls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/token" {
			refreshCalls++
			_, _ = w.Write([]byte(`{"access_token":"new","refresh_token":"new-r","expires_in":3600}`))
			return
		}
		w.WriteHeader(http.StatusUnauthorized)
	}))
	t.Cleanup(server.Close)
	oldUsageURL, oldTokenURL := claudeUsageURL, claudeTokenURL
	claudeUsageURL, claudeTokenURL = server.URL+"/usage", server.URL+"/token"
	t.Cleanup(func() { claudeUsageURL, claudeTokenURL = oldUsageURL, oldTokenURL })

	dir := t.TempDir()
	service := ServiceConfig{
		Name:        "claude",
		Kind:        "claude",
		AccountMode: AccountModeHome,
		Files:       []ManagedFile{requiredFile(filepath.Join(dir, "live.json"), ".credentials.json")},
	}
	cfg := Config{BackupRoot: filepath.Join(dir, "accounts"), StatePath: filepath.Join(dir, "state.json"), Services: []ServiceConfig{service}}
	home := AccountDir(cfg, "claude", "work")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	credentials := `{"claudeAiOauth":{"accessToken":"expired","refreshToken":"must-not-be-used"}}`
	if err := os.WriteFile(filepath.Join(home, ".credentials.json"), []byte(credentials), 0o600); err != nil {
		t.Fatal(err)
	}
	account := AccountState{Name: "work"}
	state := NewState()
	state.Service("claude").Accounts["work"] = account
	if err := SaveState(cfg.StatePath, state); err != nil {
		t.Fatal(err)
	}

	if _, err := fetchClaudeUsage(testContext(t), cfg, service, account, false); err == nil || !errors.Is(err, errCredentialsInvalid) {
		t.Fatalf("expected credentials error without refresh, got %v", err)
	}
	if refreshCalls != 0 {
		t.Fatalf("home probe made %d OAuth refresh calls", refreshCalls)
	}
	assertFileContent(t, filepath.Join(home, ".credentials.json"), credentials)
}

func TestMigrateAccountHomesCopiesLegacyClaudeFilesWithoutRemovingThem(t *testing.T) {
	dir := t.TempDir()
	cfg := Config{
		BackupRoot: filepath.Join(dir, "accounts"),
		StatePath:  filepath.Join(dir, "state.json"),
		Services:   []ServiceConfig{{Name: "claude", Kind: "claude"}},
	}
	cfg.ApplyDefaults()
	home := AccountDir(cfg, "claude", "work")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "credentials.json"), []byte("legacy-credentials"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "claude.json"), []byte("legacy-config"), 0o600); err != nil {
		t.Fatal(err)
	}
	state := NewState()
	state.Service("claude").Accounts["work"] = AccountState{Name: "work"}
	if err := SaveState(cfg.StatePath, state); err != nil {
		t.Fatal(err)
	}

	result, err := MigrateAccountHomes(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if result.Copied != 2 || result.Skipped != 0 {
		t.Fatalf("migration result = %#v", result)
	}
	assertFileContent(t, filepath.Join(home, ".credentials.json"), "legacy-credentials")
	assertFileContent(t, filepath.Join(home, ".config.json"), "legacy-config")
	assertFileContent(t, filepath.Join(home, "credentials.json"), "legacy-credentials")
	assertFileContent(t, filepath.Join(home, "claude.json"), "legacy-config")
}

func TestMigrateAccountHomesNeverOverwritesNativeFiles(t *testing.T) {
	dir := t.TempDir()
	cfg := Config{
		BackupRoot: filepath.Join(dir, "accounts"),
		StatePath:  filepath.Join(dir, "state.json"),
		Services:   []ServiceConfig{{Name: "claude", Kind: "claude"}},
	}
	cfg.ApplyDefaults()
	home := AccountDir(cfg, "claude", "work")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "credentials.json"), []byte("legacy"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".credentials.json"), []byte("native"), 0o600); err != nil {
		t.Fatal(err)
	}
	state := NewState()
	state.Service("claude").Accounts["work"] = AccountState{Name: "work"}
	if err := SaveState(cfg.StatePath, state); err != nil {
		t.Fatal(err)
	}

	result, err := MigrateAccountHomes(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if result.Copied != 0 || result.Skipped != 1 {
		t.Fatalf("migration result = %#v", result)
	}
	assertFileContent(t, filepath.Join(home, ".credentials.json"), "native")
}
