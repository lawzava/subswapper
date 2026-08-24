package subswapper

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestReplaceClaudeSetupTokenCreatesPrivateAtomicStorage(t *testing.T) {
	dir := t.TempDir()
	cfg := setupTokenTestConfig(dir)
	registerSetupTokenTestAccount(t, cfg, "work")

	status, err := ReplaceClaudeSetupToken(context.Background(), cfg, "claude", "work", "setup-token-one", nil)
	if err != nil {
		t.Fatal(err)
	}
	if !status.Configured || !status.Usable || status.Revision == "" {
		t.Fatalf("unexpected token status: %#v", status)
	}
	if status.StoredAt.IsZero() || status.ExpiresAt.Sub(status.StoredAt) != 365*24*time.Hour {
		t.Fatalf("unexpected token lifetime: stored=%v expires=%v", status.StoredAt, status.ExpiresAt)
	}

	path := claudeSetupTokenPath(cfg, "claude", "work")
	if filepath.Dir(path) == AccountDir(cfg, "claude", "work") {
		t.Fatal("setup token was stored inside CLAUDE_CONFIG_DIR")
	}
	for current := filepath.Dir(path); current != filepath.Dir(claudeSetupTokenRoot(cfg)); current = filepath.Dir(current) {
		info, err := os.Lstat(current)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() || info.Mode().Perm() != 0o700 {
			t.Fatalf("private directory %s has mode %v", current, info.Mode())
		}
	}
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
		t.Fatalf("setup token file has mode %v", info.Mode())
	}
	loaded, err := LoadClaudeSetupToken(cfg, "claude", "work")
	if err != nil {
		t.Fatal(err)
	}
	if loaded != "setup-token-one" {
		t.Fatal("loaded setup token did not match input")
	}
}

func TestReplaceClaudeSetupTokenIsAtomicAndClearsProbeState(t *testing.T) {
	dir := t.TempDir()
	cfg := setupTokenTestConfig(dir)
	registerSetupTokenTestAccount(t, cfg, "work")
	if _, err := ReplaceClaudeSetupToken(context.Background(), cfg, "claude", "work", "old-secret", nil); err != nil {
		t.Fatal(err)
	}
	state, err := LoadState(cfg.StatePath)
	if err != nil {
		t.Fatal(err)
	}
	account := state.Service("claude").Accounts["work"]
	oldRevision := account.SetupTokenRevision
	account.Usage = UsageSnapshot{ObservedAt: time.Now().UTC()}
	account.FetchBackoffUntil = time.Now().Add(time.Hour)
	account.CredentialsError = "old credentials failure"
	account.LastProbeError = "old probe failure"
	account.LastProbeStartedAt = time.Now()
	state.Service("claude").Accounts["work"] = account
	if err := SaveState(cfg.StatePath, state); err != nil {
		t.Fatal(err)
	}

	originalReplace := replaceClaudeSetupTokenFile
	replaceClaudeSetupTokenFile = func(_, _ string) error { return errors.New("injected replacement failure") }
	_, replaceErr := ReplaceClaudeSetupToken(context.Background(), cfg, "claude", "work", "new-secret", nil)
	replaceClaudeSetupTokenFile = originalReplace
	if replaceErr == nil {
		t.Fatal("expected replacement failure")
	}
	loaded, err := LoadClaudeSetupToken(cfg, "claude", "work")
	if err != nil {
		t.Fatal(err)
	}
	if loaded != "old-secret" {
		t.Fatal("failed atomic replacement changed the stored token")
	}

	status, err := ReplaceClaudeSetupToken(context.Background(), cfg, "claude", "work", "new-secret", nil)
	if err != nil {
		t.Fatal(err)
	}
	if status.Revision == oldRevision {
		t.Fatal("replacement reused the old random revision")
	}
	state, err = LoadState(cfg.StatePath)
	if err != nil {
		t.Fatal(err)
	}
	account = state.Service("claude").Accounts["work"]
	if account.SetupTokenRevision != status.Revision || !account.Usage.ObservedAt.IsZero() || !account.FetchBackoffUntil.IsZero() || account.CredentialsError != "" || account.LastProbeError != "" || !account.LastProbeStartedAt.IsZero() {
		t.Fatalf("replacement did not reset non-secret probe state: %#v", account)
	}
}

