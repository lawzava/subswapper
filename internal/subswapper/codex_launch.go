package subswapper

import (
	"os"
	"path/filepath"
	"strings"
)

// NativeCodexHome is the directory Codex uses when Subswapper does not
// override CODEX_HOME.
func NativeCodexHome() string {
	if dir := os.Getenv("CODEX_HOME"); dir != "" {
		return ExpandPath(dir)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join("~", ".codex")
	}
	return filepath.Join(home, ".codex")
}

// CodexProxyLaunchArgs are the global overrides that route one Codex process
// through the proxy. They go before the subcommand. A custom provider is
// required because Codex refuses overrides of the built-in "openai" one, and
// requires_openai_auth keeps the ChatGPT identity headers on every request.
// WebSockets stay off so every turn is a replayable HTTP request.
func CodexProxyLaunchArgs(listen string) []string {
	base := ClaudeProxyBaseURL(listen)
	provider := "model_providers." + CodexProxyProviderID + "."
	return []string{
		"-c", "chatgpt_base_url=" + base + "/backend-api/",
		"-c", "model_provider=" + CodexProxyProviderID,
		"-c", provider + "name=OpenAI",
		"-c", provider + "base_url=" + base + "/backend-api/codex",
		"-c", provider + "wire_api=responses",
		"-c", provider + "requires_openai_auth=true",
		"-c", provider + "supports_websockets=false",
		"-c", provider + "supports_standalone_web_search=true",
	}
}

// BuildCodexProxyLaunchEnvironment sets the runtime home and the routing
// metadata. An empty runtimeHome keeps Codex's own default home. CODEX_API_KEY
// is dropped because it would switch Codex to API-key auth and bypass the
// placeholder identity.
func BuildCodexProxyLaunchEnvironment(base []string, runtimeHome string, metadata map[string]string) []string {
	environment := make([]string, 0, len(base)+len(metadata)+1)
	for _, entry := range base {
		key, value, _ := strings.Cut(entry, "=")
		if key == "CODEX_API_KEY" || strings.HasPrefix(key, "SUBSWAPPER_") {
			continue
		}
		// An empty CODEX_HOME would not mean the default home to Codex.
		if key == "CODEX_HOME" && (runtimeHome != "" || value == "") {
			continue
		}
		environment = append(environment, entry)
	}
	if runtimeHome != "" {
		environment = append(environment, "CODEX_HOME="+runtimeHome)
	}
	for key, value := range metadata {
		environment = append(environment, key+"="+value)
	}
	return environment
}
