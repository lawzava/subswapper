package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"runtime/debug"
	"strings"
	"syscall"
	"time"

	"github.com/lawzava/subswapper/internal/subswapper"
)

var defaultConfigPath = subswapper.DefaultConfigPath()

// monitorCycleTimeout bounds a single monitor cycle so a wedged usage probe
// (e.g. a hung codex app-server) cannot stall the loop forever.
const monitorCycleTimeout = 2 * time.Minute

func main() {
	if err := run(os.Args[1:], os.Stdout, os.Stderr); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return
		}
		_, _ = fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		if err := printUsage(stderr); err != nil {
			return err
		}
		return errors.New("missing command")
	}

	switch args[0] {
	case "init":
		return runInit(args[1:], stdout)
	case "import-cswap", "import-claude-swap":
		return runImportClaudeSwap(args[1:], stdout)
	case "capture":
		return runCapture(args[1:], stdout)
	case "home":
		return runHome(args[1:], stdout, stderr)
	case "remove", "rm":
		return runRemove(args[1:], stdout)
	case "status", "list":
		return runStatus(args[1:], stdout)
	case "switch":
		return runSwitch(args[1:], stdout)
	case "monitor":
		return runMonitor(args[1:], stdout)
	case "version", "-version", "--version":
		return printVersion(stdout)
	case "help", "-h", "--help":
		return printUsage(stdout)
	default:
		if err := printUsage(stderr); err != nil {
			return err
		}
		return fmt.Errorf("unknown command %q", args[0])
	}
}

func runHome(args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		return errors.New("missing home command: create, path, env, login, run, or migrate")
	}
	action := args[0]
	fs := flag.NewFlagSet("home "+action, flag.ContinueOnError)
	configPath := fs.String("config", defaultConfigPath, "config file")
	serviceName := fs.String("service", "", "service name")
	accountName := fs.String("account", "", "account name; defaults to the selected account")
	email := fs.String("email", "", "account email label")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	cfg, err := subswapper.LoadConfig(*configPath)
	if err != nil {
		return err
	}
	if action == "migrate" {
		result, err := subswapper.MigrateAccountHomes(*cfg)
		if err != nil {
			return err
		}
		_, err = fmt.Fprintf(stdout, "migrated account homes: copied %d, preserved %d existing files\n", result.Copied, result.Skipped)
		return err
	}
	if *serviceName == "" {
		return errors.New("missing -service")
	}
	if action == "create" {
		if *accountName == "" {
			return errors.New("missing -account")
		}
		account, home, err := subswapper.CreateAccountHome(*cfg, *serviceName, *accountName, *email)
		if err != nil {
			return err
		}
		_, err = fmt.Fprintf(stdout, "created %s account home %s at %s\n", *serviceName, account.Name, home)
		return err
	}
	service, account, home, err := resolveHomeSelection(*cfg, *serviceName, *accountName)
	if err != nil {
		return err
	}
	switch action {
	case "path":
		_, err = fmt.Fprintln(stdout, home)
		return err
	case "env":
		environment := subswapper.AccountEnvironment(*cfg, service, account)
		for key, value := range environment {
			if _, err := fmt.Fprintf(stdout, "export %s=%s\n", key, shellQuote(value)); err != nil {
				return err
			}
		}
		return nil
	case "login":
		command, commandArgs, err := providerLoginCommand(service)
		if err != nil {
			return err
		}
		if err := runWithAccountHome(*cfg, service, account, command, commandArgs, stdout, stderr); err != nil {
			return err
		}
		return subswapper.ResetAccountProbeState(*cfg, service.Name, account)
	case "run":
		commandArgs := fs.Args()
		if len(commandArgs) == 0 {
			commandArgs = []string{providerBinary(service)}
		}
		return runWithAccountHome(*cfg, service, account, commandArgs[0], commandArgs[1:], stdout, stderr)
	default:
		return fmt.Errorf("unknown home command %q", action)
	}
}

