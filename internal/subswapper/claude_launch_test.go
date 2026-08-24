package subswapper

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBuildClaudeLaunchEnvironmentIsolatesSelectedAccount(t *testing.T) {
	token := "setup-token-account-a"
	base := []string{
		"PATH=/usr/bin",
		"LANG=en_US.UTF-8",
		"CLAUDE_CONFIG_DIR=/old/account",
		"CLAUDE_CODE_OAUTH_TOKEN=old-secret",
		"CLAUDE_CODE_SUBPROCESS_ENV_SCRUB=0",
		"SUBSWAPPER_ACCOUNT=old-account",
		"SUBSWAPPER_STALE_ROUTE=old-route",
	}

	got, err := BuildClaudeLaunchEnvironment(base, "/accounts/claude/account-a", token, map[string]string{
		"SUBSWAPPER_ACCOUNT": "account-a",
		"SUBSWAPPER_SERVICE": "claude",
	})
	if err != nil {
		t.Fatal(err)
	}
	env := environmentMap(got)
	want := map[string]string{
		"PATH":                             "/usr/bin",
		"LANG":                             "en_US.UTF-8",
		"CLAUDE_CONFIG_DIR":                "/accounts/claude/account-a",
		"CLAUDE_CODE_OAUTH_TOKEN":          token,
		"CLAUDE_CODE_SUBPROCESS_ENV_SCRUB": "1",
		"SUBSWAPPER_ACCOUNT":               "account-a",
		"SUBSWAPPER_SERVICE":               "claude",
	}
	if len(env) != len(want) {
		t.Fatalf("environment = %#v, want %#v", env, want)
	}
	for key, value := range want {
		if env[key] != value {
			t.Errorf("%s = %q, want %q", key, env[key], value)
		}
	}
	if _, ok := env["SUBSWAPPER_STALE_ROUTE"]; ok {
		t.Error("stale Subswapper metadata was preserved")
	}
}

func TestBuildClaudeLaunchEnvironmentRemovesConflictingCredentials(t *testing.T) {
	conflicts := []string{
		// Direct Anthropic API, profile, and federation routes.
		"ANTHROPIC_API_KEY",
		"ANTHROPIC_AUTH_TOKEN",
		"ANTHROPIC_BASE_URL",
		"ANTHROPIC_CUSTOM_HEADERS",
		"ANTHROPIC_CONFIG_DIR",
		"ANTHROPIC_PROFILE",
		"ANTHROPIC_FEDERATION_RULE_ID",
		"ANTHROPIC_ORGANIZATION_ID",
		"ANTHROPIC_SERVICE_ACCOUNT_ID",
		"ANTHROPIC_WORKSPACE_ID",
		"ANTHROPIC_IDENTITY_TOKEN",
		"ANTHROPIC_IDENTITY_TOKEN_FILE",
		// Login provisioning must not refresh or replace the setup token.
		"CLAUDE_CODE_OAUTH_REFRESH_TOKEN",
		"CLAUDE_CODE_OAUTH_SCOPES",
		// Amazon Bedrock, Mantle, and the AWS credential chain.
		"CLAUDE_CODE_USE_BEDROCK",
		"CLAUDE_CODE_USE_MANTLE",
		"CLAUDE_CODE_SKIP_BEDROCK_AUTH",
		"CLAUDE_CODE_SKIP_MANTLE_AUTH",
		"ANTHROPIC_BEDROCK_BASE_URL",
		"ANTHROPIC_BEDROCK_MANTLE_BASE_URL",
		"ANTHROPIC_AWS_API_KEY",
		"ANTHROPIC_AWS_WORKSPACE_ID",
		"AWS_BEARER_TOKEN_BEDROCK",
		"AWS_ACCESS_KEY_ID",
		"AWS_SECRET_ACCESS_KEY",
		"AWS_SESSION_TOKEN",
		"AWS_PROFILE",
		"AWS_DEFAULT_PROFILE",
		"AWS_REGION",
		"AWS_DEFAULT_REGION",
		"AWS_SHARED_CREDENTIALS_FILE",
		"AWS_CONFIG_FILE",
		"AWS_WEB_IDENTITY_TOKEN_FILE",
		"AWS_ROLE_ARN",
		"AWS_CONTAINER_CREDENTIALS_FULL_URI",
		// Google Cloud's Agent Platform credential and route selection.
		"CLAUDE_CODE_USE_VERTEX",
		"CLAUDE_CODE_SKIP_VERTEX_AUTH",
		"ANTHROPIC_VERTEX_BASE_URL",
		"ANTHROPIC_VERTEX_PROJECT_ID",
		"GOOGLE_APPLICATION_CREDENTIALS",
		"GOOGLE_CLOUD_PROJECT",
		"GCLOUD_PROJECT",
		"CLOUD_ML_REGION",
		"VERTEX_REGION_CLAUDE_SONNET_4_6",
		// Microsoft Foundry and Azure's default credential chain.
		"CLAUDE_CODE_USE_FOUNDRY",
		"CLAUDE_CODE_SKIP_FOUNDRY_AUTH",
		"ANTHROPIC_FOUNDRY_API_KEY",
		"ANTHROPIC_FOUNDRY_AUTH_TOKEN",
		"ANTHROPIC_FOUNDRY_BASE_URL",
		"ANTHROPIC_FOUNDRY_RESOURCE",
		"AZURE_CLIENT_ID",
		"AZURE_TENANT_ID",
		"AZURE_CLIENT_SECRET",
		"AZURE_CLIENT_CERTIFICATE_PATH",
		"AZURE_FEDERATED_TOKEN_FILE",
		"AZURE_AUTHORITY_HOST",
	}
	base := []string{"PATH=/usr/bin", "ANTHROPIC_MODEL=sonnet", "AWS_SDK_LOAD_CONFIG=1", "GOOGLE_CLOUD_QUOTA_PROJECT=billing"}
	for _, key := range conflicts {
		base = append(base, key+"=conflicting")
	}

	got, err := BuildClaudeLaunchEnvironment(base, "/accounts/work", "selected-token", nil)
	if err != nil {
		t.Fatal(err)
	}
	env := environmentMap(got)
	for _, key := range conflicts {
		if _, ok := env[key]; ok {
			t.Errorf("conflicting variable %s was preserved", key)
		}
	}
	for key, want := range map[string]string{
		"PATH":                       "/usr/bin",
		"ANTHROPIC_MODEL":            "sonnet",
		"AWS_SDK_LOAD_CONFIG":        "1",
		"GOOGLE_CLOUD_QUOTA_PROJECT": "billing",
	} {
		if env[key] != want {
			t.Errorf("unrelated %s = %q, want %q", key, env[key], want)
		}
	}
}

