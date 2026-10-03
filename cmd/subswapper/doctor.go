package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/lawzava/subswapper/internal/subswapper"
)

// doctorProbeStaleAfter is how long without a usage probe before the
// monitor looks stopped. It exceeds the default 5m interval with margin.
const doctorProbeStaleAfter = 15 * time.Minute

type doctorReport struct {
	out      io.Writer
	failures int
}

func (d *doctorReport) line(level, format string, args ...any) {
	_, _ = fmt.Fprintf(d.out, "%-6s"+format+"\n", append([]any{level}, args...)...)
}

func (d *doctorReport) ok(format string, args ...any)   { d.line("ok", format, args...) }
func (d *doctorReport) warn(format string, args ...any) { d.line("warn", format, args...) }
func (d *doctorReport) fail(format string, args ...any) {
	d.failures++
	d.line("FAIL", format, args...)
}

func runDoctor(args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("doctor", flag.ContinueOnError)
	configPath := fs.String("config", defaultConfigPath, "config file")
	if err := fs.Parse(args); err != nil {
		return err
	}
	d := &doctorReport{out: stdout}
	cfg, err := subswapper.LoadConfig(*configPath)
	if err != nil {
		d.fail("config %s: %v; create one with `subswapper setup hub` or `subswapper setup client <hub>`", *configPath, err)
		return errors.New("doctor found 1 problem")
	}
	d.ok("config %s (subswapper %s)", *configPath, subswapper.Version())
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	state, err := subswapper.LoadState(cfg.StatePath)
	if err != nil {
		d.fail("state %s: %v", cfg.StatePath, err)
		state = subswapper.NewState()
	}
	for _, service := range cfg.Services {
		if service.Disabled || !service.UsesAccountHomes() {
			continue
		}
		if service.HubClient() {
			doctorHubClient(ctx, d, *cfg, service)
		} else {
			doctorLocalService(d, *cfg, state, service)
		}
	}
	doctorPaseo(ctx, d)
	if d.failures > 0 {
		return fmt.Errorf("doctor found %d problem(s)", d.failures)
	}
	return nil
}

func doctorHubClient(ctx context.Context, d *doctorReport, cfg subswapper.Config, service subswapper.ServiceConfig) {
	credential, err := hubCredential(cfg, service)
	if err != nil {
		d.fail("%s: no hub credential; run `subswapper hub connect <hub>`", service.Name)
		return
	}
	health, err := subswapper.HubHealth(ctx, service.HubURL, credential)
	if err != nil {
		d.fail("%s: %v", service.Name, err)
	} else {
		d.ok("%s: hub at %s accepts this machine (hub %s)", service.Name, service.HubURL, health.Version)
		if health.Version != subswapper.Version() {
			d.warn("%s: hub runs subswapper %s and this machine %s; install the same release on both", service.Name, health.Version, subswapper.Version())
		}
	}
	if service.ProxyListen != "" {
		if subswapper.HubRelayReachable(service.ProxyListen, credential) {
			d.ok("%s: relay at %s reaches the hub", service.Name, service.ProxyListen)
		} else {
			d.fail("%s: relay at %s does not answer; start `subswapper proxy` (or its service)", service.Name, service.ProxyListen)
		}
	}
	doctorCodexNativeLogin(d, cfg, service)
}

