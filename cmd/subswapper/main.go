package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strings"
	"syscall"
	"time"

	"github.com/lawzava/subswapper/internal/subswapper"
	"golang.org/x/term"
)

var defaultConfigPath = subswapper.DefaultConfigPath()

var lookupClaudeSetupTokenIdentity = subswapper.LookupClaudeSetupTokenIdentity

// monitorCycleTimeout bounds a single monitor cycle so a wedged usage probe
// (e.g. a hung codex app-server) cannot stall the loop forever.
const monitorCycleTimeout = 2 * time.Minute

var claudeAuthStatusTimeout = 15 * time.Second

func main() {
	if err := runWithInput(os.Args[1:], os.Stdin, os.Stdout, os.Stderr); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return
		}
		_, _ = fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(args []string, stdout, stderr io.Writer) error {
	return runWithInput(args, os.Stdin, stdout, stderr)
}

func runWithInput(args []string, stdin io.Reader, stdout, stderr io.Writer) error {
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
		return runHome(args[1:], stdin, stdout, stderr)
	case "claude-statusline":
		return runClaudeStatusLine(stdin, stdout)
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

func runHome(args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		return errors.New("missing home command: create, repair, path, env, login, run, token, or migrate")
	}
	action := args[0]
	if action == "token" {
		return runHomeToken(args[1:], stdin, stdout, stderr)
	}
	fs := flag.NewFlagSet("home "+action, flag.ContinueOnError)
	fs.SetOutput(stderr)
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
	case "repair":
		result, repairErr := subswapper.RepairAccountHome(*cfg, service.Name, account)
		if repairErr != nil && len(result.Linked)+len(result.Unchanged)+len(result.Missing)+len(result.Conflicts) == 0 {
			return repairErr
		}
		if err := printHomeRepairResult(stdout, service.Name, account, result); err != nil {
			return err
		}
		if repairErr != nil {
			return repairErr
		}
		if len(result.Conflicts) != 0 {
			return fmt.Errorf("claude home repair found %d conflicts", len(result.Conflicts))
		}
		return nil
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
		if err := runWithAccountHome(*cfg, service, account, command, commandArgs, stdin, stdout, stderr); err != nil {
			return err
		}
		return subswapper.ResetAccountProbeState(*cfg, service.Name, account)
	case "run":
		commandArgs := fs.Args()
		if len(commandArgs) == 0 {
			commandArgs = []string{providerBinary(service)}
		}
		if isClaudeServiceConfig(service) {
			runtimeHome := subswapper.RuntimeHome(*cfg, service, account)
			return runClaudeWithSetupToken(*cfg, *configPath, service, account, runtimeHome, commandArgs[0], commandArgs[1:], stdin, stdout, stderr)
		}
		return runWithAccountHome(*cfg, service, account, commandArgs[0], commandArgs[1:], stdin, stdout, stderr)
	default:
		return fmt.Errorf("unknown home command %q", action)
	}
}

func printHomeRepairResult(w io.Writer, serviceName, accountName string, result subswapper.HomeRepairResult) error {
	if _, err := fmt.Fprintf(w, "repaired %s account home %s: linked %d, unchanged %d, missing %d, conflicts %d\n",
		serviceName, accountName, len(result.Linked), len(result.Unchanged), len(result.Missing), len(result.Conflicts)); err != nil {
		return err
	}
	for _, detail := range []struct {
		label string
		items []string
	}{
		{label: "linked", items: result.Linked},
		{label: "unchanged", items: result.Unchanged},
		{label: "missing sources", items: result.Missing},
		{label: "conflicts", items: result.Conflicts},
	} {
		if len(detail.items) == 0 {
			continue
		}
		if _, err := fmt.Fprintf(w, "%s: %s\n", detail.label, strings.Join(detail.items, ", ")); err != nil {
			return err
		}
	}
	return nil
}

