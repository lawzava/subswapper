package subswapper

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestAcquireStateLockHonorsContext(t *testing.T) {
	dir := t.TempDir()
	cfg := Config{StatePath: filepath.Join(dir, "state.json")}
	first, err := AcquireStateLock(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Release()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	_, err = AcquireStateLock(ctx, cfg)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected deadline exceeded, got %v", err)
	}
}

func TestCaptureAndSwitchAccountCopiesCredentialBundles(t *testing.T) {
	dir := t.TempDir()
	active := filepath.Join(dir, "active-auth.json")
	if err := os.WriteFile(active, []byte(`{"email":"first@example.com","token":"one"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := testConfig(dir, active)

	account, err := CaptureAccount(cfg, "codex", "first", "")
	if err != nil {
		t.Fatal(err)
	}
	if account.Email != "first@example.com" {
		t.Fatalf("expected inferred email, got %q", account.Email)
	}
	if err := os.WriteFile(active, []byte(`{"email":"second@example.com","token":"two"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := CaptureAccount(cfg, "codex", "second", ""); err != nil {
		t.Fatal(err)
	}

	if err := SwitchAccount(cfg, "codex", "first"); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(active)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != `{"email":"first@example.com","token":"one"}` {
		t.Fatalf("unexpected active auth content %q", string(data))
	}
}

func TestClaudeForeignLiveIdentityDoesNotOverwriteBackup(t *testing.T) {
	dir := t.TempDir()
	liveCredentials := filepath.Join(dir, "credentials.json")
	liveConfig := filepath.Join(dir, "claude.json")
	service := ServiceConfig{
		Name: "claude",
		Kind: "claude",
		Files: []ManagedFile{
			requiredFile(liveCredentials, "credentials.json"),
			optionalFile(liveConfig, "claude.json"),
		},
	}
	cfg := Config{BackupRoot: filepath.Join(dir, "backups"), StatePath: filepath.Join(dir, "state.json"), Services: []ServiceConfig{service}}
	accountDir := AccountDir(cfg, "claude", "a")
	if err := os.MkdirAll(accountDir, 0o700); err != nil {
		t.Fatal(err)
	}
	backupCredentials := `{"claudeAiOauth":{"accessToken":"account-a-token"}}`
	if err := os.WriteFile(filepath.Join(accountDir, "credentials.json"), []byte(backupCredentials), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(accountDir, "claude.json"), []byte(`{"oauthAccount":{"accountUuid":"account-a"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(liveCredentials, []byte(`{"claudeAiOauth":{"accessToken":"foreign-token"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(liveConfig, []byte(`{"oauthAccount":{"accountUuid":"account-c"}}`), 0o600); err != nil {
		t.Fatal(err)
	}

	err := syncAccountFiles(cfg, service, "a")
	if err == nil || !strings.Contains(err.Error(), "identity") {
		t.Fatalf("expected identity mismatch, got %v", err)
	}
	assertFileContent(t, filepath.Join(accountDir, "credentials.json"), backupCredentials)
}

func TestClaudeMatchingIdentityAllowsTokenRotation(t *testing.T) {
	dir := t.TempDir()
	liveCredentials := filepath.Join(dir, "credentials.json")
	liveConfig := filepath.Join(dir, "claude.json")
	service := ServiceConfig{
		Name: "claude",
		Kind: "claude",
		Files: []ManagedFile{
			requiredFile(liveCredentials, "credentials.json"),
			optionalFile(liveConfig, "claude.json"),
		},
	}
	cfg := Config{BackupRoot: filepath.Join(dir, "backups"), StatePath: filepath.Join(dir, "state.json"), Services: []ServiceConfig{service}}
	accountDir := AccountDir(cfg, "claude", "a")
	if err := os.MkdirAll(accountDir, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{filepath.Join(accountDir, "claude.json"), liveConfig} {
		if err := os.WriteFile(path, []byte(`{"oauthAccount":{"accountUuid":"account-a"}}`), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(accountDir, "credentials.json"), []byte(`{"claudeAiOauth":{"accessToken":"old"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	rotated := `{"claudeAiOauth":{"accessToken":"rotated"}}`
	if err := os.WriteFile(liveCredentials, []byte(rotated), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := syncAccountFiles(cfg, service, "a"); err != nil {
		t.Fatal(err)
	}
	assertFileContent(t, filepath.Join(accountDir, "credentials.json"), rotated)
}

func TestCodexForeignAccountIDIsRejected(t *testing.T) {
	dir := t.TempDir()
	live := filepath.Join(dir, "auth.json")
	service := ServiceConfig{Name: "codex", Kind: "codex", Files: []ManagedFile{requiredFile(live, "auth.json")}}
	cfg := Config{BackupRoot: filepath.Join(dir, "backups"), StatePath: filepath.Join(dir, "state.json"), Services: []ServiceConfig{service}}
	accountDir := AccountDir(cfg, "codex", "a")
	if err := os.MkdirAll(accountDir, 0o700); err != nil {
		t.Fatal(err)
	}
	backup := `{"auth_mode":"chatgpt","tokens":{"access_token":"old","account_id":"account-a"}}`
	if err := os.WriteFile(filepath.Join(accountDir, "auth.json"), []byte(backup), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(live, []byte(`{"auth_mode":"chatgpt","tokens":{"access_token":"foreign","account_id":"account-c"}}`), 0o600); err != nil {
		t.Fatal(err)
	}

	err := syncAccountFiles(cfg, service, "a")
	if err == nil || !strings.Contains(err.Error(), "identity") {
		t.Fatalf("expected identity mismatch, got %v", err)
	}
	_, probeErr := fetchCodexUsage(testContext(t), cfg, service, AccountState{Name: "a"}, true)
	if probeErr == nil || !errors.Is(probeErr, errCredentialsInvalid) || !strings.Contains(probeErr.Error(), "identity") {
		t.Fatalf("expected probe identity rejection, got %v", probeErr)
	}
	assertFileContent(t, filepath.Join(accountDir, "auth.json"), backup)
}

func TestAmbiguousChangedCredentialsAreRejected(t *testing.T) {
	dir := t.TempDir()
	live := filepath.Join(dir, "auth.json")
	service := ServiceConfig{Name: "codex", Kind: "codex", Files: []ManagedFile{requiredFile(live, "auth.json")}}
	cfg := Config{BackupRoot: filepath.Join(dir, "backups"), StatePath: filepath.Join(dir, "state.json"), Services: []ServiceConfig{service}}
	accountDir := AccountDir(cfg, "codex", "a")
	if err := os.MkdirAll(accountDir, 0o700); err != nil {
		t.Fatal(err)
	}
	backup := `{"auth_mode":"chatgpt","tokens":{"access_token":"old"}}`
	if err := os.WriteFile(filepath.Join(accountDir, "auth.json"), []byte(backup), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(live, []byte(`{"auth_mode":"chatgpt","tokens":{"access_token":"changed"}}`), 0o600); err != nil {
		t.Fatal(err)
	}

	err := syncAccountFiles(cfg, service, "a")
	if err == nil || !strings.Contains(err.Error(), "identity") {
		t.Fatalf("expected ambiguous identity error, got %v", err)
	}
	assertFileContent(t, filepath.Join(accountDir, "auth.json"), backup)
}

func TestCopyManagedFilesRollsBackCommittedTargets(t *testing.T) {
	dir := t.TempDir()
	source1 := filepath.Join(dir, "source1")
	source2 := filepath.Join(dir, "source2")
	target1 := filepath.Join(dir, "target1")
	target2 := filepath.Join(dir, "target2")
	for path, content := range map[string]string{
		source1: "new-one",
		source2: "new-two",
		target1: "old-one",
		target2: "old-two",
	} {
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	oldCommit := commitStagedFile
	commits := 0
	commitStagedFile = func(file stagedFile) error {
		commits++
		if commits == 2 {
			return errors.New("injected second commit failure")
		}
		return file.commit()
	}
	t.Cleanup(func() { commitStagedFile = oldCommit })

	cfg := Config{StatePath: filepath.Join(dir, "state.json")}
	err := copyManagedFiles(cfg, []copySpec{
		{source: source1, target: target1, required: true},
		{source: source2, target: target2, required: true},
	})
	if err == nil {
		t.Fatal("expected second target commit to fail")
	}
	assertFileContent(t, target1, "old-one")
	assertFileContent(t, target2, "old-two")
	rollbacks, globErr := filepath.Glob(filepath.Join(dir, ".subswapper-rollback-*"))
	if globErr != nil {
		t.Fatal(globErr)
	}
	if len(rollbacks) != 0 {
		t.Fatalf("expected rollback cleanup after recovery, got %v", rollbacks)
	}
}

func TestRecoverFileTransactionRestoresOriginalBundle(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "auth.json")
	rollback := filepath.Join(dir, ".subswapper-rollback-test")
	if err := os.WriteFile(target, []byte("partially-committed"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(rollback, []byte("original"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := Config{StatePath: filepath.Join(dir, "state.json")}
	journal := fileTransactionJournal{Entries: []fileTransactionEntry{{
		Target:       target,
		RollbackPath: rollback,
		Existed:      true,
	}}}
	if err := writeFileTransactionJournal(cfg, journal); err != nil {
		t.Fatal(err)
	}

	lock, err := AcquireStateLock(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	lock.Release()
	assertFileContent(t, target, "original")
	if _, err := os.Stat(fileTransactionJournalPath(cfg)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expected recovered journal removal, got %v", err)
	}
	if _, err := os.Stat(rollback); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expected recovered rollback removal, got %v", err)
	}
}

func TestSwitchRollsBackFilesWhenStateCommitFails(t *testing.T) {
	dir := t.TempDir()
	live := filepath.Join(dir, "active.json")
	cfg := testConfig(dir, live)
	captureWithUsage(t, cfg, "codex", live, "account-a", "a", 10, 10)
	captureWithUsage(t, cfg, "codex", live, "account-b", "b", 20, 20)
	oldCommit := commitStagedFile
	commits := 0
	commitStagedFile = func(file stagedFile) error {
		commits++
		if commits == 3 {
			return errors.New("injected state commit failure")
		}
		return file.commit()
	}
	t.Cleanup(func() { commitStagedFile = oldCommit })

	err := SwitchAccount(cfg, "codex", "a")
	if err == nil || !strings.Contains(err.Error(), "injected state commit failure") {
		t.Fatalf("expected state commit failure, got %v", err)
	}
	assertFileContent(t, live, "account-b")
	state, loadErr := LoadState(cfg.StatePath)
	if loadErr != nil {
		t.Fatal(loadErr)
	}
	if got := state.Service("codex").ActiveAccount; got != "b" {
		t.Fatalf("active account = %q, want b", got)
	}
}

func TestSwitchRemainsCommittedWhenRollbackCleanupFails(t *testing.T) {
	dir := t.TempDir()
	live := filepath.Join(dir, "active.json")
	cfg := testConfig(dir, live)
	captureWithUsage(t, cfg, "codex", live, "account-a", "a", 10, 10)
	captureWithUsage(t, cfg, "codex", live, "account-b", "b", 20, 20)
	oldRemove := removeRollbackFile
	removeRollbackFile = func(string) error { return errors.New("injected rollback cleanup failure") }
	t.Cleanup(func() { removeRollbackFile = oldRemove })

	if err := SwitchAccount(cfg, "codex", "a"); err != nil {
		t.Fatalf("committed switch reported failure: %v", err)
	}
	assertFileContent(t, live, "account-a")
	state, err := LoadState(cfg.StatePath)
	if err != nil {
		t.Fatal(err)
	}
	if got := state.Service("codex").ActiveAccount; got != "a" {
		t.Fatalf("active account = %q, want a", got)
	}
}

func TestCaptureRollsBackBackupWhenStateCommitFails(t *testing.T) {
	realDir := t.TempDir()
	dir := filepath.Join(t.TempDir(), "linked")
	if err := os.Symlink(realDir, dir); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	live := filepath.Join(dir, "active.json")
	cfg := testConfig(dir, live)
	if err := os.WriteFile(live, []byte("old-credential"), 0o600); err != nil {
		t.Fatal(err)
	}
	first, err := CaptureAccount(cfg, "codex", "a", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(live, []byte("new-credential"), 0o600); err != nil {
		t.Fatal(err)
	}
	oldCommit := commitStagedFile
	commitStagedFile = func(file stagedFile) error {
		if file.target == resolveTargetPath(ExpandPath(cfg.StatePath)) {
			return errors.New("injected state commit failure")
		}
		return file.commit()
	}
	t.Cleanup(func() { commitStagedFile = oldCommit })

	if _, err := CaptureAccount(cfg, "codex", "a", ""); err == nil || !strings.Contains(err.Error(), "injected state commit failure") {
		t.Fatalf("expected state commit failure, got %v", err)
	}
	assertFileContent(t, filepath.Join(AccountDir(cfg, "codex", "a"), "auth.json"), "old-credential")
	state, err := LoadState(cfg.StatePath)
	if err != nil {
		t.Fatal(err)
	}
	if got := state.Service("codex").Accounts["a"].AddedAt; !got.Equal(first.AddedAt) {
		t.Fatalf("AddedAt changed after failed capture: got %s want %s", got, first.AddedAt)
	}
}

func TestSuccessfulTransactionLeavesNoJournalOrRollbackFiles(t *testing.T) {
	dir := t.TempDir()
	source1 := filepath.Join(dir, "source1")
	source2 := filepath.Join(dir, "source2")
	target1 := filepath.Join(dir, "target1")
	target2 := filepath.Join(dir, "target2")
	for path, content := range map[string]string{
		source1: "new-one",
		source2: "new-two",
		target1: "old-one",
		target2: "old-two",
	} {
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	cfg := Config{StatePath: filepath.Join(dir, "state.json")}
	if err := copyManagedFiles(cfg, []copySpec{
		{source: source1, target: target1, required: true},
		{source: source2, target: target2, required: true},
	}); err != nil {
		t.Fatal(err)
	}
	assertFileContent(t, target1, "new-one")
	assertFileContent(t, target2, "new-two")
	if _, err := os.Stat(fileTransactionJournalPath(cfg)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expected no journal, got %v", err)
	}
	rollbacks, err := filepath.Glob(filepath.Join(dir, ".subswapper-rollback-*"))
	if err != nil {
		t.Fatal(err)
	}
	if len(rollbacks) != 0 {
		t.Fatalf("expected no rollback files, got %v", rollbacks)
	}
}

func TestMonitorOnceSwitchesEachServiceToLeastUsedAccount(t *testing.T) {
	dir := t.TempDir()
	claudeActive := filepath.Join(dir, "claude-active.json")
	codexActive := filepath.Join(dir, "codex-active.json")
	if err := os.WriteFile(claudeActive, []byte("claude-a"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(codexActive, []byte("codex-a"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := Config{
		BackupRoot: filepath.Join(dir, "backups"),
		StatePath:  filepath.Join(dir, "state.json"),
		Services: []ServiceConfig{
			testService("claude", claudeActive),
			testService("codex", codexActive),
		},
	}
	cfg.ApplyDefaults()
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}

	captureWithUsage(t, cfg, "claude", claudeActive, "claude-a", "a", 90, 80)
	captureWithUsage(t, cfg, "claude", claudeActive, "claude-b", "b", 10, 20)
	captureWithUsage(t, cfg, "codex", codexActive, "codex-a", "a", 60, 90)
	captureWithUsage(t, cfg, "codex", codexActive, "codex-b", "b", 5, 10)
	if err := SwitchAccount(cfg, "claude", "a"); err != nil {
		t.Fatal(err)
	}
	if err := SwitchAccount(cfg, "codex", "a"); err != nil {
		t.Fatal(err)
	}
	setServiceLastSwitchedAt(t, cfg, "claude", time.Now().Add(-defaultAutoSwitchCooldown-time.Minute))
	setServiceLastSwitchedAt(t, cfg, "codex", time.Now().Add(-defaultAutoSwitchCooldown-time.Minute))

	result := MonitorOnce(testContext(t), cfg, true)
	if err := errors.Join(result.Errors...); err != nil {
		t.Fatal(err)
	}
	if len(result.Switches) != 2 {
		t.Fatalf("expected 2 switches, got %d", len(result.Switches))
	}

	assertFileContent(t, claudeActive, "claude-b")
	assertFileContent(t, codexActive, "codex-b")
}

func TestMonitorOnceDoesNotSwitchBelowThreshold(t *testing.T) {
	dir := t.TempDir()
	active := filepath.Join(dir, "claude-active.json")
	if err := os.WriteFile(active, []byte("claude-a"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := Config{
		BackupRoot: filepath.Join(dir, "backups"),
		StatePath:  filepath.Join(dir, "state.json"),
		Services: []ServiceConfig{
			testService("claude", active),
		},
	}
	cfg.ApplyDefaults()
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}

	captureWithUsage(t, cfg, "claude", active, "claude-a", "a", 80, 80)
	captureWithUsage(t, cfg, "claude", active, "claude-b", "b", 10, 20)
	if err := SwitchAccount(cfg, "claude", "a"); err != nil {
		t.Fatal(err)
	}
	setServiceLastSwitchedAt(t, cfg, "claude", time.Now().Add(-defaultAutoSwitchCooldown-time.Minute))

	result := MonitorOnce(testContext(t), cfg, true)
	if err := errors.Join(result.Errors...); err != nil {
		t.Fatal(err)
	}
	if len(result.Switches) != 0 {
		t.Fatalf("expected no switches below threshold, got %d", len(result.Switches))
	}
	assertFileContent(t, active, "claude-a")
}

func TestMonitorOnceDoesNotSwitchDuringCooldown(t *testing.T) {
	dir := t.TempDir()
	active := filepath.Join(dir, "claude-active.json")
	if err := os.WriteFile(active, []byte("claude-a"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := Config{
		BackupRoot: filepath.Join(dir, "backups"),
		StatePath:  filepath.Join(dir, "state.json"),
		Services: []ServiceConfig{
			testService("claude", active),
		},
	}
	cfg.ApplyDefaults()
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}

	captureWithUsage(t, cfg, "claude", active, "claude-a", "a", 95, 95)
	captureWithUsage(t, cfg, "claude", active, "claude-b", "b", 10, 20)
	if err := SwitchAccount(cfg, "claude", "a"); err != nil {
		t.Fatal(err)
	}

	result := MonitorOnce(testContext(t), cfg, true)
	if err := errors.Join(result.Errors...); err != nil {
		t.Fatal(err)
	}
	if len(result.Switches) != 0 {
		t.Fatalf("expected no switches during cooldown, got %d", len(result.Switches))
	}
	assertFileContent(t, active, "claude-a")
}

func TestMonitorOnceDoesNotSwitchForSmallImprovement(t *testing.T) {
	dir := t.TempDir()
	active := filepath.Join(dir, "claude-active.json")
	if err := os.WriteFile(active, []byte("claude-a"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := Config{
		BackupRoot: filepath.Join(dir, "backups"),
		StatePath:  filepath.Join(dir, "state.json"),
		Services: []ServiceConfig{
			testService("claude", active),
		},
	}
	cfg.ApplyDefaults()
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}

	captureWithUsage(t, cfg, "claude", active, "claude-a", "a", 95, 95)
	captureWithUsage(t, cfg, "claude", active, "claude-b", "b", 88, 88)
	if err := SwitchAccount(cfg, "claude", "a"); err != nil {
		t.Fatal(err)
	}
	setServiceLastSwitchedAt(t, cfg, "claude", time.Now().Add(-defaultAutoSwitchCooldown-time.Minute))

	result := MonitorOnce(testContext(t), cfg, true)
	if err := errors.Join(result.Errors...); err != nil {
		t.Fatal(err)
	}
	if len(result.Switches) != 0 {
		t.Fatalf("expected no switches for small improvement, got %d", len(result.Switches))
	}
	assertFileContent(t, active, "claude-a")
}

func TestMonitorOnceSwitchesWhenFableWeeklyHitsThreshold(t *testing.T) {
	dir := t.TempDir()
	active := filepath.Join(dir, "claude-active.json")
	if err := os.WriteFile(active, []byte("claude-a"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := Config{
		BackupRoot: filepath.Join(dir, "backups"),
		StatePath:  filepath.Join(dir, "state.json"),
		Services: []ServiceConfig{
			testService("claude", active),
		},
	}
	cfg.ApplyDefaults()
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}

	fableHeavy := usageForTest(10, 10)
	fableHeavy.FableWeekly = LimitWindow{Pct: PtrFloat64(90)}
	captureWithUsageSnapshot(t, cfg, "claude", active, "claude-a", "a", fableHeavy)
	captureWithUsage(t, cfg, "claude", active, "claude-b", "b", 20, 20)
	if err := SwitchAccount(cfg, "claude", "a"); err != nil {
		t.Fatal(err)
	}
	setServiceLastSwitchedAt(t, cfg, "claude", time.Now().Add(-defaultAutoSwitchCooldown-time.Minute))

	result := MonitorOnce(testContext(t), cfg, true)
	if err := errors.Join(result.Errors...); err != nil {
		t.Fatal(err)
	}
	if len(result.Switches) != 1 {
		t.Fatalf("expected Fable-triggered switch, got %d", len(result.Switches))
	}
	assertFileContent(t, active, "claude-b")
}

func TestCaptureRemovesStaleOptionalBackup(t *testing.T) {
	dir := t.TempDir()
	required := filepath.Join(dir, "required.json")
	optional := filepath.Join(dir, "optional.json")
	if err := os.WriteFile(required, []byte("required"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(optional, []byte("optional"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := Config{
		BackupRoot: filepath.Join(dir, "backups"),
		StatePath:  filepath.Join(dir, "state.json"),
		Services: []ServiceConfig{
			{
				Name: "claude",
				Kind: "custom",
				Files: []ManagedFile{
					requiredFile(required, "required.json"),
					optionalFile(optional, "optional.json"),
				},
			},
		},
	}
	cfg.ApplyDefaults()
	if _, err := CaptureAccount(cfg, "claude", "work", ""); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(optional); err != nil {
		t.Fatal(err)
	}
	if _, err := CaptureAccount(cfg, "claude", "work", ""); err != nil {
		t.Fatal(err)
	}

	backupPath := filepath.Join(AccountDir(cfg, "claude", "work"), "optional.json")
	if _, err := os.Stat(backupPath); !os.IsNotExist(err) {
		t.Fatalf("expected stale optional backup to be removed, got %v", err)
	}
}

func TestRemoveAccountDeletesStateAndBackup(t *testing.T) {
	dir := t.TempDir()
	active := filepath.Join(dir, "active-auth.json")
	if err := os.WriteFile(active, []byte(`{"email":"first@example.com","token":"one"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := testConfig(dir, active)
	if _, err := CaptureAccount(cfg, "codex", "first", ""); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(active, []byte(`{"email":"second@example.com","token":"two"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := CaptureAccount(cfg, "codex", "second", ""); err != nil {
		t.Fatal(err)
	}
	if err := SwitchAccount(cfg, "codex", "second"); err != nil {
		t.Fatal(err)
	}

	if err := RemoveAccount(cfg, "codex", "first", false); err != nil {
		t.Fatal(err)
	}
	state, err := LoadState(cfg.StatePath)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := state.Service("codex").Accounts["first"]; ok {
		t.Fatal("expected first account to be removed from state")
	}
	if _, err := os.Stat(AccountDir(cfg, "codex", "first")); !os.IsNotExist(err) {
		t.Fatalf("expected backup directory removal, got %v", err)
	}
}

func TestRemoveAccountRollsBackBackupWhenStateCommitFails(t *testing.T) {
	dir := t.TempDir()
	active := filepath.Join(dir, "active-auth.json")
	cfg := testConfig(dir, active)
	captureWithUsage(t, cfg, "codex", active, "account-a", "a", 10, 10)
	captureWithUsage(t, cfg, "codex", active, "account-b", "b", 20, 20)
	oldCommit := commitStagedFile
	commitStagedFile = func(file stagedFile) error {
		if file.target == resolveTargetPath(ExpandPath(cfg.StatePath)) {
			return errors.New("injected state commit failure")
		}
		return file.commit()
	}
	t.Cleanup(func() { commitStagedFile = oldCommit })

	if err := RemoveAccount(cfg, "codex", "a", false); err == nil || !strings.Contains(err.Error(), "injected state commit failure") {
		t.Fatalf("expected state commit failure, got %v", err)
	}
	assertFileContent(t, filepath.Join(AccountDir(cfg, "codex", "a"), "auth.json"), "account-a")
	state, err := LoadState(cfg.StatePath)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := state.Service("codex").Accounts["a"]; !ok {
		t.Fatal("failed removal disappeared from state")
	}
}

func TestRemoveActiveAccountRequiresForce(t *testing.T) {
	dir := t.TempDir()
	active := filepath.Join(dir, "active-auth.json")
	if err := os.WriteFile(active, []byte(`{"email":"first@example.com","token":"one"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := testConfig(dir, active)
	if _, err := CaptureAccount(cfg, "codex", "first", ""); err != nil {
		t.Fatal(err)
	}

	if err := RemoveAccount(cfg, "codex", "first", false); err == nil {
		t.Fatal("expected active account removal to fail without force")
	}
	if err := RemoveAccount(cfg, "codex", "first", true); err != nil {
		t.Fatal(err)
	}
	state, err := LoadState(cfg.StatePath)
	if err != nil {
		t.Fatal(err)
	}
	if state.Service("codex").ActiveAccount != "" {
		t.Fatalf("expected active account to be cleared, got %q", state.Service("codex").ActiveAccount)
	}
}

func TestSafeNameAvoidsCollisionsForUnsafeNames(t *testing.T) {
	if safeName("a/b") == safeName("a_b") {
		t.Fatal("expected unsafe names to include a collision-resistant suffix")
	}
}

func captureWithUsage(t *testing.T, cfg Config, serviceName, activePath, content, account string, fiveHourPct, weeklyPct float64) {
	t.Helper()
	captureWithUsageSnapshot(t, cfg, serviceName, activePath, content, account, usageForTest(fiveHourPct, weeklyPct))
}

func captureWithUsageSnapshot(t *testing.T, cfg Config, serviceName, activePath, content, account string, usage UsageSnapshot) {
	t.Helper()
	if err := os.WriteFile(activePath, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := CaptureAccount(cfg, serviceName, account, account+"@example.com"); err != nil {
		t.Fatal(err)
	}
	state, err := LoadState(cfg.StatePath)
	if err != nil {
		t.Fatal(err)
	}
	service := state.Service(serviceName)
	updated := service.Accounts[account]
	updated.Usage = usage
	service.Accounts[account] = updated
	if err := SaveState(cfg.StatePath, state); err != nil {
		t.Fatal(err)
	}
}

func setServiceLastSwitchedAt(t *testing.T, cfg Config, serviceName string, when time.Time) {
	t.Helper()
	state, err := LoadState(cfg.StatePath)
	if err != nil {
		t.Fatal(err)
	}
	state.Service(serviceName).LastSwitchedAt = when.UTC()
	if err := SaveState(cfg.StatePath, state); err != nil {
		t.Fatal(err)
	}
}

func testConfig(dir, active string) Config {
	cfg := Config{
		BackupRoot: filepath.Join(dir, "backups"),
		StatePath:  filepath.Join(dir, "state.json"),
		Services: []ServiceConfig{
			testService("codex", active),
		},
	}
	cfg.ApplyDefaults()
	return cfg
}

func testService(name, active string) ServiceConfig {
	return ServiceConfig{
		Name: name,
		Kind: "custom",
		Files: []ManagedFile{
			requiredFile(active, "auth.json"),
		},
	}
}

func usageForTest(fiveHourPct, weeklyPct float64) UsageSnapshot {
	return UsageSnapshot{
		FiveHour: LimitWindow{
			Pct: PtrFloat64(fiveHourPct),
		},
		Weekly: LimitWindow{
			Pct: PtrFloat64(weeklyPct),
		},
	}
}

func assertFileContent(t *testing.T, path, want string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != want {
		t.Fatalf("expected %q, got %q", want, string(data))
	}
}
