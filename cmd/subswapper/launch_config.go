package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/lawzava/subswapper/internal/subswapper"
)

func runLaunchConfig(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("launch-config", flag.ContinueOnError)
	fs.SetOutput(stderr)
	configPath := fs.String("config", defaultConfigPath, "config file")
	serviceName := fs.String("service", "", "service to export")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *serviceName == "" || fs.NArg() != 0 {
		return errors.New("usage: subswapper launch-config -service claude|codex [-config path]")
	}
	cfg, err := subswapper.LoadConfig(*configPath)
	if err != nil {
		return err
	}
	if service, ok := cfg.Service(*serviceName); ok {
		if name, ok := subswapper.ProviderCLI(service); ok {
			if _, err := lookProviderBinary(name); err != nil {
				return err
			}
		}
	}
	executable, err := shimTarget()
	if err != nil {
		return err
	}
	launch, err := subswapper.ExportLaunchConfig(*cfg, *configPath, *serviceName, executable)
	if err != nil {
		return err
	}
	encoder := json.NewEncoder(stdout)
	encoder.SetIndent("", "  ")
	return encoder.Encode(launch)
}

// runProviderShim runs subswapper started through a link named after a
// provider CLI, the way `subswapper <name>` would. Paseo enables Codex
// features by running `<command> --version` without the provider's
// arguments, so that probe goes straight to the real CLI.
func runProviderShim(name string, args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	binary, err := lookProviderBinary(name)
	if err != nil {
		return err
	}
	if len(args) == 1 && args[0] == "--version" {
		cmd := exec.Command(binary, args...)
		cmd.Stdin = stdin
		cmd.Stdout = stdout
		cmd.Stderr = stderr
		return cmd.Run()
	}
	configPath := os.Getenv("SUBSWAPPER_CONFIG_PATH")
	if configPath == "" {
		configPath = defaultConfigPath
	}
	serviceName := name
	// Launches pass SUBSWAPPER_SERVICE to their children, so a Claude
	// session's value must not select the service for a nested codex.
	if requested := os.Getenv("SUBSWAPPER_SERVICE"); requested != "" && requested != name {
		if cfg, err := subswapper.LoadConfig(configPath); err == nil {
			if service, ok := cfg.Service(requested); ok {
				if cli, _ := subswapper.ProviderCLI(service); cli == name {
					serviceName = requested
				}
			}
		}
	}
	homeArgs := append([]string{"run", "-config", configPath, "-service", serviceName, "--", binary}, args...)
	return runHome(homeArgs, stdin, stdout, stderr)
}

// lookProviderBinary finds the provider CLI on PATH and skips subswapper
// itself, so a provider shim on PATH cannot launch itself.
func lookProviderBinary(name string) (string, error) {
	self, selfErr := os.Executable()
	var selfInfo os.FileInfo
	if selfErr == nil {
		selfInfo, selfErr = os.Stat(self)
	}
	for _, dir := range filepath.SplitList(os.Getenv("PATH")) {
		if !filepath.IsAbs(dir) {
			continue
		}
		candidate, err := exec.LookPath(filepath.Join(dir, name))
		if err != nil {
			continue
		}
		if selfErr == nil {
			if info, err := os.Stat(candidate); err == nil && os.SameFile(info, selfInfo) {
				continue
			}
		}
		return candidate, nil
	}
	return "", fmt.Errorf("%s is not on PATH; install it or add its directory to PATH", name)
}

// shimTarget prefers the path subswapper was started by over the resolved
// executable, so a link through a package manager's stable path keeps
// working after an upgrade.
func shimTarget() (string, error) {
	executable, err := os.Executable()
	if err != nil {
		return "", err
	}
	executableInfo, err := os.Stat(executable)
	if err != nil {
		return "", err
	}
	if invoked, err := exec.LookPath(os.Args[0]); err == nil {
		if invoked, err = filepath.Abs(invoked); err == nil {
			if info, err := os.Stat(invoked); err == nil && os.SameFile(info, executableInfo) {
				return invoked, nil
			}
		}
	}
	return executable, nil
}
