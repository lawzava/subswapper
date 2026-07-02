package subswapper

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"testing"
)

func TestImportClaudeSwap(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, "claude-swap")
	if err := os.MkdirAll(filepath.Join(root, "configs"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "credentials"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "cache"), 0o700); err != nil {
		t.Fatal(err)
	}
	sequence := `{
		"activeAccountNumber": 2,
		"sequence": [1, 2],
		"accounts": {
			"1": {"email": "one@example.com", "added": "2026-07-01T20:00:00Z"},
			"2": {"email": "two@example.com", "added": "2026-07-01T21:00:00Z"}
		}
	}`
	if err := os.WriteFile(filepath.Join(root, "sequence.json"), []byte(sequence), 0o600); err != nil {
		t.Fatal(err)
	}
	writeClaudeSwapFixtureAccount(t, root, "1", "one@example.com", `{"claudeAiOauth":{"accessToken":"one"}}`, `{"account":"one"}`)
	writeClaudeSwapFixtureAccount(t, root, "2", "two@example.com", `{"claudeAiOauth":{"accessToken":"two"}}`, `{"account":"two"}`)
	usage := `{"data": {
		"1": {"five_hour": {"pct": 10}, "seven_day": {"pct": 20}},
		"2": {"five_hour": {"pct": 30}, "seven_day": {"pct": 40}}
	}}`
	if err := os.WriteFile(filepath.Join(root, "cache", "usage.json"), []byte(usage), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg := testClaudeImportConfig(dir)
	// The live credentials belong to slot 2, so the import may adopt it as active.
	if err := os.WriteFile(filepath.Join(dir, "live-credentials.json"), []byte(`{"claudeAiOauth":{"accessToken":"two"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	result, err := ImportClaudeSwap(cfg, root)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Imported) != 2 {
		t.Fatalf("expected 2 imported accounts, got %d", len(result.Imported))
	}
	if result.Active != "cswap-2" {
		t.Fatalf("expected active cswap-2, got %q", result.Active)
	}
	assertFileContent(t, filepath.Join(AccountDir(cfg, "claude", "cswap-1"), "credentials.json"), `{"claudeAiOauth":{"accessToken":"one"}}`)
	assertFileContent(t, filepath.Join(AccountDir(cfg, "claude", "cswap-2"), "claude.json"), `{"account":"two"}`)

	state, err := LoadState(cfg.StatePath)
	if err != nil {
		t.Fatal(err)
	}
	account := state.Service("claude").Accounts["cswap-2"]
	if account.Email != "two@example.com" || account.Provider != "claude-swap" || account.Slot != "2" {
		t.Fatalf("unexpected imported account %#v", account)
	}
	ratio, ok := account.Usage.Weekly.Ratio()
	if !ok || ratio != 0.4 {
		t.Fatalf("expected cached weekly ratio .4, got %v %v", ratio, ok)
	}
}

func testClaudeImportConfig(dir string) Config {
	cfg := Config{
		BackupRoot: filepath.Join(dir, "backups"),
		StatePath:  filepath.Join(dir, "state.json"),
		Services: []ServiceConfig{
			{Name: "claude", Kind: "claude", Files: []ManagedFile{
				requiredFile(filepath.Join(dir, "live-credentials.json"), "credentials.json"),
				optionalFile(filepath.Join(dir, "live-claude.json"), "claude.json"),
			}},
		},
	}
	cfg.ApplyDefaults()
	return cfg
}

func writeClaudeSwapFixtureAccount(t *testing.T, root, number, email, credentials, config string) {
	t.Helper()
	encoded := base64.StdEncoding.EncodeToString([]byte(credentials))
	credentialsPath := filepath.Join(root, "credentials", ".creds-"+number+"-"+email+".enc")
	if err := os.WriteFile(credentialsPath, []byte(encoded), 0o600); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(root, "configs", ".claude-config-"+number+"-"+email+".json")
	if err := os.WriteFile(configPath, []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
}
