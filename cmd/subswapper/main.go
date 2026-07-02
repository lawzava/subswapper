package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
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
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		printUsage(stderr)
		return errors.New("missing command")
	}

	switch args[0] {
	case "init":
		return runInit(args[1:], stdout)
	case "import-cswap", "import-claude-swap":
		return runImportClaudeSwap(args[1:], stdout)
	case "capture":
		return runCapture(args[1:], stdout)
	case "remove", "rm":
		return runRemove(args[1:], stdout)
	case "status", "list":
		return runStatus(args[1:], stdout)
	case "switch":
		return runSwitch(args[1:], stdout)
	case "monitor":
		return runMonitor(args[1:], stdout)
	case "version", "-version", "--version":
		printVersion(stdout)
		return nil
	case "help", "-h", "--help":
		printUsage(stdout)
		return nil
	default:
		printUsage(stderr)
		return fmt.Errorf("unknown command %q", args[0])
	}
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
	fmt.Fprintf(stdout, "created %s\n", *configPath)
	return nil
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
	fmt.Fprint(stdout, subswapper.RenderStatus(cycle.Results, nil, time.Now()))
	return nil
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
			fmt.Fprintf(stdout, "imported claude account %s (%s)%s\n", account.Name, account.Email, active)
			continue
		}
		fmt.Fprintf(stdout, "imported claude account %s%s\n", account.Name, active)
	}
	for _, name := range result.Skipped {
		fmt.Fprintf(stdout, "skipped existing account %s\n", name)
	}
	for _, importErr := range result.Errors {
		fmt.Fprintf(stdout, "warning: %s\n", importErr)
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
		fmt.Fprintf(stdout, "captured %s account %s (%s)\n", *serviceName, account.Name, account.Email)
		return nil
	}
	fmt.Fprintf(stdout, "captured %s account %s\n", *serviceName, account.Name)
	return nil
}

func runRemove(args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("remove", flag.ContinueOnError)
	configPath := fs.String("config", defaultConfigPath, "config file")
	serviceName := fs.String("service", "", "service name")
	accountName := fs.String("account", "", "account name")
	force := fs.Bool("force", false, "remove even if this account is active")
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
	if err := subswapper.RemoveAccount(*cfg, *serviceName, *accountName, *force); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "removed %s account %s\n", *serviceName, *accountName)
	return nil
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
			fmt.Fprintf(stdout, "switched %s to %s\n", event.Service, event.Account)
		}
		if err == nil && len(switches) == 0 {
			fmt.Fprintln(stdout, "already on the best account")
		}
		return err
	}
	if *serviceName == "all" {
		return errors.New("-service all requires -account auto")
	}
	if err := subswapper.SwitchAccount(*cfg, *serviceName, *accountName); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "switched %s to %s\n", *serviceName, *accountName)
	return nil
}

func runMonitor(args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("monitor", flag.ContinueOnError)
	configPath := fs.String("config", defaultConfigPath, "config file")
	interval := fs.Duration("interval", 0, "override monitor interval")
	once := fs.Bool("once", false, "run one monitor cycle")
	noAuto := fs.Bool("no-auto", false, "observe without switching")
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
	for {
		cycleCtx, cancelCycle := context.WithTimeout(ctx, monitorCycleTimeout)
		cycle := subswapper.MonitorOnce(cycleCtx, *cfg, autoSwitch)
		cancelCycle()
		fmt.Fprint(stdout, subswapper.RenderStatus(cycle.Results, cycle.Switches, time.Now()))
		if len(cycle.Errors) > 0 {
			fmt.Fprintf(stdout, "\nerrors:\n")
			for _, cycleErr := range cycle.Errors {
				fmt.Fprintf(stdout, "- %s\n", cycleErr)
			}
		}
		if *once {
			return errors.Join(cycle.Errors...)
		}

		timer := time.NewTimer(monitorInterval)
		select {
		case <-ctx.Done():
			timer.Stop()
			if errors.Is(ctx.Err(), context.Canceled) {
				return nil
			}
			return ctx.Err()
		case <-timer.C:
			fmt.Fprintln(stdout, strings.Repeat("-", 80))
		}
	}
}

func printVersion(w io.Writer) {
	version := "unknown"
	if info, ok := debug.ReadBuildInfo(); ok && info.Main.Version != "" {
		version = info.Main.Version
	}
	fmt.Fprintln(w, "subswapper", version)
}

func printUsage(w io.Writer) {
	fmt.Fprintln(w, `subswapper manages Claude Code and Codex subscription account bundles.

Usage:
  subswapper init [-config ~/.config/subswapper/config.json]
  subswapper import-cswap [-root ~/.local/share/claude-swap]
  subswapper capture -service claude|codex -account <name> [-email user@example.com]
  subswapper remove -service claude|codex -account <name> [-force]
  subswapper status [-config ~/.config/subswapper/config.json]
  subswapper switch -service claude|codex|all [-account auto|name] [-config ~/.config/subswapper/config.json]
  subswapper monitor [-config ~/.config/subswapper/config.json] [-interval 30s] [-once] [-no-auto]
  subswapper version`)
}