func TestClaudeSetupTokenRejectsDuplicateTokenAndKnownIdentity(t *testing.T) {
	dir := t.TempDir()
	cfg := setupTokenTestConfig(dir)
	registerSetupTokenTestAccount(t, cfg, "one")
	registerSetupTokenTestAccount(t, cfg, "two")
	lookupOne := func(context.Context, string) (ClaudeSetupTokenIdentity, error) {
		return ClaudeSetupTokenIdentity{AccountUUID: "account-uuid-one"}, nil
	}
	if _, err := ReplaceClaudeSetupToken(context.Background(), cfg, "claude", "one", "secret-one", lookupOne); err != nil {
		t.Fatal(err)
	}

	if _, err := ReplaceClaudeSetupToken(context.Background(), cfg, "claude", "two", "secret-one", nil); !errors.Is(err, ErrClaudeSetupTokenDuplicate) {
		t.Fatalf("duplicate token error = %v", err)
	}
	lookupDuplicateIdentity := func(context.Context, string) (ClaudeSetupTokenIdentity, error) {
		return ClaudeSetupTokenIdentity{AccountUUID: "account-uuid-one"}, nil
	}
	if _, err := ReplaceClaudeSetupToken(context.Background(), cfg, "claude", "two", "secret-two", lookupDuplicateIdentity); !errors.Is(err, ErrClaudeSetupTokenIdentityDuplicate) {
		t.Fatalf("duplicate identity error = %v", err)
	}
	if _, err := LoadClaudeSetupToken(cfg, "claude", "two"); !errors.Is(err, ErrClaudeSetupTokenNotConfigured) {
		t.Fatalf("rejected account unexpectedly has a token: %v", err)
	}
}

