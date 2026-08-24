package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/lawzava/subswapper/internal/subswapper"
)

var errTestWriter = errors.New("test writer failed")

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, errTestWriter }

func TestRunPropagatesWriterFailure(t *testing.T) {
	if err := run([]string{"version"}, failingWriter{}, failingWriter{}); !errors.Is(err, errTestWriter) {
		t.Fatalf("version writer error = %v", err)
	}
	if err := run([]string{"help"}, failingWriter{}, failingWriter{}); !errors.Is(err, errTestWriter) {
		t.Fatalf("help writer error = %v", err)
	}
}

func TestRunHelpVersionAndMissingCommand(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if err := run([]string{"help"}, &stdout, &stderr); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout.String(), "Usage:") {
		t.Fatalf("help output:\n%s", stdout.String())
	}
	if !strings.Contains(stdout.String(), "home create") || !strings.Contains(stdout.String(), "home run") {
		t.Fatalf("help output lacks account-home commands:\n%s", stdout.String())
	}
	stdout.Reset()
	if err := run([]string{"version"}, &stdout, &stderr); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(stdout.String(), "subswapper ") {
		t.Fatalf("version output: %q", stdout.String())
	}
	if !strings.Contains(stdout.String(), runtime.Version()) {
		t.Fatalf("version output lacks Go toolchain %q: %q", runtime.Version(), stdout.String())
	}
	if err := run(nil, &stdout, &stderr); err == nil || !strings.Contains(err.Error(), "missing command") {
		t.Fatalf("missing command error = %v", err)
	}
}

func TestRunHomeCreatePathAndEnv(t *testing.T) {
	dir := t.TempDir()
	configPath := writeHomeModeConfig(t, dir, "claude")
	var stdout, stderr bytes.Buffer

	if err := run([]string{"home", "create", "-config", configPath, "-service", "claude", "-account", "work", "-email", "work@example.com"}, &stdout, &stderr); err != nil {
		t.Fatal(err)
	}
	wantHome := filepath.Join(dir, "accounts", "claude", "work")
	if !strings.Contains(stdout.String(), wantHome) {
		t.Fatalf("create output = %q, want home %q", stdout.String(), wantHome)
	}

	stdout.Reset()
	if err := run([]string{"home", "path", "-config", configPath, "-service", "claude", "-account", "work"}, &stdout, &stderr); err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(stdout.String()) != wantHome {
		t.Fatalf("path output = %q, want %q", stdout.String(), wantHome)
	}

	stdout.Reset()
	if err := run([]string{"home", "env", "-config", configPath, "-service", "claude", "-account", "work"}, &stdout, &stderr); err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(stdout.String()); got != "export CLAUDE_CONFIG_DIR='"+wantHome+"'" {
		t.Fatalf("env output = %q", got)
	}
}

func TestRunHomeCommandReceivesSelectedEnvironment(t *testing.T) {
	dir := t.TempDir()
	configPath := writeHomeModeConfig(t, dir, "codex")
	var stdout, stderr bytes.Buffer
	if err := run([]string{"home", "create", "-config", configPath, "-service", "codex", "-account", "personal"}, &stdout, &stderr); err != nil {
		t.Fatal(err)
	}
	stdout.Reset()

	if err := run([]string{"home", "run", "-config", configPath, "-service", "codex", "-account", "personal", "--", "sh", "-c", `printf %s "$CODEX_HOME"`}, &stdout, &stderr); err != nil {
		t.Fatal(err)
	}
	wantHome := filepath.Join(dir, "accounts", "codex", "personal")
	if stdout.String() != wantHome {
		t.Fatalf("run output = %q, want %q", stdout.String(), wantHome)
	}
}

