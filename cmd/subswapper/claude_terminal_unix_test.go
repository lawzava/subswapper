//go:build !windows

package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/creack/pty"
)

func TestRunHomeClaudeTerminalPreservesTTYAndRedactsOutput(t *testing.T) {
	dir := t.TempDir()
	configPath := writeHomeModeConfig(t, dir, "claude")
	createHomeAccount(t, configPath, "claude", "work")
	secret := "sk-ant-oat01-terminal-secret"
	storeTestSetupToken(t, configPath, "work", secret)
	fakeClaude := writeFakeClaude(t, dir, `
if [ "$1" = auth ] && [ "$2" = status ]; then
  printf '{"loggedIn":true,"authMethod":"oauth_token","apiProvider":"firstParty"}\n'
  exit 0
fi
stdin_tty="$(tty)"
stdin_tty="${stdin_tty#/dev/}"
controlling_tty="$(ps -o tty= -p $$ | tr -d ' ')"
if [ -t 0 ] && [ -t 1 ] && [ -t 2 ] && [ "$stdin_tty" = "$controlling_tty" ]; then
  printf 'tty=yes '
else
  printf 'tty=no(%s|%s) ' "$stdin_tty" "$controlling_tty"
fi
printf '%s' "$CLAUDE_CODE_OAUTH_TOKEN"
`)

	master, terminal, err := pty.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = master.Close() }()
	output := make(chan string, 1)
	go func() {
		var data bytes.Buffer
		buffer := make([]byte, 1024)
		for {
			count, readErr := master.Read(buffer)
			_, _ = data.Write(buffer[:count])
			if strings.Contains(data.String(), "[REDACTED]") || readErr != nil {
				output <- data.String()
				return
			}
		}
	}()

	err = runWithInput(
		[]string{"home", "run", "-config", configPath, "-service", "claude", "-account", "work", "--", fakeClaude},
		terminal,
		terminal,
		terminal,
	)
	_ = terminal.Close()
	if err != nil {
		t.Fatal(err)
	}
	got := <-output
	if !strings.Contains(got, "tty=yes") {
		t.Fatalf("Claude did not retain a terminal: %q", got)
	}
	if strings.Contains(got, secret) || !strings.Contains(got, "[REDACTED]") {
		t.Fatalf("terminal output was not redacted: %q", got)
	}
}
