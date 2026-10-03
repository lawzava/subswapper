package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/lawzava/subswapper/internal/subswapper"
)

func runSetup(args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		return errors.New("usage: subswapper setup local | setup hub [-enroll] | setup client <hub address>")
	}
	switch args[0] {
	case "local":
		return runSetupHub(args[1:], false, stdout, stderr)
	case "hub":
		return runSetupHub(args[1:], true, stdout, stderr)
	case "client":
		return runSetupClient(args[1:], stdout, stderr)
	default:
		return errors.New("usage: subswapper setup local | setup hub [-enroll] | setup client <hub address>")
	}
}

// runSetupHub configures this machine's proxies and services, and with hub
// set also serves them to other machines on its Tailscale address.
func runSetupHub(args []string, hub bool, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("setup", flag.ContinueOnError)
	fs.SetOutput(stderr)
	configPath := fs.String("config", defaultConfigPath, "config file")
	tailscaleIP := fs.String("tailscale-ip", "", "this machine's Tailscale IPv4; default from `tailscale ip -4`")
	enroll := fs.Bool("enroll", false, "let any tailnet member join with `subswapper setup client`")
	noService := fs.Bool("no-service", false, "write the config only; do not install systemd user services")
	if err := fs.Parse(args); err != nil {
		return err
	}
	ip := *tailscaleIP
	if hub && ip == "" {
		output, err := exec.Command("tailscale", "ip", "-4").Output()
		if err != nil {
			return errors.New("cannot read this machine's Tailscale IP; pass -tailscale-ip")
		}
		ip = strings.TrimSpace(strings.SplitN(string(output), "\n", 2)[0])
	}
	if err := subswapper.ConfigureHub(*configPath, ip, *enroll); err != nil {
		return err
	}
	if !hub {
		ip = ""
	}
	where := "for this machine"
	if hub {
		where = "as a hub on " + ip
	}
	if _, err := fmt.Fprintf(stdout, "configured %s %s\n", *configPath, where); err != nil {
		return err
	}
	if !*noService {
		if err := installHubServices(stdout); err != nil {
			return err
		}
	}
	next := "next:\n  subswapper add claude <name>     # once per Claude subscription\n  subswapper add codex <name>      # once per ChatGPT subscription\n"
	if hub {
		join := "subswapper setup client " + ip
		if !*enroll {
			join = "subswapper hub export -out hub.json here, then `subswapper hub import -in hub.json` there (or rerun setup with -enroll and use `" + join + "`)"
		}
		next += "  on other machines: " + join + "\n"
	}
	next += "  subswapper doctor                # check everything\n"
	_, err := io.WriteString(stdout, next)
	return err
}

// hubUnits renders the two user services a hub runs: the proxies, and the
// monitor that probes usage and refreshes Codex logins.
func hubUnits(executable, path string) map[string]string {
	unit := func(description, command string) string {
		return fmt.Sprintf(`[Unit]
Description=%s
After=network-online.target

[Service]
Type=simple
ExecStart=%s %s
Restart=always
RestartSec=5
Environment=PATH=%s

[Install]
WantedBy=default.target
`, description, executable, command, path)
	}
	return map[string]string{
		"subswapper-hub.service": unit("subswapper auth proxies", "proxy"),
		"subswapper.service":     unit("subswapper usage monitor", "monitor -interval 5m"),
	}
}

func installHubServices(stdout io.Writer) error {
	if runtime.GOOS != "linux" {
		_, err := fmt.Fprintln(stdout, "services: run `subswapper proxy` and `subswapper monitor` at login (systemd units are Linux only)")
		return err
	}
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	dirs := []string{filepath.Dir(executable)}
	for _, tool := range []string{"codex", "claude"} {
		if found, err := exec.LookPath(tool); err == nil {
			dirs = append(dirs, filepath.Dir(found))
		}
	}
	dirs = append(dirs, "/usr/local/bin", "/usr/bin", "/bin")
	seen := map[string]bool{}
	var path []string
	for _, dir := range dirs {
		if !seen[dir] {
			seen[dir] = true
			path = append(path, dir)
		}
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	unitDir := filepath.Join(home, ".config", "systemd", "user")
	if err := os.MkdirAll(unitDir, 0o755); err != nil {
		return err
	}
	units := hubUnits(executable, strings.Join(path, ":"))
	for name, content := range units {
		if err := os.WriteFile(filepath.Join(unitDir, name), []byte(content), 0o644); err != nil {
			return err
		}
	}
	for _, command := range [][]string{
		{"daemon-reload"},
		{"enable", "subswapper-hub.service", "subswapper.service"},
		{"restart", "subswapper-hub.service", "subswapper.service"},
	} {
		if output, err := exec.Command("systemctl", append([]string{"--user"}, command...)...).CombinedOutput(); err != nil {
			return fmt.Errorf("systemctl --user %s: %v: %s", strings.Join(command, " "), err, strings.TrimSpace(string(output)))
		}
	}
	if _, err := fmt.Fprintln(stdout, "services: subswapper-hub.service and subswapper.service are enabled and running"); err != nil {
		return err
	}
	if output, err := exec.Command("loginctl", "show-user", os.Getenv("USER"), "-p", "Linger").Output(); err == nil && strings.TrimSpace(string(output)) == "Linger=no" {
		_, err = fmt.Fprintf(stdout, "warning: run `sudo loginctl enable-linger %s` so the hub keeps running without a login session\n", os.Getenv("USER"))
		return err
	}
	return nil
}

func runSetupClient(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("setup client", flag.ContinueOnError)
	fs.SetOutput(stderr)
	configPath := fs.String("config", defaultConfigPath, "config file")
	positional, err := parseInterleaved(fs, args)
	if err != nil {
		return err
	}
	if len(positional) != 1 {
		return errors.New("usage: subswapper setup client <hub address, e.g. 100.67.68.117>")
	}
	bundle, err := subswapper.FetchHubBundle(context.Background(), positional[0])
	if err != nil {
		return err
	}
	if err := importHubBundle(*configPath, bundle, stdout); err != nil {
		return err
	}
	cfg, err := subswapper.LoadConfig(*configPath)
	if err != nil {
		return err
	}
	for _, service := range cfg.Services {
		if !service.HubClient() || isClaudeServiceConfig(service) {
			continue
		}
		// A real login here would break proxied launches; keep it as a backup.
		placeholder, err := subswapper.LoadHubCodexPlaceholder(*cfg, service.Name)
		if err != nil {
			return err
		}
		if err := installCodexPlaceholder(subswapper.RuntimeHome(*cfg, service, ""), placeholder, stdout); err != nil {
			return err
		}
	}
	if _, err := fmt.Fprint(stdout, `launch with:
  subswapper claude [args]   # or: alias claude='subswapper claude'
  subswapper codex [args]    # or: alias codex='subswapper codex'
checks:
`); err != nil {
		return err
	}
	return runDoctor([]string{"-config", *configPath}, stdout)
}
