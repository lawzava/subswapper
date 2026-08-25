package subswapper

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"slices"
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
	t.Setenv("HOME", filepath.Join(dir, "native-home"))
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

func TestCreateClaudeAccountHomeLinksOnlyAllowlistedNativeConfiguration(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows symlink creation depends on Developer Mode or elevated privileges")
	}
	dir := t.TempDir()
	nativeHome := filepath.Join(dir, "native-home")
	nativeClaude := filepath.Join(nativeHome, ".claude")
	t.Setenv("HOME", nativeHome)
	if err := os.MkdirAll(nativeClaude, 0o700); err != nil {
		t.Fatal(err)
	}
	sharedFiles := []string{"CLAUDE.md", "settings.json", "keybindings.json"}
	sharedDirs := []string{"plugins", "skills", "agents", "output-styles", "rules", "commands", "workflows", "themes"}
	for _, name := range sharedFiles {
		if err := os.WriteFile(filepath.Join(nativeClaude, name), []byte("shared configuration"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range sharedDirs {
		if err := os.Mkdir(filepath.Join(nativeClaude, name), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	isolatedFiles := []string{".credentials.json", ".config.json", "history.jsonl", "stats-cache.json", "mcp-needs-auth-cache.json"}
	isolatedDirs := []string{"projects", "sessions", "agent-memory", "file-history", "tasks", "teams"}
	for _, name := range isolatedFiles {
		if err := os.WriteFile(filepath.Join(nativeClaude, name), []byte("private runtime state"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range isolatedDirs {
		if err := os.Mkdir(filepath.Join(nativeClaude, name), 0o700); err != nil {
			t.Fatal(err)
		}
	}

	cfg := Config{
		BackupRoot: filepath.Join(dir, "accounts"),
		StatePath:  filepath.Join(dir, "state.json"),
		Services:   []ServiceConfig{{Name: "claude", Kind: "claude"}},
	}
	cfg.ApplyDefaults()
	_, home, err := CreateAccountHome(cfg, "claude", "work", "")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range append(sharedFiles, sharedDirs...) {
		target := filepath.Join(home, name)
		info, err := os.Lstat(target)
		if err != nil {
			t.Fatalf("shared configuration %s: %v", name, err)
		}
		if info.Mode()&os.ModeSymlink == 0 {
			t.Fatalf("shared configuration %s is not a symlink", name)
		}
		link, err := os.Readlink(target)
		if err != nil {
			t.Fatal(err)
		}
		if want := filepath.Join(nativeClaude, name); link != want {
			t.Fatalf("shared configuration %s links to %q, want %q", name, link, want)
		}
	}
	for _, name := range append(isolatedFiles, isolatedDirs...) {
		if _, err := os.Lstat(filepath.Join(home, name)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("isolated path %s was inherited: %v", name, err)
		}
	}
}

func TestRepairClaudeAccountHomeIsIdempotentAndReportsMissingSources(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows symlink creation depends on Developer Mode or elevated privileges")
	}
	dir := t.TempDir()
	nativeHome := filepath.Join(dir, "native-home")
	nativeClaude := filepath.Join(nativeHome, ".claude")
	t.Setenv("HOME", nativeHome)
	if err := os.MkdirAll(nativeClaude, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"CLAUDE.md", "settings.json"} {
		if err := os.WriteFile(filepath.Join(nativeClaude, name), []byte("shared"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
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
	state := NewState()
	state.Service("claude").Accounts["work"] = AccountState{Name: "work"}
	if err := SaveState(cfg.StatePath, state); err != nil {
		t.Fatal(err)
	}

	first, err := RepairAccountHome(cfg, "claude", "work")
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(first.Linked, []string{"CLAUDE.md", "settings.json"}) {
		t.Fatalf("first linked = %v", first.Linked)
	}
	if len(first.Unchanged) != 0 || len(first.Missing) == 0 || len(first.Conflicts) != 0 {
		t.Fatalf("first repair = %#v", first)
	}
	second, err := RepairAccountHome(cfg, "claude", "work")
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(second.Unchanged, []string{"CLAUDE.md", "settings.json"}) || len(second.Linked) != 0 {
		t.Fatalf("second repair = %#v", second)
	}
	if !slices.Equal(second.Missing, first.Missing) || len(second.Conflicts) != 0 {
		t.Fatalf("second repair changed missing sources or conflicts: %#v", second)
	}
}

func TestRepairClaudeAccountHomePreservesFilesDirectoriesAndMismatchedSymlinks(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows symlink creation depends on Developer Mode or elevated privileges")
	}
	dir := t.TempDir()
	nativeHome := filepath.Join(dir, "native-home")
	nativeClaude := filepath.Join(nativeHome, ".claude")
	t.Setenv("HOME", nativeHome)
	for _, name := range []string{"settings.json", "agents", "skills", "CLAUDE.md"} {
		path := filepath.Join(nativeClaude, name)
		if filepath.Ext(name) != "" {
			if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte("native"), 0o600); err != nil {
				t.Fatal(err)
			}
		} else if err := os.MkdirAll(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	cfg := Config{
		BackupRoot: filepath.Join(dir, "accounts"),
		StatePath:  filepath.Join(dir, "state.json"),
		Services:   []ServiceConfig{{Name: "claude", Kind: "claude"}},
	}
	cfg.ApplyDefaults()
	home := AccountDir(cfg, "claude", "work")
	if err := os.MkdirAll(filepath.Join(home, "agents"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "settings.json"), []byte("account settings stay private"), 0o600); err != nil {
		t.Fatal(err)
	}
	wrongTarget := filepath.Join(dir, "other-skills")
	if err := os.Symlink(wrongTarget, filepath.Join(home, "skills")); err != nil {
		t.Fatal(err)
	}
	state := NewState()
	state.Service("claude").Accounts["work"] = AccountState{Name: "work"}
	if err := SaveState(cfg.StatePath, state); err != nil {
		t.Fatal(err)
	}

	result, err := RepairAccountHome(cfg, "claude", "work")
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(result.Conflicts, []string{"settings.json", "skills", "agents"}) {
		t.Fatalf("conflicts = %v", result.Conflicts)
	}
	if !slices.Contains(result.Linked, "CLAUDE.md") {
		t.Fatalf("non-conflicting source was not linked: %#v", result)
	}
	assertFileContent(t, filepath.Join(home, "settings.json"), "account settings stay private")
	if info, err := os.Stat(filepath.Join(home, "agents")); err != nil || !info.IsDir() {
		t.Fatalf("existing agents directory changed: %v", err)
	}
	if got, err := os.Readlink(filepath.Join(home, "skills")); err != nil || got != wrongTarget {
		t.Fatalf("mismatched skills symlink changed: target=%q err=%v", got, err)
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

func TestRuntimeHomeUsesSharedClaudePathOnlyWhenConfigured(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	cfg := Config{BackupRoot: filepath.Join(dir, "accounts")}
	isolated := ServiceConfig{Name: "claude", Kind: "claude", AccountMode: AccountModeHome}
	shared := isolated
	shared.SharedRuntimeHome = "~/.local/share/subswapper/shared-claude"

	if got, want := RuntimeHome(cfg, isolated, "work"), AccountDir(cfg, "claude", "work"); got != want {
		t.Fatalf("isolated runtime home = %q, want %q", got, want)
	}
	if got, want := RuntimeHome(cfg, shared, "work"), filepath.Join(dir, ".local", "share", "subswapper", "shared-claude"); got != want {
		t.Fatalf("shared runtime home = %q, want %q", got, want)
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