func runHomeToken(args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		return errors.New("missing home token command: set, status, or remove")
	}
	action := args[0]
	if action != "set" && action != "status" && action != "remove" {
		return errors.New("unknown home token command")
	}
	fs := flag.NewFlagSet("home token", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	configPath := fs.String("config", defaultConfigPath, "config file")
	serviceName := fs.String("service", "", "service name")
	accountName := fs.String("account", "", "account name; defaults to the selected account")
	if err := fs.Parse(args[1:]); err != nil {
		return errors.New("invalid home token options")
	}
	if len(fs.Args()) != 0 {
		return errors.New("setup tokens must be provided through stdin or the interactive prompt")
	}
	if *serviceName == "" {
		return errors.New("missing -service")
	}
	cfg, err := subswapper.LoadConfig(*configPath)
	if err != nil {
		return err
	}
	service, account, _, err := resolveHomeSelection(*cfg, *serviceName, *accountName)
	if err != nil {
		return err
	}
	if !isClaudeServiceConfig(service) {
		return fmt.Errorf("service %q does not support Claude setup tokens", service.Name)
	}

	switch action {
	case "set":
		token, err := readClaudeSetupToken(stdin, stderr)
		if err != nil {
			return err
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		status, err := subswapper.ReplaceClaudeSetupToken(ctx, *cfg, service.Name, account, token, lookupClaudeSetupTokenIdentity)
		if err != nil {
			return errors.New("claude setup token was rejected")
		}
		identity := "identity unknown"
		if status.IdentityKnown {
			identity = "identity known"
		}
		_, err = fmt.Fprintf(stdout, "stored Claude setup token for %s; %s\n", account, identity)
		return err
	case "status":
		status, err := subswapper.ClaudeSetupTokenStatusForAccount(*cfg, service.Name, account)
		if err != nil {
			return errors.New("claude setup token status is unavailable")
		}
		state := "not configured"
		switch {
		case status.Expired:
			state = "expired"
		case status.Usable:
			state = "configured"
		}
		identity := "identity unknown"
		if status.IdentityKnown {
			identity = "identity known"
		}
		_, err = fmt.Fprintf(stdout, "Claude setup token for %s: %s; %s\n", account, state, identity)
		return err
	case "remove":
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := subswapper.RemoveClaudeSetupToken(ctx, *cfg, service.Name, account); err != nil {
			return errors.New("claude setup token removal failed")
		}
		_, err = fmt.Fprintf(stdout, "removed Claude setup token for %s\n", account)
		return err
	default:
		return errors.New("unknown home token command")
	}
}

func readClaudeSetupToken(stdin io.Reader, prompt io.Writer) (string, error) {
	if stdin == nil {
		return "", errors.New("claude setup token input is unavailable")
	}
	if file, ok := stdin.(*os.File); ok && term.IsTerminal(int(file.Fd())) {
		_, _ = fmt.Fprint(prompt, "Claude setup token: ")
		value, err := term.ReadPassword(int(file.Fd()))
		_, _ = fmt.Fprintln(prompt)
		if err != nil {
			return "", errors.New("claude setup token input failed")
		}
		return string(value), nil
	}
	const maxTokenBytes = 64 << 10
	value, err := io.ReadAll(io.LimitReader(stdin, maxTokenBytes+1))
	if err != nil {
		return "", errors.New("claude setup token input failed")
	}
	if len(value) > maxTokenBytes {
		return "", errors.New("claude setup token input is too large")
	}
	return string(value), nil
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

func runWithAccountHome(cfg subswapper.Config, service subswapper.ServiceConfig, account, command string, args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	cmd := exec.Command(command, args...)
	cmd.Env = accountProcessEnvironment(os.Environ(), subswapper.AccountEnvironment(cfg, service, account))
	cmd.Stdin = stdin
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	return cmd.Run()
}

func isClaudeServiceConfig(service subswapper.ServiceConfig) bool {
	return strings.EqualFold(service.Kind, "claude") || strings.EqualFold(service.Kind, "claude-code")
}

func runClaudeWithSetupToken(
	cfg subswapper.Config,
	configPath string,
	service subswapper.ServiceConfig,
	account string,
	runtimeHome string,
	command string,
	args []string,
	stdin io.Reader,
	stdout io.Writer,
	stderr io.Writer,
) error {
	token, status, err := subswapper.LoadClaudeSetupTokenWithStatus(cfg, service.Name, account)
	if err != nil || !status.Usable {
		return errors.New("selected Claude account has no usable setup token")
	}
	prepareRuntimeHome := subswapper.PrepareClaudeAccountHome
	if service.SharedRuntimeHome != "" {
		prepareRuntimeHome = subswapper.PrepareClaudeSharedRuntimeHome
	}
	if err := prepareRuntimeHome(runtimeHome); err != nil {
		return err
	}
	metadata := map[string]string{
		"SUBSWAPPER_CONFIG_PATH":    configPath,
		"SUBSWAPPER_SERVICE":        service.Name,
		"SUBSWAPPER_ACCOUNT":        account,
		"SUBSWAPPER_TOKEN_REVISION": status.Revision,
	}
	environment, err := subswapper.BuildClaudeLaunchEnvironment(os.Environ(), runtimeHome, token, metadata)
	if err != nil {
		return errors.New("selected Claude account environment is unusable")
	}
	commandArgs := append([]string(nil), args...)
	if isClaudeExecutable(command) {
		if err := checkClaudeAuthentication(command, environment); err != nil {
			return err
		}
		overlay, err := claudeStatusLineSettingsOverlay()
		if err != nil {
			return err
		}
		commandArgs = append([]string{"--settings", overlay}, commandArgs...)
	}
	cmd := exec.Command(command, commandArgs...)
	cmd.Env = environment
	if handled, terminalErr := runClaudeTerminalCommand(cmd, stdin, stdout, stderr, token); handled {
		return terminalErr
	}
	cmd.Stdin = stdin
	redactedStdout := newSecretRedactingWriter(stdout, token)
	redactedStderr := newSecretRedactingWriter(stderr, token)
	cmd.Stdout = redactedStdout
	cmd.Stderr = redactedStderr
	runErr := cmd.Run()
	if err := errors.Join(redactedStdout.Flush(), redactedStderr.Flush()); err != nil {
		return errors.New("claude output forwarding failed")
	}
	return runErr
}

func isClaudeExecutable(command string) bool {
	name := strings.ToLower(filepath.Base(command))
	name = strings.TrimSuffix(name, ".exe")
	return name == "claude"
}

type secretRedactingWriter struct {
	destination io.Writer
	secret      []byte
	pending     []byte
}

func newSecretRedactingWriter(destination io.Writer, secret string) *secretRedactingWriter {
	if destination == nil {
		destination = io.Discard
	}
	return &secretRedactingWriter{destination: destination, secret: []byte(secret)}
}

func (w *secretRedactingWriter) Write(data []byte) (int, error) {
	w.pending = append(w.pending, data...)
	if err := w.drain(false); err != nil {
		return 0, err
	}
	return len(data), nil
}

func (w *secretRedactingWriter) Flush() error {
	return w.drain(true)
}

func (w *secretRedactingWriter) drain(final bool) error {
	if len(w.secret) == 0 {
		return errors.New("output redaction is unavailable")
	}
	for len(w.pending) > 0 {
		if index := bytes.Index(w.pending, w.secret); index >= 0 {
			if err := writeAll(w.destination, w.pending[:index]); err != nil {
				return err
			}
			if err := writeAll(w.destination, []byte("[REDACTED]")); err != nil {
				return err
			}
			w.pending = w.pending[index+len(w.secret):]
			continue
		}
		if final {
			output := w.pending
			if bytes.HasPrefix(w.secret, w.pending) {
				output = []byte("[REDACTED]")
			}
			if err := writeAll(w.destination, output); err != nil {
				return err
			}
			w.pending = nil
			return nil
		}
		retain := longestSecretPrefixSuffix(w.pending, w.secret)
		emitLimit := len(w.pending) - retain
		if err := writeAll(w.destination, w.pending[:emitLimit]); err != nil {
			return err
		}
		w.pending = w.pending[emitLimit:]
		return nil
	}
	return nil
}

func longestSecretPrefixSuffix(data, secret []byte) int {
	limit := min(len(data), len(secret)-1)
	for length := limit; length > 0; length-- {
		if bytes.Equal(data[len(data)-length:], secret[:length]) {
			return length
		}
	}
	return 0
}

func writeAll(destination io.Writer, data []byte) error {
	for len(data) > 0 {
		written, err := destination.Write(data)
		if err != nil {
			return err
		}
		if written <= 0 || written > len(data) {
			return io.ErrShortWrite
		}
		data = data[written:]
	}
	return nil
}

type limitedOutput struct {
	buffer bytes.Buffer
	limit  int
}

func (w *limitedOutput) Write(data []byte) (int, error) {
	remaining := w.limit - w.buffer.Len()
	if remaining > 0 {
		if len(data) > remaining {
			_, _ = w.buffer.Write(data[:remaining])
		} else {
			_, _ = w.buffer.Write(data)
		}
	}
	return len(data), nil
}

func checkClaudeAuthentication(command string, environment []string) error {
	// Use the selected executable so diagnostics and launch cannot select
	// different Claude installations.
	ctx, cancel := context.WithTimeout(context.Background(), claudeAuthStatusTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, command, "auth", "status")
	cmd.WaitDelay = 250 * time.Millisecond
	cmd.Env = environment
	cmd.Stdin = nil
	output := &limitedOutput{limit: 64 << 10}
	cmd.Stdout = output
	cmd.Stderr = &limitedOutput{limit: 64 << 10}
	if err := cmd.Run(); err != nil {
		return errors.New("selected Claude account authentication check failed")
	}
	var status struct {
		LoggedIn    bool   `json:"loggedIn"`
		AuthMethod  string `json:"authMethod"`
		APIProvider string `json:"apiProvider"`
	}
	if err := json.Unmarshal(output.buffer.Bytes(), &status); err != nil ||
		!status.LoggedIn || status.AuthMethod != "oauth_token" || status.APIProvider != "firstParty" {
		return errors.New("selected Claude account authentication is not usable")
	}
	return nil
}

func claudeStatusLineSettingsOverlay() (string, error) {
	executable, err := os.Executable()
	if err != nil {
		return "", errors.New("cannot configure Claude usage capture")
	}
	overlay := struct {
		StatusLine struct {
			Type    string `json:"type"`
			Command string `json:"command"`
		} `json:"statusLine"`
	}{}
	overlay.StatusLine.Type = "command"
	overlay.StatusLine.Command = shellQuote(executable) + " claude-statusline"
	data, err := json.Marshal(overlay)
	if err != nil {
		return "", errors.New("cannot configure Claude usage capture")
	}
	return string(data), nil
}

func runClaudeStatusLine(stdin io.Reader, stdout io.Writer) error {
	const maxPayloadBytes = 1 << 20
	input, err := io.ReadAll(io.LimitReader(stdin, maxPayloadBytes+1))
	if err != nil || len(input) > maxPayloadBytes {
		return errors.New("claude status-line input is unavailable")
	}
	configPath := os.Getenv("SUBSWAPPER_CONFIG_PATH")
	serviceName := os.Getenv("SUBSWAPPER_SERVICE")
	accountName := os.Getenv("SUBSWAPPER_ACCOUNT")
	tokenRevision := os.Getenv("SUBSWAPPER_TOKEN_REVISION")
	if configPath == "" || serviceName == "" || accountName == "" || tokenRevision == "" {
		return errors.New("claude status-line routing context is unavailable")
	}
	cfg, err := subswapper.LoadConfig(configPath)
	if err != nil {
		return errors.New("claude status-line configuration is unavailable")
	}
	service, ok := cfg.Service(serviceName)
	if !ok || !isClaudeServiceConfig(service) {
		return errors.New("claude status-line service is unavailable")
	}

	var contextPayload struct {
		Workspace struct {
			ProjectDir string `json:"project_dir"`
			CurrentDir string `json:"current_dir"`
		} `json:"workspace"`
	}
	_ = json.Unmarshal(input, &contextPayload)
	workspaceRoot := contextPayload.Workspace.ProjectDir
	if workspaceRoot == "" {
		workspaceRoot = contextPayload.Workspace.CurrentDir
	}
	runtimeHome := subswapper.RuntimeHome(*cfg, service, accountName)
	command, found, resolveErr := subswapper.ResolveClaudeStatusLineCommand(runtimeHome, workspaceRoot)
	if resolveErr != nil {
		return errors.New("claude status-line settings are unavailable")
	}

	observedAt := time.Now().UTC()
	if sample, parseErr := subswapper.ParseClaudeStatusLine(input, observedAt, tokenRevision); parseErr == nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_ = subswapper.RecordClaudeStatusLineUsage(ctx, *cfg, serviceName, accountName, sample)
		cancel()
	}
	if !found {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return subswapper.RunClaudeStatusLineCommand(ctx, command, input, stdout)
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
  subswapper home repair -service claude [-account <name>]
  subswapper home path|env -service claude|codex [-account <name>]
  subswapper home login -service claude|codex [-account <name>]
  subswapper home token set|status|remove -service claude [-account <name>]
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
