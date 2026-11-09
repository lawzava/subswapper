package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"os"
	"path/filepath"

	"github.com/lawzava/subswapper/internal/subswapper"
)

// This helper prints a standalone config snippet only. It neither installs the plugin nor
// activates a provider, writes OpenCode config, or creates proxy credentials.
func runSetupOpenCode(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("setup opencode", flag.ContinueOnError)
	config := fs.String("config", defaultConfigPath, "Subswapper config")
	plugin := fs.String("plugin", "", "absolute installed plugin directory")
	service := fs.String("service", "codex", "Codex service")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 || !filepath.IsAbs(*plugin) {
		return errors.New("usage: subswapper setup opencode -plugin <absolute directory> [-config path] [-service codex]")
	}
	cfg, err := subswapper.LoadConfig(*config)
	if err != nil {
		return errors.New("cannot load Subswapper config")
	}
	if _, err := subswapper.OpenCodeCodexRoute(*cfg, *service); err != nil {
		return err
	}
	configPath, err := filepath.Abs(subswapper.ExpandPath(*config))
	if err != nil {
		return errors.New("absolute config path required")
	}
	snippet := map[string]any{"$schema": "https://opencode.ai/config.json", "plugins": []any{map[string]any{"package": *plugin, "options": map[string]string{"config": configPath, "service": *service}}}}
	encoder := json.NewEncoder(out)
	encoder.SetIndent("", "  ")
	return encoder.Encode(snippet)
}

func doctorOpenCode(ctx context.Context, d *doctorReport, cfg subswapper.Config, plugin, service string) {
	var pkg struct {
		Name         string
		Version      string
		Dependencies map[string]string
	}
	data, err := os.ReadFile(filepath.Join(plugin, "package.json"))
	_, entryErr := os.Stat(filepath.Join(plugin, "index.ts"))
	_, helperErr := os.Stat(filepath.Join(plugin, "private-group.ts"))
	_, catalogErr := os.Stat(filepath.Join(plugin, "catalog.ts"))
	if !filepath.IsAbs(plugin) || err != nil || entryErr != nil || helperErr != nil || json.Unmarshal(data, &pkg) != nil || pkg.Name != "subswapper-opencode" || (pkg.Version != "0.1.0" && pkg.Version != "0.2.0") || (pkg.Version == "0.2.0" && catalogErr != nil) || pkg.Dependencies["@opencode/plugin"] != "2.2.6" {
		d.fail("opencode: plugin version missing or unsupported; install subswapper-opencode 0.2.0 (or 0.1.0) with SDK 2.2.6")
		return
	}
	d.ok("opencode: plugin version %s present (SDK 2.2.6); runtime loading not verified", pkg.Version)
	route, err := subswapper.OpenCodeCodexRoute(cfg, service)
	if err != nil {
		d.fail("opencode: %v", err)
		return
	}
	if pkg.Version == "0.1.0" {
		d.ok("opencode: exact Responses route %s/backend-api/codex/responses; HTTP only, summary compaction", route.Origin)
	} else {
		d.ok("opencode: backend route %s/backend-api/codex; HTTP/WebSocket bridge, native compaction; backend model/image support not verified", route.Origin)
	}
	placeholder, err := subswapper.LoadExistingCodexProxyPlaceholder(cfg, service)
	if err != nil {
		d.fail("opencode: %v; start the configured proxy to provision its placeholder", err)
		return
	}
	d.ok("opencode: safe placeholder")
	if err := subswapper.CheckOpenCodeProxy(ctx, route, placeholder); err != nil {
		d.fail("opencode: %v", err)
		return
	}
	d.ok("opencode: authenticated proxy health; selected account ready (cached usage, no refresh or inference)")
}
