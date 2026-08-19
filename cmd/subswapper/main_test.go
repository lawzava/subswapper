package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/lawzava/subswapper/internal/subswapper"
)

var errTestWriter = errors.New("test writer failed")

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, errTestWriter }

func TestRunPropagatesWriterFailure(t *testing.T) {
	if err := run([]string{"version"}, failingWriter{}, failingWriter{}); !errors.Is(err, errTestWriter) {
		t.Fatalf("version writer error = %v", err)
	}
	if err := run([]string{"help"}, failingWriter{}, failingWriter{}); !errors.Is(err, errTestWriter) {
		t.Fatalf("help writer error = %v", err)
	}
}

func TestRunHelpVersionAndMissingCommand(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if err := run([]string{"help"}, &stdout, &stderr); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout.String(), "Usage:") {
		t.Fatalf("help output:\n%s", stdout.String())
	}
	if !strings.Contains(stdout.String(), "home create") || !strings.Contains(stdout.String(), "home run") {
		t.Fatalf("help output lacks account-home commands:\n%s", stdout.String())
	}
	stdout.Reset()
	if err := run([]string{"version"}, &stdout, &stderr); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(stdout.String(), "subswapper ") {
		t.Fatalf("version output: %q", stdout.String())
	}
	if !strings.Contains(stdout.String(), runtime.Version()) {
		t.Fatalf("version output lacks Go toolchain %q: %q", runtime.Version(), stdout.String())
	}
	if err := run(nil, &stdout, &stderr); err == nil || !strings.Contains(err.Error(), "missing command") {
		t.Fatalf("missing command error = %v", err)
	}
}

func TestRunHomeCreatePathAndEnv(t *testing.T) {
	dir := t.TempDir()
	configPath := writeHomeModeConfig(t, dir, "claude")
	var stdout, stderr bytes.Buffer

	if err := run([]string{"home", "create", "-config", configPath, "-service", "claude", "-account", "work", "-email", "work@example.com"}, &stdout, &stderr); err != nil {
		t.Fatal(err)
	}
	wantHome := filepath.Join(dir, "accounts", "claude", "work")
	if !strings.Contains(stdout.String(), wantHome) {
		t.Fatalf("create output = %q, want home %q", stdout.String(), wantHome)
	}

	stdout.Reset()
	if err := run([]string{"home", "path", "-config", configPath, "-service", "claude", "-account", "work"}, &stdout, &stderr); err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(stdout.String()) != wantHome {
		t.Fatalf("path output = %q, want %q", stdout.String(), wantHome)
	}

	stdout.Reset()
	if err := run([]string{"home", "env", "-config", configPath, "-service", "claude", "-account", "work"}, &stdout, &stderr); err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(stdout.String()); got != "export CLAUDE_CONFIG_DIR='"+wantHome+"'" {
		t.Fatalf("env output = %q", got)
	}
}

func TestRunHomeCommandReceivesSelectedEnvironment(t *testing.T) {
	dir := t.TempDir()
	configPath := writeHomeModeConfig(t, dir, "codex")
	var stdout, stderr bytes.Buffer
	if err := run([]string{"home", "create", "-config", configPath, "-service", "codex", "-account", "personal"}, &stdout, &stderr); err != nil {
		t.Fatal(err)
	}
	stdout.Reset()

	if err := run([]string{"home", "run", "-config", configPath, "-service", "codex", "-account", "personal", "--", "sh", "-c", `printf %s "$CODEX_HOME"`}, &stdout, &stderr); err != nil {
		t.Fatal(err)
	}
	wantHome := filepath.Join(dir, "accounts", "codex", "personal")
	if stdout.String() != wantHome {
		t.Fatalf("run output = %q, want %q", stdout.String(), wantHome)
	}
}

func TestRemoveHomeAccountPreservesHomeUnlessDeleteRequested(t *testing.T) {
	for _, test := range []struct {
		name       string
		deleteHome bool
	}{
		{name: "preserve"},
		{name: "delete", deleteHome: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			configPath := writeHomeModeConfig(t, dir, "codex")
			var stdout, stderr bytes.Buffer
			if err := run([]string{"home", "create", "-config", configPath, "-service", "codex", "-account", "work"}, &stdout, &stderr); err != nil {
				t.Fatal(err)
			}
			home := filepath.Join(dir, "accounts", "codex", "work")
			marker := filepath.Join(home, "keep-me")
			if err := os.WriteFile(marker, []byte("state"), 0o600); err != nil {
				t.Fatal(err)
			}
			args := []string{"remove", "-config", configPath, "-service", "codex", "-account", "work", "-force"}
			if test.deleteHome {
				args = append(args, "-delete-home")
			}
			if err := run(args, &stdout, &stderr); err != nil {
				t.Fatal(err)
			}
			_, err := os.Stat(marker)
			if test.deleteHome && !os.IsNotExist(err) {
				t.Fatalf("home was not deleted: %v", err)
			}
			if !test.deleteHome && err != nil {
				t.Fatalf("home was not preserved: %v", err)
			}
		})
	}
}

