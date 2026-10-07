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
		if os.Getenv(delegateMarker) != "" && len(os.Args) > 1 && os.Args[1] == "home" {
			var diagnostic launchDiagnostic
			if errors.As(err, &diagnostic) {
				_, _ = fmt.Fprintln(os.Stderr, diagnostic)
			} else {
				_, _ = fmt.Fprintln(os.Stderr, "delegated provider launch failed")
			}
		} else {
			_, _ = fmt.Fprintln(os.Stderr, err)
		}
		os.Exit(processExitCode(err))
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
	case "delegate":
		return runDelegate(args[1:], stdout, stderr)
	case "init":
		return runInit(args[1:], stdout)
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
	case "proxy":
		return runProxy(args[1:], stdout)
	case "setup":
		return runSetup(args[1:], stdout, stderr)
	case "doctor":
		return runDoctor(args[1:], stdout)
	case "add":
		return runAdd(args[1:], stdin, stdout, stderr)
	case "claude", "codex":
		return runProviderShortcut(args[0], args[1:], stdin, stdout, stderr)
	case "hub":
		return runHub(args[1:], stdin, stdout, stderr)
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
		return errors.New("missing home command: run, path, or proxy-auth")
	}
	action := args[0]
	fs := flag.NewFlagSet("home "+action, flag.ContinueOnError)
	fs.SetOutput(stderr)
	configPath := fs.String("config", defaultConfigPath, "config file")
	serviceName := fs.String("service", "", "service name")
	accountName := fs.String("account", "", "account name; defaults to the selected account")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	cfg, err := subswapper.LoadConfig(*configPath)
	if err != nil {
		return err
	}
	if *serviceName == "" {
		return errors.New("missing -service")
	}
	if service, ok := cfg.Service(*serviceName); ok && service.HubClient() {
		return runHubClientHome(*cfg, *configPath, service, action, *accountName, fs.Args(), stdin, stdout, stderr)
	}
	service, account, home, err := resolveHomeSelection(*cfg, *serviceName, *accountName)
	if err != nil {
		if action == "run" {
			return classifyLauncherFailure(err)
		}
		return err
	}
	switch action {
	case "path":
		_, err = fmt.Fprintln(stdout, home)
		return err
	case "run":
		commandArgs := fs.Args()
		if len(commandArgs) == 0 {
			commandArgs = []string{providerBinary(service)}
		}
		if isClaudeServiceConfig(service) {
			runtimeHome := subswapper.RuntimeHome(*cfg, service, account)
			return runClaudeWithSetupToken(*cfg, *configPath, service, account, runtimeHome, commandArgs[0], commandArgs[1:], stdin, stdout, stderr)
		}
		if service.CodexProxyEnabled() {
			return runCodexThroughProxy(*cfg, *configPath, service, account, commandArgs[0], commandArgs[1:], stdin, stdout, stderr)
		}
		return runWithAccountHome(*cfg, service, account, commandArgs[0], commandArgs[1:], stdin, stdout, stderr)
	case "proxy-auth":
		if !service.CodexProxyEnabled() {
			return fmt.Errorf("service %q is not a Codex service with proxy_listen", service.Name)
		}
		placeholder, err := subswapper.LoadOrCreateCodexProxyPlaceholder(*cfg, service.Name)
		if err != nil {
			return err
		}
		return installCodexPlaceholder(subswapper.RuntimeHome(*cfg, service, account), placeholder, stdout)
	default:
		return fmt.Errorf("unknown home command %q", action)
	}
}

