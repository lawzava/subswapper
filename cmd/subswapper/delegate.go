package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/lawzava/subswapper/internal/subswapper"
)

// This marker deliberately avoids SUBSWAPPER_: provider launch environments
// replace that namespace with account metadata.
const delegateMarker = "DELEGATED_BY_SUBSWAPPER"

// processFailure carries a shell exit status without exposing command arguments.
type processFailure struct {
	code    int
	message string
}

func (e *processFailure) Error() string { return e.message }

func processExitCode(err error) int {
	var failure *processFailure
	if errors.As(err, &failure) {
		return failure.code
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		if code := exit.ExitCode(); code >= 0 {
			return code
		}
		return signalExitCode(exit)
	}
	return 1
}

func runDelegate(args []string, stdout, stderr io.Writer) error {
	if os.Getenv(delegateMarker) != "" {
		return errors.New("nested delegation is not supported")
	}
	fs := flag.NewFlagSet("delegate", flag.ContinueOnError)
	fs.SetOutput(io.Discard) // Parse errors may include task text or secrets.
	configDefault := os.Getenv("SUBSWAPPER_CONFIG_PATH")
	if configDefault == "" {
		configDefault = defaultConfigPath
	}
	config := fs.String("config", configDefault, "Subswapper config")
	service := fs.String("service", "", "claude or codex")
	account := fs.String("account", "", "selected account override")
	cwd := fs.String("cwd", "", "absolute working directory")
	model := fs.String("model", "", "explicit provider model")
	effort := fs.String("effort", "", "explicit provider effort")
	intent := fs.String("intent", "", "read-only or workspace-write")
	task := fs.String("task", "", "bounded task text")
	timeout := fs.Duration("timeout", 10*time.Minute, "positive execution deadline")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			_, err := fmt.Fprintln(stdout, "Usage: subswapper delegate -service claude|codex -cwd /absolute/path -model MODEL -effort LEVEL -intent read-only|workspace-write -task TEXT [-timeout 10m] [-config PATH] [-account NAME]")
			return err
		}
		return errors.New("invalid delegate options")
	}
	if len(fs.Args()) != 0 {
		return errors.New("delegate does not accept extra provider arguments")
	}
	if *service != "claude" && *service != "codex" {
		return errors.New("delegate requires -service claude or codex")
	}
	if !filepath.IsAbs(*cwd) {
		return errors.New("delegate requires an absolute -cwd")
	}
	info, err := os.Stat(*cwd)
	if err != nil || !info.IsDir() {
		return errors.New("delegate working directory is unavailable")
	}
	if !regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._:/-]*$`).MatchString(*model) {
		return errors.New("delegate requires a valid explicit -model")
	}
	validEffort := map[string]bool{"low": true, "medium": true, "high": true, "xhigh": true, "max": *service == "claude", "minimal": *service == "codex", "none": *service == "codex"}
	if !validEffort[*effort] {
		return errors.New("delegate requires a supported explicit -effort")
	}
	if *intent != "read-only" && *intent != "workspace-write" {
		return errors.New("delegate requires -intent read-only or workspace-write")
	}
	if strings.TrimSpace(*task) == "" || strings.ContainsRune(*task, 0) {
		return errors.New("delegate requires a nonempty -task")
	}
	if *timeout <= 0 {
		return errors.New("delegate requires a positive -timeout")
	}
	configPath, err := filepath.Abs(subswapper.ExpandPath(*config))
	if err != nil {
		return errors.New("delegate config path is unavailable")
	}
	cfg, err := subswapper.LoadConfig(configPath)
	if err != nil {
		return errors.New("delegate configuration is unavailable")
	}
	selected, ok := cfg.Service(*service)
	if !ok || (*service == "claude" && !isClaudeServiceConfig(selected)) || (*service == "codex" && !strings.EqualFold(selected.Kind, "codex")) {
		return errors.New("delegate service configuration does not match the provider")
	}
	executable, err := os.Executable()
	if err != nil {
		return errors.New("delegate launcher is unavailable")
	}
	homeArgs := []string{"home", "run", "-config", configPath, "-service", *service}
	if *account != "" {
		homeArgs = append(homeArgs, "-account", *account)
	}
	homeArgs = append(homeArgs, "--", *service)
	if *service == "codex" {
		// Untrusted project configuration is not loaded. CLI sandbox settings
		// still permit the explicitly requested workspace intent.
		homeArgs = append(homeArgs, "-c", "projects."+strconv.Quote(*cwd)+`.trust_level="untrusted"`)
	}
	homeArgs = append(homeArgs, delegateProviderArgs(*service, *model, *effort, *intent)...)
	cmd := exec.Command(executable, homeArgs...)
	cmd.Dir = *cwd
	cmd.Env = append(os.Environ(), delegateMarker+"=1")
	cmd.Stdin = strings.NewReader(*task)
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	return executeDelegate(cmd, *timeout)
}

func delegateProviderArgs(service, model, effort, intent string) []string {
	if service == "codex" {
		// Keep every config override at root level. Mixing root and exec -c
		// options can discard the proxy settings in Codex 0.153.3.
		return []string{
			"-c", "model_reasoning_effort=" + strconv.Quote(effort), "-c", `approval_policy="never"`,
			"-c", "features.hooks=false", "-c", "features.plugins=false", "-c", "features.apps=false", "-c", "mcp_servers={}",
			"-c", "sandbox_workspace_write.writable_roots=[]", "-c", "sandbox_workspace_write.network_access=false",
			"-c", "sandbox_workspace_write.exclude_tmpdir_env_var=true", "-c", "sandbox_workspace_write.exclude_slash_tmp=true",
			"exec", "--model", model, "--sandbox", intent, "--ignore-user-config", "--ignore-rules", "--skip-git-repo-check",
			"--ephemeral", "--color", "never", "-",
		}
	}
	tools, mode := "Read,Glob,Grep", "dontAsk"
	if intent == "workspace-write" {
		tools, mode = "Read,Glob,Grep,Edit,Write", "acceptEdits"
	}
	return []string{"--print", "--model", model, "--effort", effort, "--output-format", "text", "--no-session-persistence", "--restricted", "--strict-mcp-config", "--mcp-config", `{"mcpServers":{}}`, "--settings", `{"disableAllHooks":true}`, "--tools", tools, "--permission-mode", mode}
}