func resolveHomeSelection(cfg subswapper.Config, serviceName, accountName string) (subswapper.ServiceConfig, string, string, error) {
	service, ok := cfg.Service(serviceName)
	if !ok {
		return subswapper.ServiceConfig{}, "", "", fmt.Errorf("service %q not found", serviceName)
	}
	if !service.UsesAccountHomes() {
		return subswapper.ServiceConfig{}, "", "", fmt.Errorf("service %q does not use account homes", serviceName)
	}
	state, err := subswapper.LoadState(cfg.StatePath)
	if err != nil {
		return subswapper.ServiceConfig{}, "", "", err
	}
	if accountName == "" {
		accountName = state.Service(service.Name).ActiveAccount
	}
	if accountName == "" {
		return subswapper.ServiceConfig{}, "", "", fmt.Errorf("service %q has no selected account", serviceName)
	}
	if _, ok := state.Account(service.Name, accountName); !ok {
		return subswapper.ServiceConfig{}, "", "", fmt.Errorf("account %q not found for service %q", accountName, service.Name)
	}
	return service, accountName, subswapper.AccountDir(cfg, service.Name, accountName), nil
}

func providerBinary(service subswapper.ServiceConfig) string {
	if strings.EqualFold(service.Kind, "codex") {
		return "codex"
	}
	return "claude"
}

func providerLoginCommand(service subswapper.ServiceConfig) (string, []string, error) {
	switch strings.ToLower(service.Kind) {
	case "claude", "claude-code":
		return "claude", []string{"auth", "login"}, nil
	case "codex":
		return "codex", []string{"login"}, nil
	default:
		return "", nil, fmt.Errorf("service %q does not have a built-in login command", service.Name)
	}
}

func runWithAccountHome(cfg subswapper.Config, service subswapper.ServiceConfig, account, command string, args []string, stdout, stderr io.Writer) error {
	cmd := exec.Command(command, args...)
	cmd.Env = accountProcessEnvironment(os.Environ(), subswapper.AccountEnvironment(cfg, service, account))
	cmd.Stdin = os.Stdin
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	return cmd.Run()
}

