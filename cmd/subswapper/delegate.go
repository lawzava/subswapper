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
	taskFile := fs.String("task-file", "", "file containing the exact task instructions")
	timeout := fs.Duration("timeout", 10*time.Minute, "positive execution deadline")
	fast := fs.Bool("fast", false, "request Codex's fast service tier (codex only)")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			_, err := fmt.Fprintln(stdout, "Usage: subswapper delegate -service claude|codex -cwd /absolute/path -model MODEL -effort LEVEL -intent read-only|workspace-write (-task TEXT | -task-file PATH) [-fast] [-timeout 10m] [-config PATH] [-account NAME]")
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
	if *fast && *service != "codex" {
		// Claude fast mode bills extra usage; subscriptions do not cover it.
		return errors.New("-fast is Codex-only")
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
	var hasTask, hasTaskFile bool
	fs.Visit(func(f *flag.Flag) {
		switch f.Name {
		case "task":
			hasTask = true
		case "task-file":
			hasTaskFile = true
		}
	})
	if hasTask == hasTaskFile {
		return errors.New("delegate requires exactly one of -task or -task-file")
	}
	if hasTaskFile {
		content, err := readDelegateTaskFile(*taskFile)
		if err != nil {
			return err
		}
		*task = content
	}
	if strings.TrimSpace(*task) == "" || strings.ContainsRune(*task, 0) {
		return errors.New("delegate requires a nonempty task without NUL bytes")
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
	homeArgs = append(homeArgs, delegateProviderArgs(*service, *model, *effort, *intent, *fast)...)
	cmd := exec.Command(executable, homeArgs...)
	cmd.Dir = *cwd
	cmd.Env = append(os.Environ(), delegateMarker+"=1")
	cmd.Stdin = strings.NewReader(*task)
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	return executeDelegate(cmd, *timeout)
}

const maxDelegateTaskFileBytes = 16 << 20

func readDelegateTaskFile(path string) (string, error) {
	// Reject devices and pipes before reading: the delegation timeout starts
	// with the provider process, not while loading the caller's prompt.
	info, err := os.Stat(path)
	if err != nil {
		return "", errors.New("delegate task file is unavailable")
	}
	if !info.Mode().IsRegular() {
		return "", errors.New("delegate task file must be a regular file")
	}
	if info.Size() > maxDelegateTaskFileBytes {
		return "", errors.New("delegate task file exceeds 16 MiB")
	}
	file, err := openDelegateTaskFile(path)
	if err != nil {
		return "", errors.New("delegate task file is unavailable")
	}
	defer func() { _ = file.Close() }()
	info, err = file.Stat()
	if err != nil {
		return "", errors.New("delegate task file is unavailable")
	}
	if !info.Mode().IsRegular() {
		return "", errors.New("delegate task file must be a regular file")
	}
	data, err := io.ReadAll(io.LimitReader(file, maxDelegateTaskFileBytes+1))
	if err != nil {
		return "", errors.New("delegate task file could not be read")
	}
	if len(data) > maxDelegateTaskFileBytes {
		return "", errors.New("delegate task file exceeds 16 MiB")
	}
	return string(data), nil
}

func delegateProviderArgs(service, model, effort, intent string, fast bool) []string {
	if service == "codex" {
		// Keep every config override at root level. Mixing root and exec -c
		// options can discard the proxy settings in Codex 0.153.3.
		args := []string{
			"-c", "model_reasoning_effort=" + strconv.Quote(effort), "-c", `approval_policy="never"`,
			"-c", "features.hooks=false", "-c", "features.plugins=false", "-c", "features.apps=false", "-c", "mcp_servers={}",
			"-c", "sandbox_workspace_write.writable_roots=[]", "-c", "sandbox_workspace_write.network_access=false",
			"-c", "sandbox_workspace_write.exclude_tmpdir_env_var=true", "-c", "sandbox_workspace_write.exclude_slash_tmp=true",
		}
		if fast {
			// Codex's model catalog names the "priority" service tier "Fast".
			args = append(args, "-c", `service_tier="priority"`)
		}
		return append(args, "exec", "--model", model, "--sandbox", intent, "--ignore-user-config", "--ignore-rules", "--skip-git-repo-check",
			"--ephemeral", "--color", "never", "-")
	}
	tools, mode := "Read,Glob,Grep", "dontAsk"
	if intent == "workspace-write" {
		tools, mode = "Read,Glob,Grep,Edit,Write", "acceptEdits"
	}
	return []string{"--print", "--model", model, "--effort", effort, "--output-format", "text", "--no-session-persistence", "--restricted", "--strict-mcp-config", "--mcp-config", `{"mcpServers":{}}`, "--settings", `{"disableAllHooks":true}`, "--tools", tools, "--permission-mode", mode}
}