func TestRunHomeTokenSetStatusAndRemoveNeverExposeToken(t *testing.T) {
	dir := t.TempDir()
	configPath := writeHomeModeConfig(t, dir, "claude")
	createHomeAccount(t, configPath, "claude", "work")
	secret := "sk-ant-oat01-command-secret"

	originalLookup := lookupClaudeSetupTokenIdentity
	lookupClaudeSetupTokenIdentity = func(context.Context, string) (subswapper.ClaudeSetupTokenIdentity, error) {
		return subswapper.ClaudeSetupTokenIdentity{}, nil
	}
	t.Cleanup(func() { lookupClaudeSetupTokenIdentity = originalLookup })

	for _, test := range []struct {
		name  string
		args  []string
		stdin string
	}{
		{
			name:  "set",
			args:  []string{"home", "token", "set", "-config", configPath, "-service", "claude", "-account", "work"},
			stdin: secret + "\n",
		},
		{
			name: "status",
			args: []string{"home", "token", "status", "-config", configPath, "-service", "claude", "-account", "work"},
		},
		{
			name: "remove",
			args: []string{"home", "token", "remove", "-config", configPath, "-service", "claude", "-account", "work"},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			err := runWithInput(test.args, strings.NewReader(test.stdin), &stdout, &stderr)
			combined := stdout.String() + stderr.String()
			if err != nil {
				combined += err.Error()
				t.Fatalf("command failed: %v", err)
			}
			if strings.Contains(combined, secret) {
				t.Fatalf("command exposed setup token: %q", combined)
			}
		})
	}

	for _, path := range []string{configPath, filepath.Join(dir, "state.json")} {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(data, []byte(secret)) {
			t.Fatalf("%s exposed setup token", filepath.Base(path))
		}
	}
}

func TestRunHomeTokenRejectsCommandArgument(t *testing.T) {
	dir := t.TempDir()
	configPath := writeHomeModeConfig(t, dir, "claude")
	createHomeAccount(t, configPath, "claude", "work")
	secret := "sk-ant-oat01-argument-secret"
	var stdout, stderr bytes.Buffer
	err := runWithInput(
		[]string{"home", "token", "set", "-config", configPath, "-service", "claude", "-account", "work", secret},
		strings.NewReader(""),
		&stdout,
		&stderr,
	)
	if err == nil {
		t.Fatal("token argument was accepted")
	}
	if strings.Contains(stdout.String()+stderr.String()+err.Error(), secret) {
		t.Fatalf("rejection exposed setup token: %v", err)
	}
}

func TestRunHomeClaudeInjectsIsolatedEnvironmentAndSettings(t *testing.T) {
	dir := t.TempDir()
	configPath := writeHomeModeConfig(t, dir, "claude")
	createHomeAccount(t, configPath, "claude", "work")
	secret := "sk-ant-oat01-launch-secret"
	storeTestSetupToken(t, configPath, "work", secret)
	fakeClaude := writeFakeClaude(t, dir, `
if [ "$1" = auth ] && [ "$2" = status ]; then
  printf '{"loggedIn":true,"authMethod":"oauth_token","apiProvider":"firstParty"}\n'
  exit 0
fi
printf 'token=%s scrub=%s config=%s account=%s api=%s refresh=%s bedrock=%s vertex=%s foundry=%s\n' \
  "$([ "$CLAUDE_CODE_OAUTH_TOKEN" = '`+secret+`' ] && printf yes || printf no)" \
  "$CLAUDE_CODE_SUBPROCESS_ENV_SCRUB" "$CLAUDE_CONFIG_DIR" "$SUBSWAPPER_ACCOUNT" \
  "${ANTHROPIC_API_KEY-unset}" "${CLAUDE_CODE_OAUTH_REFRESH_TOKEN-unset}" \
  "${CLAUDE_CODE_USE_BEDROCK-unset}" "${CLAUDE_CODE_USE_VERTEX-unset}" "${CLAUDE_CODE_USE_FOUNDRY-unset}"
printf 'args='
printf '<%s>' "$@"
`)

	t.Setenv("ANTHROPIC_API_KEY", "conflict")
	t.Setenv("CLAUDE_CODE_OAUTH_REFRESH_TOKEN", "conflict")
	t.Setenv("CLAUDE_CODE_USE_BEDROCK", "1")
	t.Setenv("CLAUDE_CODE_USE_VERTEX", "1")
	t.Setenv("CLAUDE_CODE_USE_FOUNDRY", "1")
	var stdout, stderr bytes.Buffer
	err := runWithInput(
		[]string{"home", "run", "-config", configPath, "-service", "claude", "-account", "work", "--", fakeClaude, "hello"},
		strings.NewReader(""),
		&stdout,
		&stderr,
	)
	if err != nil {
		t.Fatalf("home run failed: %v; stderr=%s", err, stderr.String())
	}
	wantHome := filepath.Join(dir, "accounts", "claude", "work")
	got := stdout.String()
	for _, want := range []string{
		"token=yes", "scrub=1", "config=" + wantHome, "account=work",
		"api=unset", "refresh=unset", "bedrock=unset", "vertex=unset", "foundry=unset",
		"<--settings>", `"statusLine"`, "<hello>",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("home run output lacks %q:\n%s", want, got)
		}
	}
	if strings.Contains(got+stderr.String(), secret) {
		t.Fatalf("home run exposed setup token: %q", got+stderr.String())
	}
}