func TestClaudeSetupTokenRejectsSameValueAsReplacement(t *testing.T) {
	dir := t.TempDir()
	cfg := setupTokenTestConfig(dir)
	registerSetupTokenTestAccount(t, cfg, "work")
	if _, err := ReplaceClaudeSetupToken(context.Background(), cfg, "claude", "work", "unchanged-secret", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := ReplaceClaudeSetupToken(context.Background(), cfg, "claude", "work", "unchanged-secret", nil); !errors.Is(err, ErrClaudeSetupTokenDuplicate) {
		t.Fatalf("same-value replacement error = %v", err)
	}
}

func TestClaudeSetupTokenRejectsDuplicateAcrossClaudeServices(t *testing.T) {
	dir := t.TempDir()
	cfg := setupTokenTestConfig(dir)
	cfg.Services = append(cfg.Services, ServiceConfig{Name: "claude-alt", Kind: "claude", AccountMode: AccountModeHome})
	state := NewState()
	state.Service("claude").Accounts["one"] = AccountState{Name: "one"}
	state.Service("claude-alt").Accounts["two"] = AccountState{Name: "two"}
	if err := SaveState(cfg.StatePath, state); err != nil {
		t.Fatal(err)
	}
	if _, err := ReplaceClaudeSetupToken(context.Background(), cfg, "claude", "one", "shared-secret", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := ReplaceClaudeSetupToken(context.Background(), cfg, "claude-alt", "two", "shared-secret", nil); !errors.Is(err, ErrClaudeSetupTokenDuplicate) {
		t.Fatalf("cross-service duplicate token error = %v", err)
	}
}

func TestClaudeSetupTokenUnknownIdentityDoesNotInferAccountLabel(t *testing.T) {
	dir := t.TempDir()
	cfg := setupTokenTestConfig(dir)
	registerSetupTokenTestAccount(t, cfg, "label-looks-like-an-org")
	status, err := ReplaceClaudeSetupToken(context.Background(), cfg, "claude", "label-looks-like-an-org", "inference-only-secret", nil)
	if err != nil {
		t.Fatal(err)
	}
	if status.IdentityKnown || status.AccountUUID != "" {
		t.Fatalf("unknown identity was inferred from the label: %#v", status)
	}
	state, err := LoadState(cfg.StatePath)
	if err != nil {
		t.Fatal(err)
	}
	if got := state.Service("claude").Accounts["label-looks-like-an-org"].AccountUUID; got != "" {
		t.Fatalf("state inferred identity %q", got)
	}
}

func TestClaudeSetupTokenIdentityLookupErrorIsRedacted(t *testing.T) {
	dir := t.TempDir()
	cfg := setupTokenTestConfig(dir)
	registerSetupTokenTestAccount(t, cfg, "work")
	secret := "secret-from-lookup"
	lookup := func(context.Context, string) (ClaudeSetupTokenIdentity, error) {
		return ClaudeSetupTokenIdentity{}, errors.New("provider rejected " + secret)
	}
	_, err := ReplaceClaudeSetupToken(context.Background(), cfg, "claude", "work", secret, lookup)
	if err == nil || strings.Contains(err.Error(), secret) || strings.Contains(err.Error(), "provider rejected") {
		t.Fatalf("identity lookup error was not redacted: %v", err)
	}
	if _, err := LoadClaudeSetupToken(cfg, "claude", "work"); !errors.Is(err, ErrClaudeSetupTokenNotConfigured) {
		t.Fatalf("lookup failure stored a token: %v", err)
	}
}

func TestRemoveClaudeSetupTokenRemovesSecretAndMetadata(t *testing.T) {
	dir := t.TempDir()
	cfg := setupTokenTestConfig(dir)
	registerSetupTokenTestAccount(t, cfg, "work")
	lookup := func(context.Context, string) (ClaudeSetupTokenIdentity, error) {
		return ClaudeSetupTokenIdentity{AccountUUID: "account-uuid"}, nil
	}
	if _, err := ReplaceClaudeSetupToken(context.Background(), cfg, "claude", "work", "remove-me", lookup); err != nil {
		t.Fatal(err)
	}
	state, err := LoadState(cfg.StatePath)
	if err != nil {
		t.Fatal(err)
	}
	account := state.Service("claude").Accounts["work"]
	account.Usage = UsageSnapshot{ObservedAt: time.Now().UTC()}
	account.LastProbeError = "old probe failure"
	state.Service("claude").Accounts["work"] = account
	if err := SaveState(cfg.StatePath, state); err != nil {
		t.Fatal(err)
	}
	if err := RemoveClaudeSetupToken(context.Background(), cfg, "claude", "work"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(claudeSetupTokenPath(cfg, "claude", "work")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("token file survived removal: %v", err)
	}
	if _, err := LoadClaudeSetupToken(cfg, "claude", "work"); !errors.Is(err, ErrClaudeSetupTokenNotConfigured) {
		t.Fatalf("removed token load error = %v", err)
	}
	status, err := ClaudeSetupTokenStatusForAccount(cfg, "claude", "work")
	if err != nil {
		t.Fatal(err)
	}
	if status.Configured || status.IdentityKnown || status.AccountUUID != "" || status.Revision != "" {
		t.Fatalf("removed token still has status metadata: %#v", status)
	}
	state, err = LoadState(cfg.StatePath)
	if err != nil {
		t.Fatal(err)
	}
	account = state.Service("claude").Accounts["work"]
	if account.SetupTokenRevision != "" || account.AccountUUID != "" || !account.Usage.ObservedAt.IsZero() || account.LastProbeError != "" {
		t.Fatalf("removed token still has state metadata: %#v", account)
	}
}

func TestAccountRemovalRequiresExplicitSetupTokenRemoval(t *testing.T) {
	dir := t.TempDir()
	cfg := setupTokenTestConfig(dir)
	registerSetupTokenTestAccount(t, cfg, "work")
	if _, err := ReplaceClaudeSetupToken(context.Background(), cfg, "claude", "work", "remove-before-account", nil); err != nil {
		t.Fatal(err)
	}
	if err := RemoveAccountWithOptions(cfg, "claude", "work", true, false); err == nil || !strings.Contains(err.Error(), "setup token") {
		t.Fatalf("account removal error = %v", err)
	}
	if _, err := LoadClaudeSetupToken(cfg, "claude", "work"); err != nil {
		t.Fatalf("blocked account removal changed token: %v", err)
	}
	if err := RemoveClaudeSetupToken(context.Background(), cfg, "claude", "work"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(claudeSetupTokenPath(cfg, "claude", "work")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("explicit token removal left a token file: %v", err)
	}
	if err := RemoveAccountWithOptions(cfg, "claude", "work", true, false); err != nil {
		t.Fatal(err)
	}
}

func TestClaudeSetupTokenExpiryIsReportedAndRejected(t *testing.T) {
	dir := t.TempDir()
	cfg := setupTokenTestConfig(dir)
	registerSetupTokenTestAccount(t, cfg, "work")
	fixed := time.Date(2026, time.August, 24, 12, 0, 0, 0, time.UTC)
	originalNow := claudeSetupTokenNow
	claudeSetupTokenNow = func() time.Time { return fixed }
	t.Cleanup(func() { claudeSetupTokenNow = originalNow })
	if _, err := ReplaceClaudeSetupToken(context.Background(), cfg, "claude", "work", "expires-later", nil); err != nil {
		t.Fatal(err)
	}
	claudeSetupTokenNow = func() time.Time { return fixed.Add(claudeSetupTokenLifetime) }
	status, err := ClaudeSetupTokenStatusForAccount(cfg, "claude", "work")
	if err != nil {
		t.Fatal(err)
	}
	if !status.Configured || status.Usable || !status.Expired {
		t.Fatalf("expired token status = %#v", status)
	}
	if _, err := LoadClaudeSetupToken(cfg, "claude", "work"); !errors.Is(err, ErrClaudeSetupTokenExpired) {
		t.Fatalf("expired token load error = %v", err)
	}
}

func TestClaudeSetupTokenRejectsSymlinkTarget(t *testing.T) {
	dir := t.TempDir()
	cfg := setupTokenTestConfig(dir)
	registerSetupTokenTestAccount(t, cfg, "work")
	if _, err := ReplaceClaudeSetupToken(context.Background(), cfg, "claude", "work", "first-secret", nil); err != nil {
		t.Fatal(err)
	}
	path := claudeSetupTokenPath(cfg, "claude", "work")
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	victim := filepath.Join(dir, "victim")
	if err := os.WriteFile(victim, []byte("unchanged"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(victim, path); err != nil {
		t.Fatal(err)
	}
	_, err := ReplaceClaudeSetupToken(context.Background(), cfg, "claude", "work", "second-secret", nil)
	if !errors.Is(err, ErrClaudeSetupTokenStorageUnsafe) {
		t.Fatalf("symlink target error = %v", err)
	}
	data, err := os.ReadFile(victim)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "unchanged" {
		t.Fatal("symlink target was modified")
	}
}

func TestClaudeSetupTokenIsAbsentFromConfigStateAndErrors(t *testing.T) {
	dir := t.TempDir()
	cfg := setupTokenTestConfig(dir)
	registerSetupTokenTestAccount(t, cfg, "work")
	registerSetupTokenTestAccount(t, cfg, "other")
	secret := "never-serialize-this-secret"
	if _, err := ReplaceClaudeSetupToken(context.Background(), cfg, "claude", "work", secret, nil); err != nil {
		t.Fatal(err)
	}
	status, err := ClaudeSetupTokenStatusForAccount(cfg, "claude", "work")
	if err != nil {
		t.Fatal(err)
	}
	_, duplicateErr := ReplaceClaudeSetupToken(context.Background(), cfg, "claude", "other", secret, nil)
	stateData, err := os.ReadFile(cfg.StatePath)
	if err != nil {
		t.Fatal(err)
	}
	configData, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	statusData, err := json.Marshal(status)
	if err != nil {
		t.Fatal(err)
	}
	for source, data := range map[string][]byte{
		"state":  stateData,
		"config": configData,
		"status": statusData,
		"error":  []byte(duplicateErr.Error()),
	} {
		if strings.Contains(string(data), secret) {
			t.Fatalf("%s exposed the setup token", source)
		}
	}
}

func TestClaudeSetupTokenStateFailureLeavesRevisionMismatch(t *testing.T) {
	dir := t.TempDir()
	cfg := setupTokenTestConfig(dir)
	registerSetupTokenTestAccount(t, cfg, "work")
	if _, err := ReplaceClaudeSetupToken(context.Background(), cfg, "claude", "work", "first-secret", nil); err != nil {
		t.Fatal(err)
	}
	originalSave := saveClaudeSetupTokenState
	saveClaudeSetupTokenState = func(string, *State) error { return errors.New("injected state failure") }
	_, replaceErr := ReplaceClaudeSetupToken(context.Background(), cfg, "claude", "work", "second-secret", nil)
	saveClaudeSetupTokenState = originalSave
	if replaceErr == nil {
		t.Fatal("expected state save failure")
	}
	if _, err := LoadClaudeSetupToken(cfg, "claude", "work"); !errors.Is(err, ErrClaudeSetupTokenRevisionMismatch) {
		t.Fatalf("partial replacement did not fail closed: %v", err)
	}
}

func TestLoadStateWithoutSetupTokenFieldsRemainsCompatible(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	legacy := `{"version":1,"services":{"claude":{"active_account":"work","accounts":{"work":{"name":"work","added_at":"2026-08-24T00:00:00Z"}}}}}`
	if err := os.WriteFile(path, []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}
	state, err := LoadState(path)
	if err != nil {
		t.Fatal(err)
	}
	account := state.Service("claude").Accounts["work"]
	if account.AccountUUID != "" || account.SetupTokenRevision != "" {
		t.Fatalf("legacy state gained setup-token metadata: %#v", account)
	}
}

func setupTokenTestConfig(dir string) Config {
	return Config{
		BackupRoot: filepath.Join(dir, "data", "accounts"),
		StatePath:  filepath.Join(dir, "data", "state.json"),
		Services: []ServiceConfig{{
			Name:        "claude",
			Kind:        "claude",
			AccountMode: AccountModeHome,
		}},
	}
}

func registerSetupTokenTestAccount(t *testing.T, cfg Config, accountName string) {
	t.Helper()
	state, err := LoadState(cfg.StatePath)
	if err != nil {
		t.Fatal(err)
	}
	state.Service("claude").Accounts[accountName] = AccountState{Name: accountName, AddedAt: time.Now().UTC()}
	if err := SaveState(cfg.StatePath, state); err != nil {
		t.Fatal(err)
	}
}