func TestBuildClaudeLaunchEnvironmentNeverIncludesTokenInErrors(t *testing.T) {
	token := "secret-token-must-not-appear"
	tests := []struct {
		name      string
		configDir string
		token     string
		metadata  map[string]string
	}{
		{name: "missing config", token: token},
		{name: "missing token", configDir: "/accounts/work"},
		{name: "invalid token", configDir: "/accounts/work", token: token + "\x00"},
		{name: "invalid metadata", configDir: "/accounts/work", token: token, metadata: map[string]string{"BAD=KEY": "value"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := BuildClaudeLaunchEnvironment(nil, test.configDir, test.token, test.metadata)
			if err == nil {
				t.Fatal("expected error")
			}
			if strings.Contains(err.Error(), token) {
				t.Fatalf("error exposed setup token: %q", err)
			}
		})
	}
}

func TestPrepareClaudeAccountHomeRemovesOnlyTopLevelOAuthAccount(t *testing.T) {
	homeRoot := t.TempDir()
	t.Setenv("HOME", filepath.Join(homeRoot, "native"))
	if err := os.MkdirAll(os.Getenv("HOME"), 0o700); err != nil {
		t.Fatal(err)
	}
	accountHome := filepath.Join(homeRoot, "accounts", "work")
	if err := os.MkdirAll(accountHome, 0o755); err != nil {
		t.Fatal(err)
	}
	files := map[string]string{
		".claude.json": `{ "theme": "dark", "oauthAccount": {"accountUuid":"wrong"}, "nested": {"oauthAccount":{"keep":true}} }`,
		".config.json": `{"oauthAccount":{"accountUuid":"also-wrong"},"feature":true,"count":3}`,
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(accountHome, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	originalInfo, err := os.Stat(filepath.Join(accountHome, ".claude.json"))
	if err != nil {
		t.Fatal(err)
	}

	if err := PrepareClaudeAccountHome(accountHome); err != nil {
		t.Fatal(err)
	}

	info, err := os.Stat(accountHome)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o700 {
		t.Errorf("account home mode = %o, want 700", got)
	}
	replacedInfo, err := os.Stat(filepath.Join(accountHome, ".claude.json"))
	if err != nil {
		t.Fatal(err)
	}
	if os.SameFile(originalInfo, replacedInfo) {
		t.Error(".claude.json was modified in place instead of atomically replaced")
	}
	for name := range files {
		path := filepath.Join(accountHome, name)
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var got map[string]json.RawMessage
		if err := json.Unmarshal(data, &got); err != nil {
			t.Fatalf("decode %s: %v", name, err)
		}
		if _, ok := got["oauthAccount"]; ok {
			t.Errorf("%s retained top-level oauthAccount: %s", name, data)
		}
		fileInfo, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if mode := fileInfo.Mode().Perm(); mode != 0o600 {
			t.Errorf("%s mode = %o, want 600", name, mode)
		}
	}
	claudeData, err := os.ReadFile(filepath.Join(accountHome, ".claude.json"))
	if err != nil {
		t.Fatal(err)
	}
	var claudeConfig struct {
		Theme  string `json:"theme"`
		Nested struct {
			OAuthAccount map[string]bool `json:"oauthAccount"`
		} `json:"nested"`
	}
	if err := json.Unmarshal(claudeData, &claudeConfig); err != nil {
		t.Fatal(err)
	}
	if claudeConfig.Theme != "dark" || !claudeConfig.Nested.OAuthAccount["keep"] {
		t.Fatalf("unrelated JSON changed: %s", claudeData)
	}
}

func TestPrepareClaudeAccountHomeFailsClosedBeforeAnyRewrite(t *testing.T) {
	homeRoot := t.TempDir()
	t.Setenv("HOME", filepath.Join(homeRoot, "native"))
	if err := os.MkdirAll(os.Getenv("HOME"), 0o700); err != nil {
		t.Fatal(err)
	}
	accountHome := filepath.Join(homeRoot, "accounts", "work")
	if err := os.MkdirAll(accountHome, 0o700); err != nil {
		t.Fatal(err)
	}
	validPath := filepath.Join(accountHome, ".claude.json")
	validData := []byte(`{"keep":true,"oauthAccount":{"accountUuid":"wrong"}}`)
	if err := os.WriteFile(validPath, validData, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(accountHome, ".config.json"), []byte(`{"oauthAccount":`), 0o600); err != nil {
		t.Fatal(err)
	}

	err := PrepareClaudeAccountHome(accountHome)
	if err == nil || !strings.Contains(err.Error(), "malformed") {
		t.Fatalf("error = %v, want malformed JSON error", err)
	}
	got, err := os.ReadFile(validPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(validData) {
		t.Fatalf("valid config changed before malformed peer failed: %q", got)
	}
}

func TestPrepareClaudeAccountHomeRejectsSymlinkAndNonRegularFiles(t *testing.T) {
	tests := []struct {
		name  string
		setup func(t *testing.T, path string)
	}{
		{
			name: "symlink",
			setup: func(t *testing.T, path string) {
				t.Helper()
				target := filepath.Join(filepath.Dir(path), "target.json")
				if err := os.WriteFile(target, []byte(`{"oauthAccount":{}}`), 0o600); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(target, path); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "directory",
			setup: func(t *testing.T, path string) {
				t.Helper()
				if err := os.Mkdir(path, 0o700); err != nil {
					t.Fatal(err)
				}
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			homeRoot := t.TempDir()
			t.Setenv("HOME", filepath.Join(homeRoot, "native"))
			if err := os.MkdirAll(os.Getenv("HOME"), 0o700); err != nil {
				t.Fatal(err)
			}
			accountHome := filepath.Join(homeRoot, "accounts", "work")
			if err := os.MkdirAll(accountHome, 0o700); err != nil {
				t.Fatal(err)
			}
			test.setup(t, filepath.Join(accountHome, ".claude.json"))

			err := PrepareClaudeAccountHome(accountHome)
			if err == nil || !strings.Contains(err.Error(), "regular file") {
				t.Fatalf("error = %v, want regular-file rejection", err)
			}
		})
	}
}

func TestPrepareClaudeAccountHomeNeverTouchesNativeHome(t *testing.T) {
	homeRoot := t.TempDir()
	nativeHome := filepath.Join(homeRoot, "native")
	t.Setenv("HOME", nativeHome)
	if err := os.MkdirAll(filepath.Join(nativeHome, ".claude"), 0o700); err != nil {
		t.Fatal(err)
	}
	nativeSentinel := filepath.Join(nativeHome, ".claude.json")
	nativeData := []byte(`{"oauthAccount":{"accountUuid":"native"},"sentinel":"byte-identical"}`)
	if err := os.WriteFile(nativeSentinel, nativeData, 0o600); err != nil {
		t.Fatal(err)
	}
	nativeHomeSentinel := filepath.Join(nativeHome, ".claude", ".config.json")
	nativeHomeData := []byte(`{"oauthAccount":{"accountUuid":"native-home"},"sentinel":"byte-identical"}`)
	if err := os.WriteFile(nativeHomeSentinel, nativeHomeData, 0o600); err != nil {
		t.Fatal(err)
	}
	accountHome := filepath.Join(homeRoot, "accounts", "work")
	if err := os.MkdirAll(accountHome, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(accountHome, ".claude.json"), []byte(`{"oauthAccount":{"accountUuid":"work"}}`), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := PrepareClaudeAccountHome(accountHome); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(nativeSentinel)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(nativeData) {
		t.Fatalf("native config changed: %q", got)
	}
	got, err = os.ReadFile(nativeHomeSentinel)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(nativeHomeData) {
		t.Fatalf("native home config changed: %q", got)
	}

	for _, forbidden := range []string{nativeHome, filepath.Join(nativeHome, ".claude")} {
		if err := PrepareClaudeAccountHome(forbidden); err == nil {
			t.Fatalf("native path %q was accepted", forbidden)
		}
	}
	got, err = os.ReadFile(nativeSentinel)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(nativeData) {
		t.Fatalf("native config changed after rejected path: %q", got)
	}
	got, err = os.ReadFile(nativeHomeSentinel)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(nativeHomeData) {
		t.Fatalf("native home config changed after rejected path: %q", got)
	}
}

func environmentMap(entries []string) map[string]string {
	result := make(map[string]string, len(entries))
	for _, entry := range entries {
		key, value, ok := strings.Cut(entry, "=")
		if ok {
			result[key] = value
		}
	}
	return result
}
