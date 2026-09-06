//go:build !windows

package main

import (
	"bytes"
	"errors"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/lawzava/subswapper/internal/subswapper"
)

func TestDelegateProcess(t *testing.T) {
	binary := filepath.Join(t.TempDir(), "subswapper")
	build := exec.Command("go", "build", "-o", binary, ".")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %v: %s", err, out)
	}
	for _, provider := range []string{"claude", "codex"} {
		t.Run(provider, func(t *testing.T) {
			// Exercise equivalent path spellings, including macOS /var aliases.
			realDir := t.TempDir()
			dir := filepath.Join(t.TempDir(), "workspace-link")
			if err := os.Symlink(realDir, dir); err != nil {
				t.Fatal(err)
			}
			server := httptest.NewUnstartedServer(nil)
			listen := server.Listener.Addr().String()
			var config string
			if provider == "claude" {
				config = writeProxyHomeConfig(t, dir, listen)
			} else {
				config = writeCodexProxyHomeConfig(t, dir, listen)
			}
			createHomeAccount(t, config, provider, "test")
			cfg, err := subswapper.LoadConfig(config)
			if err != nil {
				t.Fatal(err)
			}
			if provider == "claude" {
				storeTestSetupToken(t, config, "test", "synthetic-account-secret")
				proxy, err := subswapper.NewClaudeProxy(*cfg, provider, nil)
				if err != nil {
					t.Fatal(err)
				}
				server.Config.Handler = proxy
			} else {
				proxy, err := subswapper.NewCodexProxy(*cfg, provider, nil)
				if err != nil {
					t.Fatal(err)
				}
				server.Config.Handler = proxy
			}
			server.Start()
			defer server.Close()
			binDir := filepath.Join(dir, "bin")
			if err := os.MkdirAll(binDir, 0700); err != nil {
				t.Fatal(err)
			}
			fake := filepath.Join(binDir, provider)
			script := `#!/bin/sh
if [ "$1" = auth ]; then
 printf '{"loggedIn":true,"authMethod":"oauth_token","apiProvider":"firstParty"}\n'
 exit 0
fi
printf 'cwd=%s\nproxy=%s\nbase=%s\nmarker=%s\n' "$PWD" "$SUBSWAPPER_PROXY" "$ANTHROPIC_BASE_URL" "$DELEGATED_BY_SUBSWAPPER"
printf 'arg=<%s>\n' "$@"
printf 'task='; cat
printf '\nchild stderr\n' >&2
exit 23
`
			if err := os.WriteFile(fake, []byte(script), 0700); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
			task := "Synthetic task: 'quotes' $(never-run)\nsecond line"
			base := []string{"delegate", "-config", config, "-service", provider, "-cwd", dir, "-model", "synthetic-model", "-effort", "high", "-intent", "read-only", "-timeout", "10s", "-task", task}
			cmd := exec.Command(binary, base...)
			var out, stderr bytes.Buffer
			cmd.Stdout = &out
			cmd.Stderr = &stderr
			err = cmd.Run()
			var exit *exec.ExitError
			if !errors.As(err, &exit) || exit.ExitCode() != 23 {
				t.Fatalf("want exit 23, got %v; %s", err, stderr.String())
			}
			cwdLine, _, _ := strings.Cut(out.String(), "\n")
			actualDir, err := os.Stat(strings.TrimPrefix(cwdLine, "cwd="))
			if err != nil {
				t.Fatalf("reported working directory: %v", err)
			}
			expectedDir, err := os.Stat(dir)
			if err != nil {
				t.Fatal(err)
			}
			if !os.SameFile(actualDir, expectedDir) {
				t.Fatalf("wrong working directory: %s", cwdLine)
			}
			for _, want := range []string{"proxy=1", "marker=1", "arg=<synthetic-model>", "task=" + task} {
				if !strings.Contains(out.String(), want) {
					t.Errorf("missing %q in %s", want, out.String())
				}
			}
			if !strings.Contains(stderr.String(), "child stderr") {
				t.Error("stderr not forwarded")
			}
			if provider == "codex" {
				for _, want := range []string{"model_provider=subswapper", "supports_websockets=false", "arg=<read-only>", "model_reasoning_effort=\"high\"", "approval_policy=\"never\"", "trust_level=\"untrusted\"", "arg=<--skip-git-repo-check>"} {
					if !strings.Contains(out.String(), want) {
						t.Errorf("missing %s", want)
					}
				}
			} else {
				for _, want := range []string{"base=http://" + listen, "arg=<Read,Glob,Grep>", "arg=<--restricted>", "arg=<--strict-mcp-config>"} {
					if !strings.Contains(out.String(), want) {
						t.Errorf("missing %s", want)
					}
				}
			}
			t.Run("write-intent", func(t *testing.T) {
				args := append([]string(nil), base...)
				for i := range args {
					if args[i] == "read-only" {
						args[i] = "workspace-write"
					}
				}
				got, _ := exec.Command(binary, args...).CombinedOutput()
				want := "arg=<workspace-write>"
				if provider == "claude" {
					want = "arg=<Read,Glob,Grep,Edit,Write>"
				}
				if !strings.Contains(string(got), want) {
					t.Fatalf("missing write policy: %s", got)
				}
			})
			t.Run("no-recursion", func(t *testing.T) {
				cmd := exec.Command(binary, base...)
				cmd.Env = append(os.Environ(), "DELEGATED_BY_SUBSWAPPER=1")
				got, err := cmd.CombinedOutput()
				if err == nil || !strings.Contains(string(got), "nested delegation") {
					t.Fatalf("nested launch: %v %s", err, got)
				}
			})
			t.Run("safe-errors", func(t *testing.T) {
				bad := filepath.Join(dir, "invalid.json")
				if err := os.WriteFile(bad, []byte(`{"secret":"SYNTHETIC-PRIVATE", broken`), 0600); err != nil {
					t.Fatal(err)
				}
				args := append([]string(nil), base...)
				args[2] = bad
				got, err := exec.Command(binary, args...).CombinedOutput()
				if err == nil || strings.Contains(string(got), "SYNTHETIC-PRIVATE") || strings.Contains(string(got), bad) {
					t.Fatalf("unsafe error: %v %s", err, got)
				}
			})
			t.Run("home-error-is-safe", func(t *testing.T) {
				args := append(append([]string(nil), base...), "-account", "SYNTHETIC-PRIVATE-ACCOUNT")
				got, err := exec.Command(binary, args...).CombinedOutput()
				if err == nil || strings.Contains(string(got), "SYNTHETIC-PRIVATE") || !strings.Contains(string(got), "delegated provider launch failed") {
					t.Fatalf("unsafe home error: %v %s", err, got)
				}
			})
			for _, result := range []struct {
				name, command string
				code          int
			}{
				{"success", "exit 0", 0}, {"signal-exit", "kill -HUP $$", 129},
			} {
				t.Run(result.name, func(t *testing.T) {
					prefix := "#!/bin/sh\nif [ \"$1\" = auth ]; then printf '{\"loggedIn\":true,\"authMethod\":\"oauth_token\",\"apiProvider\":\"firstParty\"}'; exit 0; fi\n"
					if err := os.WriteFile(fake, []byte(prefix+result.command+"\n"), 0700); err != nil {
						t.Fatal(err)
					}
					got, err := exec.Command(binary, base...).CombinedOutput()
					if result.code == 0 {
						if err != nil {
							t.Fatalf("success: %v %s", err, got)
						}
						return
					}
					var exit *exec.ExitError
					if !errors.As(err, &exit) || exit.ExitCode() != result.code {
						t.Fatalf("want %d: %v %s", result.code, err, got)
					}
				})
			}
			for _, signal := range []syscall.Signal{0, syscall.SIGINT, syscall.SIGTERM} {
				t.Run("cancel-"+signal.String(), func(t *testing.T) {
					ready := filepath.Join(dir, "ready")
					script := "#!/bin/sh\nif [ \"$1\" = auth ]; then printf '{\"loggedIn\":true,\"authMethod\":\"oauth_token\",\"apiProvider\":\"firstParty\"}'; exit 0; fi\ntrap '' INT TERM\n(sleep 2; echo leaked > escaped) &\necho ready > ready\nwait\n"
					if err := os.WriteFile(fake, []byte(script), 0700); err != nil {
						t.Fatal(err)
					}
					args := append([]string(nil), base...)
					if signal == 0 {
						for i := range args {
							if args[i] == "10s" {
								args[i] = "500ms"
							}
						}
					}
					cmd := exec.Command(binary, args...)
					var output bytes.Buffer
					cmd.Stdout = &output
					cmd.Stderr = &output
					if err := cmd.Start(); err != nil {
						t.Fatal(err)
					}
					done := make(chan error, 1)
					go func() { done <- cmd.Wait() }()
					if signal != 0 {
						deadline := time.Now().Add(5 * time.Second)
						for {
							if _, err := os.Stat(ready); err == nil {
								break
							}
							if time.Now().After(deadline) {
								_ = cmd.Process.Kill()
								t.Fatal("child never ready")
							}
							time.Sleep(10 * time.Millisecond)
						}
						if err := cmd.Process.Signal(signal); err != nil {
							t.Fatal(err)
						}
					}
					select {
					case err := <-done:
						want := 124
						if signal != 0 {
							want = 128 + int(signal)
						}
						var exit *exec.ExitError
						if !errors.As(err, &exit) || exit.ExitCode() != want {
							t.Fatalf("want %d got %v %s", want, err, output.String())
						}
					case <-time.After(5 * time.Second):
						_ = cmd.Process.Kill()
						t.Fatal("cancellation hung")
					}
					time.Sleep(2100 * time.Millisecond)
					if _, err := os.Stat(filepath.Join(dir, "escaped")); !os.IsNotExist(err) {
						t.Fatal("descendant survived cancellation")
					}
					_ = os.Remove(ready)
				})
			}
		})
	}
}