func accountProcessEnvironment(base []string, overrides map[string]string) []string {
	result := make([]string, 0, len(base)+len(overrides))
	for _, entry := range base {
		key, _, _ := strings.Cut(entry, "=")
		if _, replaced := overrides[key]; !replaced {
			result = append(result, entry)
		}
	}
	for key, value := range overrides {
		result = append(result, key+"="+value)
	}
	return result
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", `'"'"'`) + "'"
}

func runInit(args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("init", flag.ContinueOnError)
	configPath := fs.String("config", defaultConfigPath, "config file to create")
	if err := fs.Parse(args); err != nil {
		return err
	}

	if err := subswapper.WriteSampleConfig(*configPath); err != nil {
		return err
	}
	_, err := fmt.Fprintf(stdout, "created %s\n", *configPath)
	return err
}

func runStatus(args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("status", flag.ContinueOnError)
	configPath := fs.String("config", defaultConfigPath, "config file")
	if err := fs.Parse(args); err != nil {
		return err
	}

	cfg, err := subswapper.LoadConfig(*configPath)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	cycle, err := subswapper.StatusOnce(ctx, *cfg)
	if err != nil {
		return err
	}
	_, err = io.WriteString(stdout, subswapper.RenderStatus(cycle.Results, nil, time.Now()))
	return err
}

func runImportClaudeSwap(args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("import-cswap", flag.ContinueOnError)
	configPath := fs.String("config", defaultConfigPath, "config file")
	root := fs.String("root", subswapper.DefaultClaudeSwapRoot(), "claude-swap data directory")
	if err := fs.Parse(args); err != nil {
		return err
	}

	cfg, err := subswapper.LoadConfig(*configPath)
	if err != nil {
		return err
	}
	result, err := subswapper.ImportClaudeSwap(*cfg, *root)
	if err != nil {
		return err
	}
	for _, account := range result.Imported {
		active := ""
		if account.Name == result.Active {
			active = " active"
		}
		if account.Email != "" {
			if _, err := fmt.Fprintf(stdout, "imported claude account %s (%s)%s\n", account.Name, account.Email, active); err != nil {
				return err
			}
			continue
		}
		if _, err := fmt.Fprintf(stdout, "imported claude account %s%s\n", account.Name, active); err != nil {
			return err
		}
	}
	for _, name := range result.Skipped {
		if _, err := fmt.Fprintf(stdout, "skipped existing account %s\n", name); err != nil {
			return err
		}
	}
	for _, importErr := range result.Errors {
		if _, err := fmt.Fprintf(stdout, "warning: %s\n", importErr); err != nil {
			return err
		}
	}
	return nil
}

func runCapture(args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("capture", flag.ContinueOnError)
	configPath := fs.String("config", defaultConfigPath, "config file")
	serviceName := fs.String("service", "", "service name")
	accountName := fs.String("account", "", "account name")
	email := fs.String("email", "", "account email label")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *serviceName == "" {
		return errors.New("missing -service")
	}
	if *accountName == "" {
		return errors.New("missing -account")
	}

	cfg, err := subswapper.LoadConfig(*configPath)
	if err != nil {
		return err
	}
	account, err := subswapper.CaptureAccount(*cfg, *serviceName, *accountName, *email)
	if err != nil {
		return err
	}
	if account.Email != "" {
		_, err := fmt.Fprintf(stdout, "captured %s account %s (%s)\n", *serviceName, account.Name, account.Email)
		return err
	}
	_, err = fmt.Fprintf(stdout, "captured %s account %s\n", *serviceName, account.Name)
	return err
}

func runRemove(args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("remove", flag.ContinueOnError)
	configPath := fs.String("config", defaultConfigPath, "config file")
	serviceName := fs.String("service", "", "service name")
	accountName := fs.String("account", "", "account name")
	force := fs.Bool("force", false, "remove even if this account is active")
	deleteHome := fs.Bool("delete-home", false, "also permanently delete an account home and its contents")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *serviceName == "" {
		return errors.New("missing -service")
	}
	if *accountName == "" {
		return errors.New("missing -account")
	}

	cfg, err := subswapper.LoadConfig(*configPath)
	if err != nil {
		return err
	}
	if err := subswapper.RemoveAccountWithOptions(*cfg, *serviceName, *accountName, *force, *deleteHome); err != nil {
		return err
	}
	action := "unregistered"
	if *deleteHome {
		action = "removed"
	}
	_, err = fmt.Fprintf(stdout, "%s %s account %s\n", action, *serviceName, *accountName)
	return err
}

func runSwitch(args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("switch", flag.ContinueOnError)
	configPath := fs.String("config", defaultConfigPath, "config file")
	serviceName := fs.String("service", "", "service name")
	accountName := fs.String("account", "auto", "account name, or auto")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *serviceName == "" {
		return errors.New("missing -service")
	}

	cfg, err := subswapper.LoadConfig(*configPath)
	if err != nil {
		return err
	}
	if *accountName == "auto" {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		switches, err := subswapper.SwitchBest(ctx, *cfg, *serviceName)
		for _, event := range switches {
			if _, writeErr := fmt.Fprintf(stdout, "switched %s to %s\n", event.Service, event.Account); writeErr != nil {
				return errors.Join(err, writeErr)
			}
		}
		if err == nil && len(switches) == 0 {
			_, err = fmt.Fprintln(stdout, "already on the best account")
		}
		return err
	}
	if *serviceName == "all" {
		return errors.New("-service all requires -account auto")
	}
	if err := subswapper.SwitchAccount(*cfg, *serviceName, *accountName); err != nil {
		return err
	}
	_, err = fmt.Fprintf(stdout, "switched %s to %s\n", *serviceName, *accountName)
	return err
}

func runMonitor(args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("monitor", flag.ContinueOnError)
	configPath := fs.String("config", defaultConfigPath, "config file")
	interval := fs.Duration("interval", 0, "override monitor interval")
	once := fs.Bool("once", false, "run one monitor cycle")
	noAuto := fs.Bool("no-auto", false, "observe without switching")
	verbose := fs.Bool("verbose", false, "print the full status table every cycle")
	if err := fs.Parse(args); err != nil {
		return err
	}

	cfg, err := subswapper.LoadConfig(*configPath)
	if err != nil {
		return err
	}
	monitorInterval := cfg.Monitor.Interval.Duration
	if *interval > 0 {
		monitorInterval = *interval
	}
	if monitorInterval <= 0 {
		monitorInterval = time.Minute
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	autoSwitch := cfg.Monitor.AutoSwitchEnabled() && !*noAuto
	return runMonitorLoop(ctx, *cfg, monitorInterval, *once, autoSwitch, *verbose, stdout, subswapper.MonitorOnce)
}

type monitorCycleRunner func(context.Context, subswapper.Config, bool) subswapper.CycleResult

func runMonitorLoop(
	ctx context.Context,
	cfg subswapper.Config,
	monitorInterval time.Duration,
	once bool,
	autoSwitch bool,
	verbose bool,
	stdout io.Writer,
	runCycle monitorCycleRunner,
) error {
	var previous []subswapper.ServiceStatus
	previousCycleError := ""
	first := true
	for {
		cycleCtx, cancelCycle := context.WithTimeout(ctx, monitorCycleTimeout)
		cycle := runCycle(cycleCtx, cfg, autoSwitch)
		cancelCycle()
		if once || verbose {
			if _, err := io.WriteString(stdout, subswapper.RenderStatus(cycle.Results, cycle.Switches, time.Now())); err != nil {
				return err
			}
		} else {
			if first {
				if _, err := fmt.Fprintf(stdout, "subswapper monitor started, interval %s\n", monitorInterval); err != nil {
					return err
				}
			}
			if events := subswapper.RenderMonitorEvents(previous, cycle.Results, cycle.Switches); events != "" {
				if _, err := io.WriteString(stdout, events); err != nil {
					return err
				}
			}
		}
		if !once {
			cycleError := summarizeCycleErrors(cycle.Errors)
			switch {
			case cycleError != "" && cycleError != previousCycleError:
				if _, err := fmt.Fprintf(stdout, "error monitor: %s\n", cycleError); err != nil {
					return err
				}
			case cycleError == "" && previousCycleError != "":
				if _, err := fmt.Fprintln(stdout, "recovered monitor"); err != nil {
					return err
				}
			}
			previousCycleError = cycleError
		}
		if once {
			return errors.Join(cycle.Errors...)
		}
		previous = cycle.Results
		first = false

		timer := time.NewTimer(monitorInterval)
		select {
		case <-ctx.Done():
			timer.Stop()
			if errors.Is(ctx.Err(), context.Canceled) {
				return nil
			}
			return ctx.Err()
		case <-timer.C:
		}
	}
}

func summarizeCycleErrors(errs []error) string {
	if len(errs) == 0 {
		return ""
	}
	summary := strings.Join(strings.Fields(errors.Join(errs...).Error()), " ")
	if len(summary) > 512 {
		summary = summary[:512]
	}
	return summary
}

func printVersion(w io.Writer) error {
	version := "unknown"
	if info, ok := debug.ReadBuildInfo(); ok && info.Main.Version != "" {
		version = info.Main.Version
	}
	_, err := fmt.Fprintln(w, "subswapper", version, runtime.Version())
	return err
}

func printUsage(w io.Writer) error {
	_, err := fmt.Fprintln(w, `subswapper manages isolated Claude Code and Codex account homes and usage limits.

Usage:
  subswapper init [-config ~/.config/subswapper/config.json]
  subswapper import-cswap [-root ~/.local/share/claude-swap]
  subswapper home create -service claude|codex -account <name> [-email user@example.com]
  subswapper home path|env -service claude|codex [-account <name>]
  subswapper home login -service claude|codex [-account <name>]
  subswapper home run -service claude|codex [-account <name>] [-- command args...]
  subswapper home migrate [-config ~/.config/subswapper/config.json]
  subswapper capture -service claude|codex -account <name> [-email user@example.com]
  subswapper remove -service claude|codex -account <name> [-force] [-delete-home]
  subswapper status [-config ~/.config/subswapper/config.json]
  subswapper switch -service claude|codex|all [-account auto|name] [-config ~/.config/subswapper/config.json]
  subswapper monitor [-config ~/.config/subswapper/config.json] [-interval 5m] [-once] [-no-auto] [-verbose]
  subswapper version`)
	return err
}