func doctorLocalService(d *doctorReport, cfg subswapper.Config, state *subswapper.State, service subswapper.ServiceConfig) {
	accounts := state.Service(service.Name).Accounts
	if len(accounts) == 0 {
		d.warn("%s: no accounts; add one with `subswapper add %s <name>`", service.Name, service.Name)
	} else {
		names := make([]string, 0, len(accounts))
		var newestProbe time.Time
		for name, account := range accounts {
			names = append(names, name)
			if account.LastProbeStartedAt.After(newestProbe) {
				newestProbe = account.LastProbeStartedAt
			}
		}
		sort.Strings(names)
		for _, name := range names {
			if problem := accounts[name].CredentialsError; problem != "" {
				d.warn("%s/%s: %s; sign in again with `subswapper add %s %s`", service.Name, name, problem, service.Name, name)
			}
		}
		d.ok("%s: %d account(s): %s", service.Name, len(names), strings.Join(names, ", "))
		if time.Since(newestProbe) > doctorProbeStaleAfter {
			d.warn("%s: no usage probe since %s; is `subswapper monitor` running?", service.Name, formatProbeTime(newestProbe))
		}
		if strings.EqualFold(service.Kind, "codex") {
			if _, err := exec.LookPath("codex"); err != nil {
				d.fail("%s: codex is not on PATH, so the monitor cannot refresh Codex logins", service.Name)
			}
		}
	}
	if service.ProxyEnabled() {
		var reachable bool
		var credential string
		if service.ClaudeProxyEnabled() {
			secret, err := subswapper.LoadOrCreateClaudeProxySecret(cfg, service.Name)
			credential, reachable = secret, err == nil && subswapper.ClaudeProxyReachable(service, secret)
		} else {
			placeholder, err := subswapper.LoadOrCreateCodexProxyPlaceholder(cfg, service.Name)
			credential, reachable = placeholder.Token, err == nil && subswapper.CodexProxyReachable(service, placeholder)
		}
		if reachable {
			d.ok("%s: proxy at %s answers", service.Name, service.ProxyListen)
		} else {
			d.fail("%s: proxy at %s does not answer; start `subswapper proxy` (or its service)", service.Name, service.ProxyListen)
		}
		if service.HubListen != "" {
			if subswapper.HubRelayReachable(service.HubListen, credential) {
				d.ok("%s: hub listener at %s answers", service.Name, service.HubListen)
			} else {
				d.fail("%s: hub listener at %s does not answer", service.Name, service.HubListen)
			}
		}
	}
	if service.CodexProxyEnabled() {
		doctorCodexNativeLogin(d, cfg, service)
	}
}

func formatProbeTime(at time.Time) string {
	if at.IsZero() {
		return "ever"
	}
	return at.Local().Format("Jan02 15:04")
}

// doctorCodexNativeLogin catches the most common breakage: a plain `codex
// login` overwrote the placeholder that proxied launches rely on.
func doctorCodexNativeLogin(d *doctorReport, cfg subswapper.Config, service subswapper.ServiceConfig) {
	if !strings.EqualFold(service.Kind, "codex") || service.SharedRuntimeHome == "" {
		return
	}
	path := filepath.Join(subswapper.RuntimeHome(cfg, service, ""), "auth.json")
	data, err := os.ReadFile(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		d.ok("%s: %s is installed on the next launch", service.Name, path)
	case err != nil:
		d.fail("%s: read %s: %v", service.Name, path, err)
	case !subswapper.IsCodexProxyAuthFile(data):
		d.fail("%s: %s holds a real ChatGPT login, so proxied launches fail; move it aside with `subswapper home proxy-auth -service %s`", service.Name, path, service.Name)
	default:
		d.ok("%s: %s holds the proxy placeholder", service.Name, path)
	}
}

// doctorPaseo checks every Paseo provider that launches subswapper.
func doctorPaseo(ctx context.Context, d *doctorReport) {
	home, err := os.UserHomeDir()
	if err != nil {
		return
	}
	data, err := os.ReadFile(filepath.Join(home, ".paseo", "config.json"))
	if err != nil {
		return
	}
	var config any
	if json.Unmarshal(data, &config) != nil {
		d.warn("paseo: ~/.paseo/config.json is not valid JSON")
		return
	}
	providers := map[string]string{}
	collectPaseoProviders(config, "", providers)
	names := make([]string, 0, len(providers))
	for name := range providers {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		binary := providers[name]
		if _, err := os.Stat(binary); err != nil {
			d.fail("paseo: provider %s runs %s, which does not exist", name, binary)
			continue
		}
		probeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		output, err := exec.CommandContext(probeCtx, binary, "version").Output()
		cancel()
		fields := strings.Fields(string(output))
		switch {
		case err != nil || len(fields) < 2:
			d.warn("paseo: provider %s: `%s version` failed", name, binary)
		case fields[1] != subswapper.Version():
			d.warn("paseo: provider %s runs subswapper %s from %s; this is %s", name, fields[1], binary, subswapper.Version())
		default:
			d.ok("paseo: provider %s runs this subswapper (%s)", name, binary)
		}
	}
}

// collectPaseoProviders finds objects whose "command" array runs subswapper
// and records the binary under the key that holds the object.
func collectPaseoProviders(node any, key string, found map[string]string) {
	switch value := node.(type) {
	case map[string]any:
		if command, ok := value["command"].([]any); ok && len(command) > 0 {
			if binary, ok := command[0].(string); ok && strings.Contains(binary, "subswapper") {
				found[key] = binary
			}
		}
		for child, next := range value {
			collectPaseoProviders(next, child, found)
		}
	case []any:
		for _, next := range value {
			collectPaseoProviders(next, key, found)
		}
	}
}
