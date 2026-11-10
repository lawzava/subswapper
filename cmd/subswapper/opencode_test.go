package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
)

func TestSetupOpenCodePrintsSnippetWithoutCreatingCredentials(t *testing.T) {
	dir := t.TempDir()
	config := writeCodexProxyHomeConfig(t, dir, "127.0.0.1:17879")
	plugin := filepath.Join(dir, "plugin")
	before, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var out, stderr bytes.Buffer
	if err := run([]string{"setup", "opencode", "-config", config, "-plugin", plugin}, &out, &stderr); err != nil {
		t.Fatal(err)
	}
	var snippet struct {
		Plugins []struct {
			Package string
			Options map[string]string
		} `json:"plugins"`
		LegacyPlugin json.RawMessage `json:"plugin"`
	}
	if err := json.Unmarshal(out.Bytes(), &snippet); err != nil {
		t.Fatal(err)
	}
	if len(snippet.Plugins) != 1 || snippet.Plugins[0].Package != plugin || snippet.Plugins[0].Options["config"] != config || snippet.Plugins[0].Options["service"] != "codex" || snippet.LegacyPlugin != nil {
		t.Fatalf("snippet: %s", out.String())
	}
	if strings.Contains(out.String(), "token") {
		t.Fatal("snippet includes credentials")
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != len(before) {
		t.Fatal("read-only helper created data")
	}
}

func TestDoctorOpenCodeIsReadOnlyAndChecksPackagePlaceholderHealthReadiness(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("doctor placeholder validation requires POSIX ownership and permission checks")
	}
	// macOS temp roots can contain /var -> /private/var; production rejects
	// symlink ancestors, so create synthetic fixtures at the canonical root.
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	plugin := filepath.Join(dir, "plugin")
	if err := os.Mkdir(plugin, 0o700); err != nil {
		t.Fatal(err)
	}
	for name, content := range map[string]string{"package.json": `{"name":"subswapper-opencode","version":"0.1.0","dependencies":{"@opencode/plugin":"2.2.6"}}`, "index.ts": "export default {}", "private-group.ts": "export {}"} {
		if err := os.WriteFile(filepath.Join(plugin, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	var healthy, ready atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer synthetic.fixture.signature" {
			t.Error("missing placeholder")
			w.WriteHeader(401)
			return
		}
		if !healthy.Load() {
			w.WriteHeader(401)
			return
		}
		if strings.HasSuffix(r.URL.Path, "/readiness") {
			_, _ = w.Write([]byte(`{"service":"codex","selected_ready":` + map[bool]string{true: "true", false: "false"}[ready.Load()] + `}`))
			return
		}
		_, _ = w.Write([]byte(`{"service":"codex","proxy":"subswapper","version":"test","capabilities":["codex-readiness-v1"]}`))
	}))
	defer server.Close()
	config := writeCodexProxyHomeConfig(t, dir, strings.TrimPrefix(server.URL, "http://"))
	// Read this synthetic config only to locate the synthetic data root.
	data, err := os.ReadFile(config)
	if err != nil {
		t.Fatal(err)
	}
	var cfg struct {
		StatePath string `json:"state_path"`
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(filepath.Dir(cfg.StatePath), "tokens", "codex", "proxy-placeholder.json")
	check := func(want string, pass bool) {
		t.Helper()
		var out, stderr bytes.Buffer
		err := run([]string{"doctor", "-config", config, "-opencode-plugin", plugin}, &out, &stderr)
		if (err == nil) != pass || !strings.Contains(out.String(), want) {
			t.Fatalf("doctor err=%v output=%s", err, out.String())
		}
		if strings.Contains(out.String(), "synthetic.fixture.signature") {
			t.Fatal("doctor leaked secret")
		}
	}
	check("placeholder", false)
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("doctor created placeholder")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`{"token":"synthetic.fixture.signature","account_id":"subswapper-proxy"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	check("authenticated proxy health", false)
	healthy.Store(true)
	check("selected account", false)
	ready.Store(true)
	check("selected account ready", true)
	check("HTTP only, summary compaction", true)
	if err := os.WriteFile(filepath.Join(plugin, "package.json"), []byte(`{"name":"subswapper-opencode","version":"0.2.0","dependencies":{"@opencode/plugin":"2.2.6"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(plugin, "catalog.ts"), []byte("export {}"), 0o600); err != nil {
		t.Fatal(err)
	}
	check("plugin version 0.2.0 present", true)
	check("HTTP/WebSocket bridge, native compaction", true)
	if err := os.Remove(filepath.Join(plugin, "catalog.ts")); err != nil {
		t.Fatal(err)
	}
	check("plugin version", false)
	if err := os.WriteFile(filepath.Join(plugin, "catalog.ts"), []byte("export {}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(plugin, "private-group.ts")); err != nil {
		t.Fatal(err)
	}
	check("plugin version", false)
	if err := os.WriteFile(filepath.Join(plugin, "private-group.ts"), []byte("export {}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	check("placeholder", false)
	if err := os.WriteFile(filepath.Join(plugin, "package.json"), []byte(`{"version":"0.0.0"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	check("plugin version", false)
}