func TestRunHomeClaudePreservesArbitraryCommandArguments(t *testing.T) {
	dir := t.TempDir()
	configPath := writeHomeModeConfig(t, dir, "claude")
	createHomeAccount(t, configPath, "claude", "work")
	storeTestSetupToken(t, configPath, "work", "sk-ant-oat01-arbitrary-command")
	var stdout, stderr bytes.Buffer
	err := runWithInput(
		[]string{"home", "run", "-config", configPath, "-service", "claude", "-account", "work", "--", "sh", "-c", `printf '%s' "$1"`, "command", "original-argument"},
		strings.NewReader(""),
		&stdout,
		&stderr,
	)
	if err != nil {
		t.Fatal(err)
	}
	if got := stdout.String(); got != "original-argument" {
		t.Fatalf("arbitrary command output = %q", got)
	}
}

func TestRunHomeClaudeRejectsMissingAndFailedAuthenticationWithoutOutput(t *testing.T) {
	for _, test := range []struct {
		name       string
		storeToken bool
		authOutput string
	}{
		{name: "missing"},
		{name: "not logged in", storeToken: true, authOutput: `{"loggedIn":false,"error":"provider-secret"}`},
		{name: "wrong credential source", storeToken: true, authOutput: `{"loggedIn":true,"authMethod":"api_key","apiProvider":"firstParty"}`},
		{name: "wrong provider", storeToken: true, authOutput: `{"loggedIn":true,"authMethod":"oauth_token","apiProvider":"bedrock"}`},
		{name: "malformed", storeToken: true, authOutput: `provider-secret`},
	} {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			configPath := writeHomeModeConfig(t, dir, "claude")
			createHomeAccount(t, configPath, "claude", "work")
			secret := "sk-ant-oat01-rejected-secret"
			if test.storeToken {
				storeTestSetupToken(t, configPath, "work", secret)
			}
			marker := filepath.Join(dir, "executed")
			fakeClaude := writeFakeClaude(t, dir, `
if [ "$1" = auth ] && [ "$2" = status ]; then
  printf '%s\n' '`+test.authOutput+`'
  exit 0
fi
touch '`+marker+`'
`)
			var stdout, stderr bytes.Buffer
			err := runWithInput(
				[]string{"home", "run", "-config", configPath, "-service", "claude", "-account", "work", "--", fakeClaude},
				strings.NewReader(""),
				&stdout,
				&stderr,
			)
			if err == nil {
				t.Fatal("unusable authentication was accepted")
			}
			combined := stdout.String() + stderr.String() + err.Error()
			for _, forbidden := range []string{secret, "provider-secret"} {
				if strings.Contains(combined, forbidden) {
					t.Fatalf("failure exposed secret-bearing output: %q", combined)
				}
			}
			if _, statErr := os.Stat(marker); !errors.Is(statErr, os.ErrNotExist) {
				t.Fatalf("Claude command ran with unusable authentication: %v", statErr)
			}
		})
	}
}

func TestRunHomeClaudeBoundsAuthenticationCheck(t *testing.T) {
	dir := t.TempDir()
	configPath := writeHomeModeConfig(t, dir, "claude")
	createHomeAccount(t, configPath, "claude", "work")
	storeTestSetupToken(t, configPath, "work", "sk-ant-oat01-auth-timeout")
	fakeClaude := writeFakeClaude(t, dir, `
if [ "$1" = auth ] && [ "$2" = status ]; then
  sleep 5
fi
`)
	previousTimeout := claudeAuthStatusTimeout
	claudeAuthStatusTimeout = 50 * time.Millisecond
	t.Cleanup(func() { claudeAuthStatusTimeout = previousTimeout })

	started := time.Now()
	err := runWithInput(
		[]string{"home", "run", "-config", configPath, "-service", "claude", "-account", "work", "--", fakeClaude},
		strings.NewReader(""),
		io.Discard,
		io.Discard,
	)
	if err == nil {
		t.Fatal("timed-out authentication check was accepted")
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("authentication check took %s", elapsed)
	}
	if strings.Contains(err.Error(), "sk-ant-") {
		t.Fatalf("timeout error exposed setup token: %v", err)
	}
}