func installCodexPlaceholder(runtimeHome string, placeholder subswapper.CodexProxyPlaceholder, stdout io.Writer) error {
	backup, err := subswapper.ReplaceCodexRuntimeAuth(runtimeHome, placeholder)
	if err != nil {
		return err
	}
	if backup != "" {
		if _, err := fmt.Fprintf(stdout, "moved the previous login to %s\n", backup); err != nil {
			return err
		}
	}
	_, err = fmt.Fprintf(stdout, "installed the Codex proxy placeholder in %s\n", runtimeHome)
	return err
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

func runWithAccountHome(cfg subswapper.Config, service subswapper.ServiceConfig, account, command string, args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	cmd := exec.Command(command, args...)
	cmd.Env = accountProcessEnvironment(os.Environ(), subswapper.AccountEnvironment(cfg, service, account))
	cmd.Stdin = stdin
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	return cmd.Run()
}

// runCodexThroughProxy launches Codex against the local ChatGPT auth proxy.
// The process holds only a placeholder login; the proxy swaps real tokens per
// request. A proxy that is configured but not running degrades to a
// fixed-login launch in the selected account home so work is never blocked.
func runCodexThroughProxy(
	cfg subswapper.Config,
	configPath string,
	service subswapper.ServiceConfig,
	account string,
	command string,
	args []string,
	stdin io.Reader,
	stdout io.Writer,
	stderr io.Writer,
) error {
	placeholder, err := subswapper.LoadOrCreateCodexProxyPlaceholder(cfg, service.Name)
	switch {
	case err != nil:
		_, _ = fmt.Fprintf(stderr, "subswapper: Codex proxy placeholder unavailable (%v); launching with the account login\n", err)
		return runWithAccountHome(cfg, service, account, command, args, stdin, stdout, stderr)
	case !subswapper.CodexProxyReachable(service, placeholder):
		_, _ = fmt.Fprintf(stderr, "subswapper: Codex proxy at %s is not running; launching with the account login\n", service.ProxyListen)
		return runWithAccountHome(cfg, service, account, command, args, stdin, stdout, stderr)
	}
	// A shared or native runtime home must hold the placeholder: a real login
	// there would refresh itself and invalidate the registered copy. The
	// account home keeps its own login, which the proxy overrides anyway.
	runtimeHome := subswapper.RuntimeHome(cfg, service, account)
	launchHome := runtimeHome
	if service.SharedRuntimeHome != "" {
		if err := subswapper.EnsureCodexProxyAuth(runtimeHome, placeholder); err != nil {
			if errors.Is(err, subswapper.ErrCodexRuntimeAuthIsReal) {
				return fmt.Errorf("%s/auth.json holds a real ChatGPT login; capture it with `subswapper capture -service %s -account <name>` and then run `subswapper home proxy-auth -service %s`", runtimeHome, service.Name, service.Name)
			}
			return err
		}
		if service.UsesNativeRuntimeHome() {
			launchHome = ""
		}
	}
	metadata := map[string]string{
		"SUBSWAPPER_CONFIG_PATH": configPath,
		"SUBSWAPPER_SERVICE":     service.Name,
		"SUBSWAPPER_ACCOUNT":     account,
		"SUBSWAPPER_PROXY":       "1",
	}
	commandArgs := append([]string(nil), args...)
	if isCodexExecutable(command) {
		commandArgs = append(subswapper.CodexProxyLaunchArgs(service.ProxyListen), commandArgs...)
	}
	cmd := exec.Command(command, commandArgs...)
	cmd.Env = subswapper.BuildCodexProxyLaunchEnvironment(os.Environ(), launchHome, metadata)
	cmd.Stdin = stdin
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	return cmd.Run()
}

func isCodexExecutable(command string) bool {
	base := strings.TrimSuffix(filepath.Base(command), filepath.Ext(command))
	return strings.EqualFold(base, "codex")
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
		return diagnoseClaudeTokenFailure(err)
	}
	// Through the proxy the process only ever holds the shared secret; the
	// proxy swaps real tokens per request. A proxy that is configured but not
	// running degrades to a fixed-token launch so work is never blocked.
	proxyMode := false
	if service.ClaudeProxyEnabled() {
		secret, secretErr := subswapper.LoadOrCreateClaudeProxySecret(cfg, service.Name)
		switch {
		case secretErr != nil:
			_, _ = fmt.Fprintf(stderr, "subswapper: Claude proxy secret unavailable (%v); launching with a fixed account token\n", secretErr)
		case !subswapper.ClaudeProxyReachable(service, secret):
			_, _ = fmt.Fprintf(stderr, "subswapper: Claude proxy at %s is not running; launching with a fixed account token\n", service.ProxyListen)
		default:
			proxyMode = true
			token = secret
		}
	}
	// With the native home Claude keeps its own files; there is nothing to
	// prepare and the real token never reaches this process. A fallback launch
	// still isolates the fixed token in the account home.
	nativeHome := proxyMode && service.UsesNativeRuntimeHome()
	if !nativeHome {
		if service.UsesNativeRuntimeHome() {
			runtimeHome = subswapper.AccountDir(cfg, service.Name, account)
		}
		prepareRuntimeHome := subswapper.PrepareClaudeAccountHome
		if service.SharedRuntimeHome != "" {
			prepareRuntimeHome = subswapper.PrepareClaudeSharedRuntimeHome
		}
		if err := prepareRuntimeHome(runtimeHome); err != nil {
			var diagnostic launchDiagnostic
			if classified := classifyLauncherFailure(err); errors.As(classified, &diagnostic) {
				return diagnostic
			}
			return launchRuntimeUnavailable
		}
	}
	metadata := map[string]string{
		"SUBSWAPPER_CONFIG_PATH":    configPath,
		"SUBSWAPPER_SERVICE":        service.Name,
		"SUBSWAPPER_ACCOUNT":        account,
		"SUBSWAPPER_TOKEN_REVISION": status.Revision,
	}
	var environment []string
	if proxyMode {
		metadata["SUBSWAPPER_PROXY"] = "1"
		launchHome := runtimeHome
		if nativeHome {
			launchHome = ""
		}
		environment, err = subswapper.BuildClaudeProxyLaunchEnvironment(os.Environ(), launchHome, token, service.ProxyListen, metadata, service.ProxyEnvScrub)
	} else {
		environment, err = subswapper.BuildClaudeLaunchEnvironment(os.Environ(), runtimeHome, token, metadata, service.ProxyEnvScrub)
	}
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
	return runClaudeProcess(command, commandArgs, environment, token, stdin, stdout, stderr)
}