func TestDelegateRejectsUnsafeArguments(t *testing.T) {
	base := []string{"delegate", "-service", "codex", "-cwd", t.TempDir(), "-model", "synthetic", "-effort", "high", "-intent", "read-only", "-task", "synthetic"}
	for _, extra := range [][]string{{"--", "--dangerously-bypass-approvals-and-sandbox"}, {"-intent", "all"}, {"-timeout", "0"}, {"-effort", "injected\nvalue"}, {"-model", "--injected"}} {
		var out bytes.Buffer
		err := runWithInput(append(append([]string(nil), base...), extra...), strings.NewReader(""), &out, &out)
		if err == nil {
			t.Errorf("accepted %v", extra)
		}
	}
}

func TestDelegateCodexPolicy(t *testing.T) {
	args := strings.Join(delegateProviderArgs("codex", "synthetic", "high", "workspace-write"), "\n")
	for _, want := range []string{`features.hooks=false`, `features.plugins=false`, `features.apps=false`, `mcp_servers={}`, `sandbox_workspace_write.writable_roots=[]`, `sandbox_workspace_write.network_access=false`, `sandbox_workspace_write.exclude_tmpdir_env_var=true`, `sandbox_workspace_write.exclude_slash_tmp=true`} {
		if !strings.Contains(args, want) {
			t.Errorf("missing policy %s", want)
		}
	}
}

func TestDelegateCodexKeepsConfigBeforeExec(t *testing.T) {
	args := delegateProviderArgs("codex", "synthetic", "high", "read-only")
	inExec := false
	for _, arg := range args {
		if arg == "exec" {
			inExec = true
		}
		if inExec && arg == "-c" {
			t.Fatal("exec-level config can discard the root proxy overrides in Codex 0.153.3")
		}
	}
	if !inExec {
		t.Fatal("missing exec subcommand")
	}
}