func TestRunningClaudeProcessKeepsLaunchAccountAfterSwitch(t *testing.T) {
	dir := t.TempDir()
	configPath := writeHomeModeConfig(t, dir, "claude")
	createHomeAccount(t, configPath, "claude", "a")
	createHomeAccount(t, configPath, "claude", "b")
	storeTestSetupToken(t, configPath, "a", "sk-ant-oat01-account-a")
	storeTestSetupToken(t, configPath, "b", "sk-ant-oat01-account-b")
	ready := filepath.Join(dir, "ready")
	release := filepath.Join(dir, "release")
	result := filepath.Join(dir, "result")
	fakeClaude := writeFakeClaude(t, dir, `
if [ "$1" = auth ] && [ "$2" = status ]; then
  printf '{"loggedIn":true,"authMethod":"oauth_token","apiProvider":"firstParty"}\n'
  exit 0
fi
while [ "$1" != run ]; do shift; done
touch "$2"
while [ ! -f "$3" ]; do sleep 0.01; done
printf '%s' "$SUBSWAPPER_ACCOUNT" > "$4"
`)

	errCh := make(chan error, 1)
	go func() {
		errCh <- runWithInput(
			[]string{"home", "run", "-config", configPath, "-service", "claude", "--", fakeClaude, "run", ready, release, result},
			strings.NewReader(""),
			io.Discard,
			io.Discard,
		)
	}()
	waitForFile(t, ready)
	var stdout, stderr bytes.Buffer
	if err := runWithInput([]string{"switch", "-config", configPath, "-service", "claude", "-account", "b"}, strings.NewReader(""), &stdout, &stderr); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(release, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := <-errCh; err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(result)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "a" {
		t.Fatalf("running process account = %q, want a", data)
	}
}

func TestRunHomeClaudeRedactsTokenPrintedByChild(t *testing.T) {
	dir := t.TempDir()
	configPath := writeHomeModeConfig(t, dir, "claude")
	createHomeAccount(t, configPath, "claude", "work")
	secret := "sk-ant-oat01-child-output-secret"
	storeTestSetupToken(t, configPath, "work", secret)
	fakeClaude := writeFakeClaude(t, dir, `
if [ "$1" = auth ] && [ "$2" = status ]; then
  printf '{"loggedIn":true,"authMethod":"oauth_token","apiProvider":"firstParty"}\n'
  exit 0
fi
printf '%s' "$CLAUDE_CODE_OAUTH_TOKEN"
printf '%s' "$CLAUDE_CODE_OAUTH_TOKEN" >&2
`)
	var stdout, stderr bytes.Buffer
	if err := runWithInput(
		[]string{"home", "run", "-config", configPath, "-service", "claude", "-account", "work", "--", fakeClaude},
		strings.NewReader(""),
		&stdout,
		&stderr,
	); err != nil {
		t.Fatal(err)
	}
	combined := stdout.String() + stderr.String()
	if strings.Contains(combined, secret) || !strings.Contains(combined, "[REDACTED]") {
		t.Fatalf("child output was not redacted: %q", combined)
	}
}

func TestSecretRedactingWriterCatchesChunkBoundaries(t *testing.T) {
	secret := "setup-token-across-chunks"
	var output bytes.Buffer
	writer := newSecretRedactingWriter(&output, secret)
	if _, err := writer.Write([]byte("prompt> ")); err != nil {
		t.Fatal(err)
	}
	if got := output.String(); got != "prompt> " {
		t.Fatalf("non-secret prompt was buffered: %q", got)
	}
	for _, part := range []string{"before setup-", "token-across-", "chunks after"} {
		if _, err := writer.Write([]byte(part)); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Flush(); err != nil {
		t.Fatal(err)
	}
	if got := output.String(); got != "prompt> before [REDACTED] after" {
		t.Fatalf("redacted output = %q", got)
	}

	var partialOutput bytes.Buffer
	partialWriter := newSecretRedactingWriter(&partialOutput, secret)
	if _, err := partialWriter.Write([]byte(secret[:len(secret)-1])); err != nil {
		t.Fatal(err)
	}
	if err := partialWriter.Flush(); err != nil {
		t.Fatal(err)
	}
	if got := partialOutput.String(); got != "[REDACTED]" {
		t.Fatalf("partial token prefix was exposed: %q", got)
	}
}

func TestClaudeStatusLineRecordsUsageAndPreservesExistingCommand(t *testing.T) {
	dir := t.TempDir()
	configPath := writeHomeModeConfig(t, dir, "claude")
	createHomeAccount(t, configPath, "claude", "work")
	storeTestSetupToken(t, configPath, "work", "sk-ant-oat01-statusline")
	cfg, err := subswapper.LoadConfig(configPath)
	if err != nil {
		t.Fatal(err)
	}
	status, err := subswapper.ClaudeSetupTokenStatusForAccount(*cfg, "claude", "work")
	if err != nil {
		t.Fatal(err)
	}
	home := filepath.Join(dir, "accounts", "claude", "work")
	settingsPath := filepath.Join(home, "settings.json")
	settings := []byte(`{"theme":"dark","statusLine":{"type":"command","command":"cat"}}`)
	if err := os.WriteFile(settingsPath, settings, 0o600); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	payload := []byte(fmt.Sprintf(`{"workspace":{"project_dir":%q},"rate_limits":{"five_hour":{"used_percentage":12,"resets_at":%d},"seven_day":{"used_percentage":34,"resets_at":%d}}}`,
		dir, now.Add(time.Hour).Unix(), now.Add(24*time.Hour).Unix()))
	t.Setenv("SUBSWAPPER_CONFIG_PATH", configPath)
	t.Setenv("SUBSWAPPER_SERVICE", "claude")
	t.Setenv("SUBSWAPPER_ACCOUNT", "work")
	t.Setenv("SUBSWAPPER_TOKEN_REVISION", status.Revision)
	var stdout, stderr bytes.Buffer
	if err := runWithInput([]string{"claude-statusline"}, bytes.NewReader(payload), &stdout, &stderr); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(stdout.Bytes(), payload) {
		t.Fatalf("existing status-line output changed:\n got %q\nwant %q", stdout.Bytes(), payload)
	}
	storedSettings, err := os.ReadFile(settingsPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(storedSettings, settings) {
		t.Fatalf("existing settings changed:\n got %s\nwant %s", storedSettings, settings)
	}
	state, err := subswapper.LoadState(cfg.StatePath)
	if err != nil {
		t.Fatal(err)
	}
	account, ok := state.Account("claude", "work")
	if !ok || !account.Usage.HasCoreLimits() {
		t.Fatalf("status-line usage was not recorded: %#v", account.Usage)
	}
}

func createHomeAccount(t *testing.T, configPath, service, account string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	if err := runWithInput(
		[]string{"home", "create", "-config", configPath, "-service", service, "-account", account},
		strings.NewReader(""),
		&stdout,
		&stderr,
	); err != nil {
		t.Fatal(err)
	}
}

func storeTestSetupToken(t *testing.T, configPath, account, token string) {
	t.Helper()
	cfg, err := subswapper.LoadConfig(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := subswapper.ReplaceClaudeSetupToken(context.Background(), *cfg, "claude", account, token, nil); err != nil {
		t.Fatal(err)
	}
}

func writeFakeClaude(t *testing.T, dir, body string) string {
	t.Helper()
	root := filepath.Join(dir, fmt.Sprintf("fake-claude-%d", time.Now().UnixNano()))
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "claude")
	if err := os.WriteFile(path, []byte("#!/bin/sh\nset -eu\n"+body), 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}

func waitForFile(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", filepath.Base(path))
}

func TestRemoveHomeAccountPreservesHomeUnlessDeleteRequested(t *testing.T) {
	for _, test := range []struct {
		name       string
		deleteHome bool
	}{
		{name: "preserve"},
		{name: "delete", deleteHome: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			configPath := writeHomeModeConfig(t, dir, "codex")
			var stdout, stderr bytes.Buffer
			if err := run([]string{"home", "create", "-config", configPath, "-service", "codex", "-account", "work"}, &stdout, &stderr); err != nil {
				t.Fatal(err)
			}
			home := filepath.Join(dir, "accounts", "codex", "work")
			marker := filepath.Join(home, "keep-me")
			if err := os.WriteFile(marker, []byte("state"), 0o600); err != nil {
				t.Fatal(err)
			}
			args := []string{"remove", "-config", configPath, "-service", "codex", "-account", "work", "-force"}
			if test.deleteHome {
				args = append(args, "-delete-home")
			}
			if err := run(args, &stdout, &stderr); err != nil {
				t.Fatal(err)
			}
			_, err := os.Stat(marker)
			if test.deleteHome && !os.IsNotExist(err) {
				t.Fatalf("home was not deleted: %v", err)
			}
			if !test.deleteHome && err != nil {
				t.Fatalf("home was not preserved: %v", err)
			}
		})
	}
}

func writeHomeModeConfig(t *testing.T, dir, kind string) string {
	t.Helper()
	cfg := subswapper.Config{
		BackupRoot: filepath.Join(dir, "accounts"),
		StatePath:  filepath.Join(dir, "state.json"),
		Services:   []subswapper.ServiceConfig{{Name: kind, Kind: kind}},
	}
	cfg.ApplyDefaults()
	path := filepath.Join(dir, "config.json")
	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestRunStatusWithCustomProbe(t *testing.T) {
	dir := t.TempDir()
	live := filepath.Join(dir, "active.json")
	if err := os.WriteFile(live, []byte("credential"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := subswapper.Config{
		BackupRoot: filepath.Join(dir, "backups"),
		StatePath:  filepath.Join(dir, "state.json"),
		Services: []subswapper.ServiceConfig{{
			Name:         "svc",
			Kind:         "custom",
			Files:        []subswapper.ManagedFile{{Path: live, BackupName: "auth.json"}},
			UsageCommand: []string{"sh", "-c", `echo '{"five_hour":{"pct":12},"weekly":{"pct":34}}'`},
		}},
	}
	cfg.ApplyDefaults()
	configPath := filepath.Join(dir, "config.json")
	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(configPath, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := subswapper.CaptureAccount(cfg, "svc", "main", ""); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	if err := run([]string{"status", "-config", configPath}, &stdout, &stderr); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout.String(), "svc") || !strings.Contains(stdout.String(), "34%") {
		t.Fatalf("status output:\n%s", stdout.String())
	}
}

func TestMonitorLoopDeduplicatesEvents(t *testing.T) {
	ready := statusResult("ready")
	failing := statusResult("ready (stale usage from Jul13 00:00: probe failed)")
	cycles := []subswapper.CycleResult{
		{Results: failing},
		{Results: failing},
		{Results: ready},
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	calls := 0
	runner := func(context.Context, subswapper.Config, bool) subswapper.CycleResult {
		cycle := cycles[calls]
		calls++
		if calls == len(cycles) {
			cancel()
		}
		return cycle
	}
	var out bytes.Buffer
	if err := runMonitorLoop(ctx, subswapper.Config{}, time.Millisecond, false, false, false, &out, runner); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	if strings.Count(got, "probe failed") != 1 {
		t.Fatalf("failure event count = %d, output:\n%s", strings.Count(got, "probe failed"), got)
	}
	if strings.Count(got, "recovered claude/a") != 1 {
		t.Fatalf("recovery event count = %d, output:\n%s", strings.Count(got, "recovered claude/a"), got)
	}
	if strings.Contains(got, "SERVICE") {
		t.Fatalf("continuous monitor printed a table:\n%s", got)
	}
}

func TestMonitorLoopVerboseAndOnceRenderTables(t *testing.T) {
	for _, test := range []struct {
		name    string
		once    bool
		verbose bool
	}{
		{name: "once", once: true},
		{name: "verbose", verbose: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			runner := func(context.Context, subswapper.Config, bool) subswapper.CycleResult {
				cancel()
				return subswapper.CycleResult{Results: statusResult("ready")}
			}
			var out bytes.Buffer
			if err := runMonitorLoop(ctx, subswapper.Config{}, time.Millisecond, test.once, false, test.verbose, &out, runner); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(out.String(), "SERVICE") {
				t.Fatalf("missing table:\n%s", out.String())
			}
		})
	}
}

func TestMonitorLoopVerboseReportsCycleErrors(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runner := func(context.Context, subswapper.Config, bool) subswapper.CycleResult {
		cancel()
		return subswapper.CycleResult{
			Results: statusResult("ready"),
			Errors:  []error{errors.New("state save failed")},
		}
	}
	var out bytes.Buffer
	if err := runMonitorLoop(ctx, subswapper.Config{}, time.Millisecond, false, false, true, &out, runner); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "state save failed") {
		t.Fatalf("verbose monitor hid cycle error:\n%s", out.String())
	}
}

func statusResult(reason string) []subswapper.ServiceStatus {
	return []subswapper.ServiceStatus{{
		Service: subswapper.ServiceConfig{Name: "claude"},
		Accounts: []subswapper.AccountStatus{{
			Service:    "claude",
			Account:    subswapper.AccountState{Name: "a"},
			Selectable: true,
			Reason:     reason,
		}},
	}}
}
