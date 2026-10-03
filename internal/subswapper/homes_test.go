package subswapper

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
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
