package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"

	"github.com/lawzava/subswapper/internal/subswapper"
)

// hubBundleLimit bounds an imported bundle; a real one is well under 4 KiB.
const hubBundleLimit = 1 << 20

func runHub(args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		return errors.New("missing hub command: connect, export, or import")
	}
	action := args[0]
	fs := flag.NewFlagSet("hub "+action, flag.ContinueOnError)
	fs.SetOutput(stderr)
	configPath := fs.String("config", defaultConfigPath, "config file")
	switch action {
	case "export":
		serviceName := fs.String("service", "", "export only this service; default every service with hub_listen")
		host := fs.String("host", "", "host clients dial, e.g. a MagicDNS name; default the hub_listen IP")
		out := fs.String("out", "", "bundle file to create, or - for stdout")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		if *out == "" {
			return errors.New("missing -out; the bundle holds the hub credential, so name a private file or - for stdout")
		}
		cfg, err := subswapper.LoadConfig(*configPath)
		if err != nil {
			return err
		}
		bundle, err := subswapper.ExportHubBundle(*cfg, *serviceName, *host)
		if err != nil {
			return err
		}
		data, err := json.MarshalIndent(bundle, "", "  ")
		if err != nil {
			return err
		}
		data = append(data, '\n')
		if *out == "-" {
			_, err = stdout.Write(data)
			return err
		}
		return writeNewPrivateFile(*out, data)
	case "import":
		in := fs.String("in", "", "bundle file from `subswapper hub export`, or - for stdin")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		if *in == "" {
			return errors.New("missing -in")
		}
		reader := stdin
		if *in != "-" {
			file, err := os.Open(*in)
			if err != nil {
				return err
			}
			defer func() { _ = file.Close() }()
			reader = file
		}
		data, err := io.ReadAll(io.LimitReader(reader, hubBundleLimit+1))
		if err != nil {
			return err
		}
		if len(data) > hubBundleLimit {
			return errors.New("hub bundle is too large")
		}
		var bundle subswapper.HubBundle
		if err := json.Unmarshal(data, &bundle); err != nil {
			return errors.New("hub bundle is not valid JSON")
		}
		return importHubBundle(*configPath, bundle, stdout)
	case "connect":
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		if fs.NArg() != 1 {
			return errors.New("usage: subswapper hub connect [-config path] <hub address, e.g. 100.67.68.117>")
		}
		bundle, err := subswapper.FetchHubBundle(context.Background(), fs.Arg(0))
		if err != nil {
			return err
		}
		return importHubBundle(*configPath, bundle, stdout)
	default:
		return fmt.Errorf("unknown hub command %q", action)
	}
}

func importHubBundle(configPath string, bundle subswapper.HubBundle, stdout io.Writer) error {
	names, err := subswapper.ImportHubBundle(configPath, bundle)
	if err != nil {
		return err
	}
	for _, name := range names {
		for _, service := range bundle.Services {
			if service.Name != name {
				continue
			}
			if _, err := fmt.Fprintf(stdout, "%s now uses the hub at %s\n", name, service.HubURL); err != nil {
				return err
			}
		}
	}
	return nil
}

// writeNewPrivateFile refuses to replace an existing file, so an export
// cannot clobber another bundle or config by mistake.
func writeNewPrivateFile(path string, data []byte) error {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err := file.Write(data); err != nil {
		_ = file.Close()
		_ = os.Remove(path)
		return err
	}
	return file.Close()
}

// applyHubStatus fills each hub client service in results with the rows its
// hub reports, asking each hub host once, and returns one line per service
// saying where its accounts live.
func applyHubStatus(cfg subswapper.Config, results []subswapper.ServiceStatus) []string {
	type hubGroup struct {
		url, credential string
		services        []subswapper.ServiceConfig
	}
	var groups []*hubGroup
	byHost := map[string]*hubGroup{}
	var notes []string
	resultIndex := map[string]int{}
	for index, result := range results {
		resultIndex[result.Service.Name] = index
	}
	setNote := func(name, note string) {
		if index, ok := resultIndex[name]; ok {
			results[index].Note = note
		}
	}
	for _, service := range cfg.Services {
		if !service.HubClient() || service.Disabled {
			continue
		}
		var credential string
		var err error
		if isClaudeServiceConfig(service) {
			credential, err = subswapper.LoadHubClaudeSecret(cfg, service.Name)
		} else {
			var placeholder subswapper.CodexProxyPlaceholder
			placeholder, err = subswapper.LoadHubCodexPlaceholder(cfg, service.Name)
			credential = placeholder.Token
		}
		if err != nil {
			setNote(service.Name, "no hub credential; run hub connect")
			continue
		}
		host := service.HubURL
		if parsed, parseErr := url.Parse(service.HubURL); parseErr == nil {
			host = parsed.Scheme + "://" + parsed.Hostname()
		}
		group := byHost[host]
		if group == nil {
			group = &hubGroup{url: service.HubURL, credential: credential}
			byHost[host] = group
			groups = append(groups, group)
		}
		group.services = append(group.services, service)
	}
	for _, group := range groups {
		hubResults, err := subswapper.FetchHubStatus(context.Background(), group.url, group.credential)
		for _, service := range group.services {
			if err != nil {
				setNote(service.Name, "hub at "+service.HubURL+" unavailable")
				notes = append(notes, fmt.Sprintf("%s: hub at %s unavailable: %v", service.Name, service.HubURL, err))
				continue
			}
			found := false
			for _, hubResult := range hubResults {
				if hubResult.Service.Name != service.Name {
					continue
				}
				found = true
				if index, ok := resultIndex[service.Name]; ok {
					results[index].Accounts = hubResult.Accounts
					results[index].Note = hubResult.Note
				}
			}
			if !found {
				setNote(service.Name, "hub has no "+service.Name+" service")
				continue
			}
			notes = append(notes, fmt.Sprintf("%s: accounts on the hub at %s", service.Name, service.HubURL))
		}
	}
	return notes
}

