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
	"time"

	"github.com/lawzava/subswapper/internal/subswapper"
	"golang.org/x/term"
)

const accountCommandTimeout = 2 * time.Minute

// parseInterleaved parses flags that may follow positional arguments, as in
// `subswapper add claude work -email me@example.com`.
func parseInterleaved(fs *flag.FlagSet, args []string) ([]string, error) {
	var positional []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		if fs.NArg() == 0 {
			return positional, nil
		}
		positional = append(positional, fs.Arg(0))
		args = fs.Args()[1:]
	}
}

// hubCredential is what a hub client presents for service.
func hubCredential(cfg subswapper.Config, service subswapper.ServiceConfig) (string, error) {
	if isClaudeServiceConfig(service) {
		return subswapper.LoadHubClaudeSecret(cfg, service.Name)
	}
	placeholder, err := subswapper.LoadHubCodexPlaceholder(cfg, service.Name)
	return placeholder.Token, err
}

func runAdd(args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("add", flag.ContinueOnError)
	fs.SetOutput(stderr)
	configPath := fs.String("config", defaultConfigPath, "config file")
	email := fs.String("email", "", "account label; Codex defaults to the login's email")
	device := fs.Bool("device", false, "Codex: sign in with a device code, for machines without a browser")
	paste := fs.Bool("paste", false, "Claude: paste an existing setup token instead of running claude setup-token")
	positional, err := parseInterleaved(fs, args)
	if err != nil {
		return err
	}
	if len(positional) != 2 {
		return errors.New("usage: subswapper add <claude|codex> <account> [-email label] [-device] [-paste]")
	}
	cfg, err := subswapper.LoadConfig(*configPath)
	if err != nil {
		return err
	}
	service, ok := cfg.Service(positional[0])
	if !ok {
		return fmt.Errorf("service %q not found", positional[0])
	}
	accountName := positional[1]
	ctx, cancel := context.WithTimeout(context.Background(), accountCommandTimeout)
	defer cancel()

	request := subswapper.HubAccountRequest{Account: accountName, Email: *email}
	if isClaudeServiceConfig(service) {
		request.SetupToken, err = obtainClaudeSetupToken(stdin, stdout, stderr, *paste)
	} else {
		request.AuthJSON, err = loginCodexOnce(*device, stdin, stdout, stderr)
		if request.Email == "" {
			request.Email = subswapper.CodexLoginEmail(request.AuthJSON)
		}
	}
	if err != nil {
		return err
	}

	where := ""
	var created bool
	if service.HubClient() {
		credential, err := hubCredential(*cfg, service)
		if err != nil {
			return err
		}
		result, err := subswapper.HubAddAccount(ctx, service.HubURL, credential, request)
		if err != nil {
			return err
		}
		created = result.Created
		where = " on the hub at " + service.HubURL
	} else if isClaudeServiceConfig(service) {
		created, err = subswapper.AddClaudeAccount(ctx, *cfg, service.Name, accountName, request.Email, request.SetupToken, lookupClaudeSetupTokenIdentity)
	} else {
		created, err = subswapper.AddCodexAccount(*cfg, service.Name, accountName, request.Email, request.AuthJSON)
	}
	if err != nil {
		return err
	}
	verb := "updated"
	if created {
		verb = "added"
	}
	label := ""
	if request.Email != "" {
		label = " (" + request.Email + ")"
	}
	_, err = fmt.Fprintf(stdout, "%s %s account %s%s%s\n", verb, service.Name, accountName, label, where)
	return err
}

// obtainClaudeSetupToken runs `claude setup-token` on a terminal so the user
// signs in, then reads the printed token through a hidden prompt. Piped
// input is read as the token directly.
func obtainClaudeSetupToken(stdin io.Reader, stdout, stderr io.Writer, paste bool) (string, error) {
	file, ok := stdin.(*os.File)
	if ok && term.IsTerminal(int(file.Fd())) && !paste {
		_, _ = fmt.Fprintln(stderr, "Running `claude setup-token`. Sign in in the browser; it prints a token that starts with sk-ant-oat01-.")
		cmd := exec.Command("claude", "setup-token")
		cmd.Stdin, cmd.Stdout, cmd.Stderr = stdin, stdout, stderr
		if err := cmd.Run(); err != nil {
			return "", fmt.Errorf("claude setup-token failed: %w", err)
		}
		_, _ = fmt.Fprintln(stderr, "Copy the token printed above.")
	}
	return readClaudeSetupToken(stdin, stderr)
}

// loginCodexOnce signs in to ChatGPT in a throwaway Codex home and returns
// the login file. The home is deleted afterwards, so the only copy is the
// one subswapper registers, and nothing else refreshes it.
func loginCodexOnce(device bool, stdin io.Reader, stdout, stderr io.Writer) ([]byte, error) {
	home, err := os.MkdirTemp("", "subswapper-codex-login-")
	if err != nil {
		return nil, err
	}
	defer func() { _ = os.RemoveAll(home) }()
	args := []string{"-c", `cli_auth_credentials_store="file"`, "login"}
	if device {
		args = append(args, "--device-auth")
	}
	cmd := exec.Command("codex", args...)
	cmd.Env = accountProcessEnvironment(os.Environ(), map[string]string{"CODEX_HOME": home})
	cmd.Stdin, cmd.Stdout, cmd.Stderr = stdin, stdout, stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("codex login failed: %w", err)
	}
	data, err := os.ReadFile(filepath.Join(home, "auth.json"))
	if err != nil {
		return nil, errors.New("codex login did not write a login file")
	}
	return data, nil
}

// runProviderShortcut launches a provider through `home run`:
// `subswapper claude [-config path] [--] [claude args...]`.
func runProviderShortcut(name string, args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	configPath := defaultConfigPath
	if len(args) >= 2 && (args[0] == "-config" || args[0] == "--config") {
		configPath = args[1]
		args = args[2:]
	}
	if len(args) > 0 && args[0] == "--" {
		args = args[1:]
	}
	homeArgs := append([]string{"run", "-config", configPath, "-service", name, "--", name}, args...)
	return runHome(homeArgs, stdin, stdout, stderr)
}