func writeHomeModeConfig(t *testing.T, dir, kind string) string {
	t.Helper()
	cfg := subswapper.Config{
		BackupRoot: filepath.Join(dir, "accounts"),
		StatePath:  filepath.Join(dir, "state.json"),
		Services:   []subswapper.ServiceConfig{{Name: kind, Kind: kind}},
	}
	cfg.ApplyDefaults()
	path := filepath.Join(dir, "config.json")
	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestRunStatusWithCustomProbe(t *testing.T) {
	dir := t.TempDir()
	live := filepath.Join(dir, "active.json")
	if err := os.WriteFile(live, []byte("credential"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := subswapper.Config{
		BackupRoot: filepath.Join(dir, "backups"),
		StatePath:  filepath.Join(dir, "state.json"),
		Services: []subswapper.ServiceConfig{{
			Name:         "svc",
			Kind:         "custom",
			Files:        []subswapper.ManagedFile{{Path: live, BackupName: "auth.json"}},
			UsageCommand: []string{"sh", "-c", `echo '{"five_hour":{"pct":12},"weekly":{"pct":34}}'`},
		}},
	}
	cfg.ApplyDefaults()
	configPath := filepath.Join(dir, "config.json")
	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(configPath, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := subswapper.CaptureAccount(cfg, "svc", "main", ""); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	if err := run([]string{"status", "-config", configPath}, &stdout, &stderr); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout.String(), "svc") || !strings.Contains(stdout.String(), "34%") {
		t.Fatalf("status output:\n%s", stdout.String())
	}
}

func TestMonitorLoopDeduplicatesEvents(t *testing.T) {
	ready := statusResult("ready")
	failing := statusResult("ready (stale usage from Jul13 00:00: probe failed)")
	cycles := []subswapper.CycleResult{
		{Results: failing},
		{Results: failing},
		{Results: ready},
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	calls := 0
	runner := func(context.Context, subswapper.Config, bool) subswapper.CycleResult {
		cycle := cycles[calls]
		calls++
		if calls == len(cycles) {
			cancel()
		}
		return cycle
	}
	var out bytes.Buffer
	if err := runMonitorLoop(ctx, subswapper.Config{}, time.Millisecond, false, false, false, &out, runner); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	if strings.Count(got, "probe failed") != 1 {
		t.Fatalf("failure event count = %d, output:\n%s", strings.Count(got, "probe failed"), got)
	}
	if strings.Count(got, "recovered claude/a") != 1 {
		t.Fatalf("recovery event count = %d, output:\n%s", strings.Count(got, "recovered claude/a"), got)
	}
	if strings.Contains(got, "SERVICE") {
		t.Fatalf("continuous monitor printed a table:\n%s", got)
	}
}

func TestMonitorLoopVerboseAndOnceRenderTables(t *testing.T) {
	for _, test := range []struct {
		name    string
		once    bool
		verbose bool
	}{
		{name: "once", once: true},
		{name: "verbose", verbose: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			runner := func(context.Context, subswapper.Config, bool) subswapper.CycleResult {
				cancel()
				return subswapper.CycleResult{Results: statusResult("ready")}
			}
			var out bytes.Buffer
			if err := runMonitorLoop(ctx, subswapper.Config{}, time.Millisecond, test.once, false, test.verbose, &out, runner); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(out.String(), "SERVICE") {
				t.Fatalf("missing table:\n%s", out.String())
			}
		})
	}
}

func TestMonitorLoopVerboseReportsCycleErrors(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runner := func(context.Context, subswapper.Config, bool) subswapper.CycleResult {
		cancel()
		return subswapper.CycleResult{
			Results: statusResult("ready"),
			Errors:  []error{errors.New("state save failed")},
		}
	}
	var out bytes.Buffer
	if err := runMonitorLoop(ctx, subswapper.Config{}, time.Millisecond, false, false, true, &out, runner); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "state save failed") {
		t.Fatalf("verbose monitor hid cycle error:\n%s", out.String())
	}
}

func statusResult(reason string) []subswapper.ServiceStatus {
	return []subswapper.ServiceStatus{{
		Service: subswapper.ServiceConfig{Name: "claude"},
		Accounts: []subswapper.AccountStatus{{
			Service:    "claude",
			Account:    subswapper.AccountState{Name: "a"},
			Selectable: true,
			Reason:     reason,
		}},
	}}
}