func printHubListen(stdout io.Writer, service subswapper.ServiceConfig) error {
	if service.HubListen == "" {
		return nil
	}
	_, err := fmt.Fprintf(stdout, "subswapper %s hub listening on %s\n", service.Name, service.HubListen)
	return err
}

// runHubClientHome handles `home` commands for a service whose accounts live
// on another machine. Only launches and the Codex placeholder make sense here.
func runHubClientHome(
	cfg subswapper.Config,
	configPath string,
	service subswapper.ServiceConfig,
	action string,
	account string,
	args []string,
	stdin io.Reader,
	stdout io.Writer,
	stderr io.Writer,
) error {
	if account != "" {
		return fmt.Errorf("service %q uses the hub at %s, which selects the account; -account is not available here", service.Name, service.HubURL)
	}
	switch action {
	case "run":
		if len(args) == 0 {
			args = []string{providerBinary(service)}
		}
		return runThroughHub(cfg, configPath, service, args[0], args[1:], stdin, stdout, stderr)
	case "proxy-auth":
		if isClaudeServiceConfig(service) {
			return fmt.Errorf("service %q is not a Codex service", service.Name)
		}
		placeholder, err := subswapper.LoadHubCodexPlaceholder(cfg, service.Name)
		if err != nil {
			return err
		}
		return installCodexPlaceholder(subswapper.RuntimeHome(cfg, service, ""), placeholder, stdout)
	case "path":
		_, err := fmt.Fprintln(stdout, subswapper.RuntimeHome(cfg, service, ""))
		return err
	default:
		return fmt.Errorf("service %q uses the hub at %s; run `home %s` on the hub", service.Name, service.HubURL, action)
	}
}

// runThroughHub launches a provider against a loopback relay to the hub. It
// never falls back to a direct launch: this machine has no account tokens,
// and the hub's network egress is part of what the user chose.
func runThroughHub(
	cfg subswapper.Config,
	configPath string,
	service subswapper.ServiceConfig,
	command string,
	args []string,
	stdin io.Reader,
	stdout io.Writer,
	stderr io.Writer,
) error {
	unreachable := fmt.Errorf("subswapper hub at %s is unreachable or rejected this machine's credential; refusing a direct launch", service.HubURL)
	runtimeHome := subswapper.RuntimeHome(cfg, service, "")
	launchHome := runtimeHome
	if service.UsesNativeRuntimeHome() {
		launchHome = ""
	}
	metadata := map[string]string{
		"SUBSWAPPER_CONFIG_PATH": configPath,
		"SUBSWAPPER_SERVICE":     service.Name,
		"SUBSWAPPER_PROXY":       "1",
	}

	credential := ""
	var placeholder subswapper.CodexProxyPlaceholder
	var err error
	if isClaudeServiceConfig(service) {
		credential, err = subswapper.LoadHubClaudeSecret(cfg, service.Name)
	} else {
		placeholder, err = subswapper.LoadHubCodexPlaceholder(cfg, service.Name)
		credential = placeholder.Token
	}
	if err != nil {
		return err
	}
	// Prefer the machine's fixed relay; otherwise this launch gets its own.
	// Either way a passing health check proves the hub accepts the credential.
	listen := service.ProxyListen
	if listen == "" || !subswapper.HubRelayReachable(listen, credential) {
		var stop func()
		listen, stop, err = subswapper.StartHubRelay(service.HubURL)
		if err != nil {
			return err
		}
		defer stop()
		if !subswapper.HubRelayReachable(listen, credential) {
			return unreachable
		}
	}

	if !isClaudeServiceConfig(service) {
		if err := subswapper.EnsureCodexProxyAuth(runtimeHome, placeholder); err != nil {
			if errors.Is(err, subswapper.ErrCodexRuntimeAuthIsReal) {
				return fmt.Errorf("%s/auth.json holds a real ChatGPT login; move it aside with `subswapper home proxy-auth -service %s`", runtimeHome, service.Name)
			}
			return err
		}
		commandArgs := append([]string(nil), args...)
		if isCodexExecutable(command) {
			commandArgs = append(subswapper.CodexProxyLaunchArgs(listen), commandArgs...)
		}
		cmd := exec.Command(command, commandArgs...)
		cmd.Env = subswapper.BuildCodexProxyLaunchEnvironment(os.Environ(), launchHome, metadata)
		cmd.Stdin = stdin
		cmd.Stdout = stdout
		cmd.Stderr = stderr
		return cmd.Run()
	}

	secret := credential
	if launchHome != "" {
		if err := subswapper.PrepareClaudeSharedRuntimeHome(launchHome); err != nil {
			return err
		}
	}
	environment, err := subswapper.BuildClaudeProxyLaunchEnvironment(os.Environ(), launchHome, secret, listen, metadata, service.ProxyEnvScrub)
	if err != nil {
		return errors.New("hub Claude launch environment is unusable")
	}
	if isClaudeExecutable(command) {
		if err := checkClaudeAuthentication(command, environment); err != nil {
			return err
		}
	}
	return runClaudeProcess(command, args, environment, secret, stdin, stdout, stderr)
}