// runClaudeProcess runs Claude with secret redacted from everything it prints.
func runClaudeProcess(command string, commandArgs, environment []string, token string, stdin io.Reader, stdout, stderr io.Writer) error {
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
	if runErr != nil && cmd.Process == nil {
		return launchExecutable
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
		if cmd.Process == nil {
			return launchExecutable
		}
		if ctx.Err() != nil {
			return launchAuthTimeout
		}
		return launchAuthFailed
	}
	var status struct {
		LoggedIn    bool   `json:"loggedIn"`
		AuthMethod  string `json:"authMethod"`
		APIProvider string `json:"apiProvider"`
	}
	if err := json.Unmarshal(output.buffer.Bytes(), &status); err != nil ||
		!status.LoggedIn || status.AuthMethod != "oauth_token" || status.APIProvider != "firstParty" {
		return launchAuthUnusable
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
	// Behind the proxy the status line reflects whichever account served the
	// last request, so it cannot be attributed to the launch account.
	if sample, parseErr := subswapper.ParseClaudeStatusLine(input, observedAt, tokenRevision); parseErr == nil && os.Getenv("SUBSWAPPER_PROXY") != "1" {
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

// accountProcessEnvironment applies the home overrides and drops routing
// metadata inherited from an enclosing proxied session, plus CODEX_API_KEY,
// which would switch Codex away from the account's ChatGPT login.
func accountProcessEnvironment(base []string, overrides map[string]string) []string {
	result := make([]string, 0, len(base)+len(overrides))
	for _, entry := range base {
		key, _, _ := strings.Cut(entry, "=")
		if strings.HasPrefix(key, "SUBSWAPPER_") || key == "CODEX_API_KEY" {
			continue
		}
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
	asJSON := fs.Bool("json", false, "print a machine-readable report")
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
	notes := applyHubStatus(*cfg, cycle.Results)
	if *asJSON {
		encoder := json.NewEncoder(stdout)
		encoder.SetIndent("", "  ")
		return encoder.Encode(subswapper.BuildStatusReport(cycle.Results, time.Now().UTC()))
	}
	if _, err := io.WriteString(stdout, subswapper.RenderStatus(cycle.Results, nil, time.Now())); err != nil {
		return err
	}
	for _, note := range notes {
		if _, err := fmt.Fprintln(stdout, note); err != nil {
			return err
		}
	}
	return nil
}

// serviceAndAccount fills -service and -account from positional arguments,
// so `remove claude work` and `remove -service claude -account work` agree.
func serviceAndAccount(positional []string, serviceName, accountName *string) error {
	if len(positional) > 2 {
		return errors.New("too many arguments; expected <service> [account]")
	}
	if len(positional) >= 1 {
		*serviceName = positional[0]
	}
	if len(positional) == 2 {
		*accountName = positional[1]
	}
	return nil
}

func runRemove(args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("remove", flag.ContinueOnError)
	configPath := fs.String("config", defaultConfigPath, "config file")
	serviceName := fs.String("service", "", "service name")
	accountName := fs.String("account", "", "account name")
	force := fs.Bool("force", false, "remove even if this account is active")
	deleteHome := fs.Bool("delete-home", false, "also permanently delete an account home and its contents")
	positional, err := parseInterleaved(fs, args)
	if err != nil {
		return err
	}
	if err := serviceAndAccount(positional, serviceName, accountName); err != nil {
		return err
	}
	if *serviceName == "" || *accountName == "" {
		return errors.New("usage: subswapper remove <service> <account> [-force] [-delete-home]")
	}

	cfg, err := subswapper.LoadConfig(*configPath)
	if err != nil {
		return err
	}
	service, ok := cfg.Service(*serviceName)
	if !ok {
		return fmt.Errorf("service %q not found", *serviceName)
	}
	ctx, cancel := context.WithTimeout(context.Background(), accountCommandTimeout)
	defer cancel()
	if service.HubClient() {
		if *deleteHome {
			return errors.New("-delete-home is only available on the hub")
		}
		credential, err := hubCredential(*cfg, service)
		if err != nil {
			return err
		}
		if err := subswapper.HubRemoveAccount(ctx, service.HubURL, credential, *accountName, *force); err != nil {
			return err
		}
		_, err = fmt.Fprintf(stdout, "unregistered %s account %s on the hub\n", service.Name, *accountName)
		return err
	}
	if err := subswapper.UnregisterAccount(ctx, *cfg, service.Name, *accountName, *force, *deleteHome); err != nil {
		return err
	}
	action := "unregistered"
	if *deleteHome {
		action = "removed"
	}
	_, err = fmt.Fprintf(stdout, "%s %s account %s\n", action, service.Name, *accountName)
	return err
}

func runSwitch(args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("switch", flag.ContinueOnError)
	configPath := fs.String("config", defaultConfigPath, "config file")
	serviceName := fs.String("service", "", "service name, or all")
	accountName := fs.String("account", "auto", "account name, or auto")
	positional, err := parseInterleaved(fs, args)
	if err != nil {
		return err
	}
	if err := serviceAndAccount(positional, serviceName, accountName); err != nil {
		return err
	}
	if *serviceName == "" {
		return errors.New("usage: subswapper switch <service|all> [account|auto]")
	}

	cfg, err := subswapper.LoadConfig(*configPath)
	if err != nil {
		return err
	}
	if *serviceName == "all" && *accountName != "auto" {
		return errors.New("-service all requires -account auto")
	}
	ctx, cancel := context.WithTimeout(context.Background(), accountCommandTimeout)
	defer cancel()

	local := *serviceName
	for _, service := range cfg.Services {
		if !service.HubClient() || service.Disabled || (*serviceName != "all" && service.Name != *serviceName) {
			continue
		}
		credential, err := hubCredential(*cfg, service)
		if err != nil {
			return err
		}
		result, err := subswapper.HubSwitch(ctx, service.HubURL, credential, *accountName)
		if err != nil {
			return err
		}
		if _, err := fmt.Fprintf(stdout, "switched %s to %s on the hub\n", service.Name, result.Active); err != nil {
			return err
		}
		if service.Name == *serviceName {
			return nil
		}
	}
	if *accountName == "auto" {
		switches, err := subswapper.SwitchBest(ctx, *cfg, local)
		for _, event := range switches {
			if _, writeErr := fmt.Fprintf(stdout, "switched %s to %s\n", event.Service, event.Account); writeErr != nil {
				return errors.Join(err, writeErr)
			}
		}
		if err == nil && len(switches) == 0 && local != "all" {
			_, err = fmt.Fprintln(stdout, "already on the best account")
		}
		return err
	}
	if err := subswapper.SwitchAccount(*cfg, local, *accountName); err != nil {
		return err
	}
	_, err = fmt.Fprintf(stdout, "switched %s to %s\n", local, *accountName)
	return err
}

func runMonitor(args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("monitor", flag.ContinueOnError)
	configPath := fs.String("config", defaultConfigPath, "config file")
	interval := fs.Duration("interval", 0, "override monitor interval")
	once := fs.Bool("once", false, "run one monitor cycle")
	noAuto := fs.Bool("no-auto", false, "observe without switching")
	// Accepted so existing service units keep starting; warm-up was removed.
	_ = fs.Bool("no-warmup", false, "ignored")
	verbose := fs.Bool("verbose", false, "print the full status table every cycle")
	withProxy := fs.Bool("proxy", false, "also serve every auth proxy configured by proxy_listen")
	if err := fs.Parse(args); err != nil {
		return err
	}

	cfg, err := subswapper.LoadConfig(*configPath)
	if err != nil {
		return err
	}
	if *withProxy {
		proxyServices := configuredProxyServices(*cfg, "")
		if len(proxyServices) == 0 {
			return errors.New("monitor -proxy requires a Claude or Codex service with proxy_listen")
		}
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		ctx, cancel := context.WithCancelCause(ctx)
		defer cancel(nil)
		for _, proxyService := range proxyServices {
			proxy, err := newServiceProxy(*cfg, proxyService, proxyLogger(stdout))
			if err != nil {
				return err
			}
			go func() {
				if serveErr := proxy.Serve(ctx); serveErr != nil {
					cancel(serveErr)
					return
				}
				cancel(nil)
			}()
			if _, err := fmt.Fprintf(stdout, "subswapper %s proxy listening on %s\n", proxyService.Name, proxyService.ProxyListen); err != nil {
				return err
			}
			if err := printHubListen(stdout, proxyService); err != nil {
				return err
			}
		}
		monitorErr := runMonitorWithConfig(ctx, *cfg, *interval, *once, *noAuto, *verbose, stdout)
		cancel(nil)
		if cause := context.Cause(ctx); cause != nil && !errors.Is(cause, context.Canceled) {
			return cause
		}
		return monitorErr
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return runMonitorWithConfig(ctx, *cfg, *interval, *once, *noAuto, *verbose, stdout)
}

func runMonitorWithConfig(ctx context.Context, cfg subswapper.Config, interval time.Duration, once, noAuto, verbose bool, stdout io.Writer) error {
	monitorInterval := cfg.Monitor.Interval.Duration
	if interval > 0 {
		monitorInterval = interval
	}
	if monitorInterval <= 0 {
		monitorInterval = time.Minute
	}
	autoSwitch := cfg.Monitor.AutoSwitchEnabled() && !noAuto
	return runMonitorLoop(ctx, cfg, monitorInterval, once, autoSwitch, verbose, stdout, subswapper.MonitorOnce)
}

type serviceProxy interface {
	Serve(context.Context) error
}

func newServiceProxy(cfg subswapper.Config, service subswapper.ServiceConfig, logf func(string, ...any)) (serviceProxy, error) {
	switch {
	case service.HubRelayEnabled():
		return subswapper.NewHubRelay(service)
	case service.ClaudeProxyEnabled():
		proxy, err := subswapper.NewClaudeProxy(cfg, service.Name, logf)
		if err == nil {
			proxy.IdentityLookup = lookupClaudeSetupTokenIdentity
		}
		return proxy, err
	case service.CodexProxyEnabled():
		return subswapper.NewCodexProxy(cfg, service.Name, logf)
	}
	return nil, fmt.Errorf("service %q has no proxy_listen configured", service.Name)
}

func configuredProxyServices(cfg subswapper.Config, name string) []subswapper.ServiceConfig {
	var services []subswapper.ServiceConfig
	for _, service := range cfg.Services {
		if name != "" && service.Name != name {
			continue
		}
		if (service.ProxyEnabled() || service.HubRelayEnabled()) && !service.Disabled {
			services = append(services, service)
		}
	}
	return services
}

func proxyLogger(stdout io.Writer) func(string, ...any) {
	return func(format string, args ...any) {
		_, _ = fmt.Fprintf(stdout, format+"\n", args...)
	}
}

func runProxy(args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("proxy", flag.ContinueOnError)
	configPath := fs.String("config", defaultConfigPath, "config file")
	serviceName := fs.String("service", "", "serve only this service's proxy; default every configured proxy")
	listen := fs.String("listen", "", "override the configured proxy_listen address; requires -service")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, err := subswapper.LoadConfig(*configPath)
	if err != nil {
		return err
	}
	if *listen != "" {
		if *serviceName == "" {
			return errors.New("-listen requires -service")
		}
		if err := subswapper.ValidateClaudeProxyListen(*listen); err != nil {
			return fmt.Errorf("-listen: %w", err)
		}
		for index := range cfg.Services {
			if cfg.Services[index].Name == *serviceName {
				cfg.Services[index].ProxyListen = *listen
			}
		}
	}
	services := configuredProxyServices(*cfg, *serviceName)
	if len(services) == 0 {
		if *serviceName != "" {
			return fmt.Errorf("service %q has no proxy_listen configured; set it in the config or pass -listen", *serviceName)
		}
		return errors.New("no service has proxy_listen configured")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	for _, service := range services {
		proxy, err := newServiceProxy(*cfg, service, proxyLogger(stdout))
		if err != nil {
			return err
		}
		go func() {
			if serveErr := proxy.Serve(ctx); serveErr != nil {
				cancel(serveErr)
				return
			}
			cancel(nil)
		}()
		if _, err := fmt.Fprintf(stdout, "subswapper %s proxy listening on %s\n", service.Name, service.ProxyListen); err != nil {
			return err
		}
		if err := printHubListen(stdout, service); err != nil {
			return err
		}
	}
	<-ctx.Done()
	if cause := context.Cause(ctx); cause != nil && !errors.Is(cause, context.Canceled) {
		return cause
	}
	return nil
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
	_, err := fmt.Fprintln(w, "subswapper", subswapper.Version(), runtime.Version())
	return err
}

func printUsage(w io.Writer) error {
	_, err := fmt.Fprintln(w, `subswapper shares several Claude and ChatGPT subscriptions across Claude Code,
Codex, and your machines. All commands accept -config <path>.

Set up:
  subswapper setup local                 one machine
  subswapper setup hub [-enroll]         this machine holds the accounts for others
  subswapper setup client <hub>          use a hub's accounts from this machine
  subswapper doctor                      check the setup and print fixes

Accounts:
  subswapper add claude|codex <name> [-email label] [-device] [-paste]
  subswapper remove claude|codex <name> [-force] [-delete-home]
  subswapper switch claude|codex|all [name|auto]
  subswapper status [-json]

Run:
  subswapper claude [args...]            Claude Code through subswapper
  subswapper codex [args...]             Codex through subswapper
  subswapper home run -service claude|codex [-account <name>] [-- command args...]
  subswapper home path -service claude|codex [-account <name>]
  subswapper home proxy-auth -service codex
  subswapper delegate -service claude|codex -cwd /path -model MODEL -effort LEVEL -intent read-only|workspace-write (-task TEXT | -task-file PATH) [-timeout 10m]

Services and hub:
  subswapper monitor [-interval 5m] [-once] [-no-auto] [-verbose] [-proxy]
  subswapper proxy [-service claude] [-listen 127.0.0.1:7878]
  subswapper hub connect|export|import ...
  subswapper init
  subswapper version`)
	return err
}
