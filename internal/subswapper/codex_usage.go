package subswapper

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

var codexCommand = "codex"

type codexAuthFile struct {
	AuthMode string       `json:"auth_mode"`
	Tokens   *codexTokens `json:"tokens"`
}

type codexTokens struct {
	AccessToken string `json:"access_token"`
}

type codexRateLimitsResponse struct {
	RateLimits          codexRateLimitSnapshot            `json:"rateLimits"`
	RateLimitsByLimitID map[string]codexRateLimitSnapshot `json:"rateLimitsByLimitId"`
}

type codexRateLimitSnapshot struct {
	Primary              *codexRateLimitWindow `json:"primary"`
	Secondary            *codexRateLimitWindow `json:"secondary"`
	RateLimitReachedType string                `json:"rateLimitReachedType"`
}

type codexRateLimitWindow struct {
	UsedPercent        int    `json:"usedPercent"`
	WindowDurationMins *int64 `json:"windowDurationMins"`
	ResetsAt           *int64 `json:"resetsAt"`
}

type codexRPCResponse struct {
	ID     any             `json:"id,omitempty"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *codexRPCError  `json:"error,omitempty"`
}

type codexRPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type codexRPCFailure struct {
	code    int
	message string
}

func (e *codexRPCFailure) Error() string {
	return fmt.Sprintf("codex app-server RPC error %d", e.code)
}

type codexAppServerError struct {
	cause  error
	stderr string
}

func (e *codexAppServerError) Error() string { return fmt.Sprintf("codex app-server: %v", e.cause) }
func (e *codexAppServerError) Unwrap() error { return e.cause }

func fetchCodexUsage(ctx context.Context, cfg Config, service ServiceConfig, account AccountState, active bool) (UsageSnapshot, error) {
	source, err := snapshotCredentialSource(ctx, cfg, service, account, active, func() (credentialSource, error) {
		return findCodexAuth(cfg, service, account, active)
	})
	if err != nil {
		return UsageSnapshot{}, err
	}
	if err := validateCodexAuth(source.data); err != nil {
		return UsageSnapshot{}, fmt.Errorf("%w: %v", errCredentialsInvalid, err)
	}

	codexHome, err := os.MkdirTemp("", "subswapper-codex-*")
	if err != nil {
		return UsageSnapshot{}, err
	}
	defer func() { _ = os.RemoveAll(codexHome) }()

	if err := writeFileAtomic(filepath.Join(codexHome, "auth.json"), source.data); err != nil {
		return UsageSnapshot{}, err
	}
	raw, err := readCodexRateLimits(ctx, codexHome)
	if err != nil {
		if isCodexRateLimitedError(err) {
			return UsageSnapshot{}, &rateLimitedError{message: err.Error()}
		}
		if isCodexAuthError(err) {
			return UsageSnapshot{}, fmt.Errorf("%w: %v", errCredentialsInvalid, err)
		}
		return UsageSnapshot{}, err
	}
	refreshed, readErr := os.ReadFile(filepath.Join(codexHome, "auth.json"))
	if readErr == nil && !bytes.Equal(refreshed, source.data) && validateCodexAuth(refreshed) == nil {
		// Only touch the live file when it was the source of these tokens;
		// never clobber a live login we did not read.
		if err := applyCredentialUpdate(ctx, cfg, service, account, source, refreshed, true); err != nil {
			return UsageSnapshot{}, err
		}
	}

	usage := convertCodexRateLimits(raw)
	if !usage.HasLimits() {
		return UsageSnapshot{}, errors.New("codex rate limits returned missing limits")
	}
	usage.ObservedAt = time.Now().UTC()
	return usage, nil
}

func findCodexAuth(cfg Config, service ServiceConfig, account AccountState, active bool) (credentialSource, error) {
	return findCredentialSource(cfg, service, account, active,
		func(data []byte) bool {
			var auth codexAuthFile
			if err := json.Unmarshal(data, &auth); err != nil {
				return false
			}
			return auth.Tokens != nil || auth.AuthMode != ""
		},
		"no managed file contains Codex auth",
		"read stored Codex auth")
}

func isCodexRateLimitedError(err error) bool {
	msg := strings.ToLower(codexDiagnosticText(err))
	return strings.Contains(msg, "429") || strings.Contains(msg, "too many requests")
}

// isCodexAuthError recognizes app-server failures caused by dead stored
// tokens (e.g. "failed to refresh token: 401 Unauthorized"), so the account
// is marked unselectable instead of surviving on its cached usage snapshot.
func isCodexAuthError(err error) bool {
	msg := strings.ToLower(codexDiagnosticText(err))
	for _, marker := range []string{"unauthorized", "401", "403", "refresh token", "not logged in", "invalid_grant"} {
		if strings.Contains(msg, marker) {
			return true
		}
	}
	return false
}

func codexDiagnosticText(err error) string {
	parts := []string{err.Error()}
	var appServerErr *codexAppServerError
	if errors.As(err, &appServerErr) && appServerErr.stderr != "" {
		parts = append(parts, appServerErr.stderr)
	}
	var rpcErr *codexRPCFailure
	if errors.As(err, &rpcErr) && rpcErr.message != "" {
		parts = append(parts, rpcErr.message)
	}
	return strings.Join(parts, " ")
}

func validateCodexAuth(data []byte) error {
	var auth codexAuthFile
	if err := json.Unmarshal(data, &auth); err != nil {
		return fmt.Errorf("parse Codex auth: %w", err)
	}
	if auth.AuthMode != "" && auth.AuthMode != "chatgpt" {
		return fmt.Errorf("codex auth mode %q has no subscription limits", auth.AuthMode)
	}
	if auth.Tokens == nil || auth.Tokens.AccessToken == "" {
		return errors.New("codex ChatGPT access token missing")
	}
	return nil
}

func readCodexRateLimits(ctx context.Context, codexHome string) (codexRateLimitsResponse, error) {
	cmd := exec.CommandContext(ctx, codexCommand, "app-server", "--stdio")
	cmd.Env = envWithOverride(os.Environ(), "CODEX_HOME", codexHome)
	// Bound Wait after cancellation even if a grandchild keeps the pipes open.
	cmd.WaitDelay = 10 * time.Second

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return codexRateLimitsResponse{}, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return codexRateLimitsResponse{}, err
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	if err := cmd.Start(); err != nil {
		return codexRateLimitsResponse{}, fmt.Errorf("start Codex app-server: %w", err)
	}
	reader := bufio.NewReader(stdout)
	// finish reaps the child before stderr is read, so the buffer is never
	// read while the exec copier goroutine can still write to it. Shutdown is
	// graceful (EOF on stdin) so the server can flush a refreshed auth.json;
	// the kill is only a bounded fallback for a wedged server.
	finished := false
	finish := func() string {
		if !finished {
			finished = true
			_ = stdin.Close()
			done := make(chan struct{})
			go func() {
				_ = cmd.Wait()
				close(done)
			}()
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				_ = cmd.Process.Kill()
				<-done
			}
		}
		return stderr.String()
	}
	defer finish()

	initialize := map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "initialize",
		"params": map[string]any{
			"clientInfo": map[string]string{
				"name":    "subswapper",
				"version": "dev",
			},
			"capabilities": map[string]bool{"experimentalApi": true},
		},
	}
	if err := writeCodexRPC(stdin, initialize); err != nil {
		return codexRateLimitsResponse{}, err
	}
	if _, err := readCodexRPCResponse(reader, 1); err != nil {
		return codexRateLimitsResponse{}, wrapCodexAppServerError(err, finish())
	}

	request := map[string]any{
		"jsonrpc": "2.0",
		"id":      2,
		"method":  "account/rateLimits/read",
		"params":  nil,
	}
	if err := writeCodexRPC(stdin, request); err != nil {
		return codexRateLimitsResponse{}, err
	}
	result, err := readCodexRPCResponse(reader, 2)
	if err != nil {
		return codexRateLimitsResponse{}, wrapCodexAppServerError(err, finish())
	}
	finish()

	var limits codexRateLimitsResponse
	if err := json.Unmarshal(result, &limits); err != nil {
		return codexRateLimitsResponse{}, fmt.Errorf("decode Codex rate limits: %w", err)
	}
	return limits, nil
}

func writeCodexRPC(w io.Writer, payload map[string]any) error {
	return json.NewEncoder(w).Encode(payload)
}

func readCodexRPCResponse(reader *bufio.Reader, id int) (json.RawMessage, error) {
	for {
		line, err := reader.ReadBytes('\n')
		if err != nil {
			return nil, err
		}
		line = bytes.TrimSpace(line)
		if !bytes.HasPrefix(line, []byte("{")) {
			continue
		}
		var response codexRPCResponse
		if err := json.Unmarshal(line, &response); err != nil {
			continue
		}
		if !codexRPCIDMatches(response.ID, id) {
			continue
		}
		if response.Error != nil {
			return nil, &codexRPCFailure{code: response.Error.Code, message: response.Error.Message}
		}
		if len(response.Result) == 0 {
			return nil, errors.New("codex app-server response missing result")
		}
		return response.Result, nil
	}
}

func codexRPCIDMatches(value any, want int) bool {
	switch typed := value.(type) {
	case float64:
		return int(typed) == want
	case string:
		parsed, err := strconv.Atoi(typed)
		return err == nil && parsed == want
	default:
		return false
	}
}

func wrapCodexAppServerError(err error, stderr string) error {
	return &codexAppServerError{cause: err, stderr: strings.TrimSpace(stderr)}
}

func envWithOverride(env []string, key, value string) []string {
	prefix := key + "="
	replacement := prefix + value
	overridden := false
	result := make([]string, 0, len(env)+1)
	for _, item := range env {
		if strings.HasPrefix(item, prefix) {
			if !overridden {
				result = append(result, replacement)
				overridden = true
			}
			continue
		}
		result = append(result, item)
	}
	if !overridden {
		result = append(result, replacement)
	}
	return result
}

func convertCodexRateLimits(raw codexRateLimitsResponse) UsageSnapshot {
	limits := raw.RateLimits
	if byID, ok := raw.RateLimitsByLimitID["codex"]; ok {
		limits = byID
	}

	var usage UsageSnapshot
	assignCodexWindow(&usage, limits.Primary, true)
	assignCodexWindow(&usage, limits.Secondary, false)
	if limits.RateLimitReachedType != "" {
		forceLimitReached(&usage.FiveHour)
		forceLimitReached(&usage.Weekly)
	}
	return usage
}

func assignCodexWindow(usage *UsageSnapshot, window *codexRateLimitWindow, primary bool) {
	if window == nil {
		return
	}
	converted := LimitWindow{
		Pct:      PtrFloat64(float64(window.UsedPercent)),
		ResetsAt: codexUnixTime(window.ResetsAt),
	}
	if window.WindowDurationMins != nil {
		switch *window.WindowDurationMins {
		case 300:
			usage.FiveHour = converted
			return
		case 10080:
			usage.Weekly = converted
			return
		}
	}
	if primary {
		usage.FiveHour = converted
		return
	}
	usage.Weekly = converted
}

func codexUnixTime(value *int64) time.Time {
	if value == nil || *value <= 0 {
		return time.Time{}
	}
	return time.Unix(*value, 0).UTC()
}

func forceLimitReached(window *LimitWindow) {
	ratio, ok := window.Ratio()
	if !ok || ratio < 1 {
		window.Pct = PtrFloat64(100)
		// A stale reset time would make Ratio read the forced value as 0;
		// the server's fresh "limit reached" signal wins over it.
		if !window.ResetsAt.IsZero() && !time.Now().Before(window.ResetsAt) {
			window.ResetsAt = time.Time{}
		}
	}
}
