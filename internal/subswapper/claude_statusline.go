package subswapper

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"time"
)

// ClaudeStatusLineSample binds rate-limit data to the setup-token revision
// that produced it. Callers must revalidate the revision before routing.
type ClaudeStatusLineSample struct {
	Usage         UsageSnapshot
	ObservedAt    time.Time
	TokenRevision string
}

type claudeStatusLinePayload struct {
	RateLimits *struct {
		FiveHour *claudeStatusLineWindow `json:"five_hour"`
		SevenDay *claudeStatusLineWindow `json:"seven_day"`
	} `json:"rate_limits"`
}

type claudeStatusLineWindow struct {
	UsedPercentage *float64 `json:"used_percentage"`
	ResetsAt       *int64   `json:"resets_at"`
}

type claudeSettingsSource struct {
	name string
	path string
}

// ParseClaudeStatusLine extracts the two generic subscription windows.
// It intentionally leaves model-specific limits, including Fable, unavailable.
func ParseClaudeStatusLine(input []byte, observedAt time.Time, tokenRevision string) (ClaudeStatusLineSample, error) {
	var payload claudeStatusLinePayload
	if err := json.Unmarshal(input, &payload); err != nil {
		return ClaudeStatusLineSample{}, errors.New("claude status-line usage is malformed")
	}
	if payload.RateLimits == nil || payload.RateLimits.FiveHour == nil || payload.RateLimits.SevenDay == nil {
		return ClaudeStatusLineSample{}, errors.New("claude status-line usage is unavailable")
	}
	if !validClaudeStatusLineWindow(*payload.RateLimits.FiveHour, observedAt) ||
		!validClaudeStatusLineWindow(*payload.RateLimits.SevenDay, observedAt) {
		return ClaudeStatusLineSample{}, errors.New("claude status-line usage is invalid")
	}

	fiveHour := *payload.RateLimits.FiveHour.UsedPercentage
	sevenDay := *payload.RateLimits.SevenDay.UsedPercentage
	usage := UsageSnapshot{
		FiveHour: LimitWindow{
			Pct:      PtrFloat64(fiveHour),
			ResetsAt: time.Unix(*payload.RateLimits.FiveHour.ResetsAt, 0).UTC(),
		},
		Weekly: LimitWindow{
			Pct:      PtrFloat64(sevenDay),
			ResetsAt: time.Unix(*payload.RateLimits.SevenDay.ResetsAt, 0).UTC(),
		},
		ObservedAt:    observedAt,
		Source:        claudeUsageSourceStatusLine,
		TokenRevision: tokenRevision,
	}
	return ClaudeStatusLineSample{
		Usage:         usage,
		ObservedAt:    observedAt,
		TokenRevision: tokenRevision,
	}, nil
}

func validClaudeStatusLineWindow(window claudeStatusLineWindow, observedAt time.Time) bool {
	if window.UsedPercentage == nil || window.ResetsAt == nil {
		return false
	}
	percentage := *window.UsedPercentage
	if math.IsNaN(percentage) || math.IsInf(percentage, 0) || percentage < 0 || percentage > 100 {
		return false
	}
	return *window.ResetsAt > observedAt.Unix()
}

// RoutingUsage returns usage only while its age, token revision, and reset
// times still match the caller's current routing context.
func (sample ClaudeStatusLineSample) RoutingUsage(now time.Time, tokenRevision string, maxAge time.Duration) (UsageSnapshot, bool) {
	if sample.TokenRevision == "" || tokenRevision == "" || sample.TokenRevision != tokenRevision {
		return UsageSnapshot{}, false
	}
	if maxAge <= 0 || sample.ObservedAt.IsZero() || sample.ObservedAt.After(now) || now.Sub(sample.ObservedAt) >= maxAge {
		return UsageSnapshot{}, false
	}
	if !sample.Usage.ObservedAt.Equal(sample.ObservedAt) ||
		sample.Usage.Source != claudeUsageSourceStatusLine ||
		sample.Usage.TokenRevision != tokenRevision ||
		!validClaudeRoutingWindow(sample.Usage.FiveHour, now) ||
		!validClaudeRoutingWindow(sample.Usage.Weekly, now) {
		return UsageSnapshot{}, false
	}
	usage := sample.Usage
	usage.FableWeekly = LimitWindow{}
	return usage, true
}

func validClaudeRoutingWindow(window LimitWindow, now time.Time) bool {
	if window.Pct == nil || window.ResetsAt.IsZero() || !window.ResetsAt.After(now) {
		return false
	}
	percentage := *window.Pct
	return !math.IsNaN(percentage) && !math.IsInf(percentage, 0) && percentage >= 0 && percentage <= 100
}

// ResolveClaudeStatusLineCommand reads Claude's effective status-line command
// without changing user or workspace settings. Later scopes take precedence.
func ResolveClaudeStatusLineCommand(accountHome, workspaceRoot string) (string, bool, error) {
	if accountHome == "" {
		return "", false, errors.New("claude account home is required")
	}
	sources := []claudeSettingsSource{
		{name: "user", path: filepath.Join(accountHome, "settings.json")},
	}
	if workspaceRoot != "" {
		sources = append(sources,
			claudeSettingsSource{name: "project", path: filepath.Join(workspaceRoot, ".claude", "settings.json")},
			claudeSettingsSource{name: "local", path: filepath.Join(workspaceRoot, ".claude", "settings.local.json")},
		)
	}

	var command string
	var found bool
	for _, source := range sources {
		present, candidate, candidateFound, err := readClaudeStatusLineCommand(source.path)
		if err != nil {
			return "", false, fmt.Errorf("read Claude %s settings: %w", source.name, err)
		}
		if present {
			command = candidate
			found = candidateFound
		}
	}
	return command, found, nil
}

func readClaudeStatusLineCommand(path string) (present bool, command string, found bool, err error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, "", false, nil
	}
	if err != nil {
		return false, "", false, errors.New("settings are unavailable")
	}
	var settings struct {
		StatusLine json.RawMessage `json:"statusLine"`
	}
	if err := json.Unmarshal(data, &settings); err != nil {
		return false, "", false, errors.New("settings are malformed")
	}
	if settings.StatusLine == nil {
		return false, "", false, nil
	}
	if string(settings.StatusLine) == "null" {
		return true, "", false, nil
	}
	var statusLine struct {
		Type    string  `json:"type"`
		Command *string `json:"command"`
	}
	if err := json.Unmarshal(settings.StatusLine, &statusLine); err != nil {
		return false, "", false, errors.New("status-line settings are malformed")
	}
	if statusLine.Command == nil || *statusLine.Command == "" || (statusLine.Type != "" && statusLine.Type != "command") {
		return true, "", false, nil
	}
	return true, *statusLine.Command, true, nil
}

// RunClaudeStatusLineCommand uses the platform shell, passes the original
// payload as stdin, and forwards only the command's stdout.
func RunClaudeStatusLineCommand(ctx context.Context, command string, input []byte, stdout io.Writer) error {
	if command == "" {
		return nil
	}
	if stdout == nil {
		stdout = io.Discard
	}

	var cmd *exec.Cmd
	if runtime.GOOS == "windows" {
		shell := os.Getenv("COMSPEC")
		if shell == "" {
			shell = "cmd.exe"
		}
		cmd = exec.CommandContext(ctx, shell, "/d", "/s", "/c", command)
	} else {
		cmd = exec.CommandContext(ctx, "/bin/sh", "-c", command)
	}
	cmd.Stdin = bytes.NewReader(input)
	cmd.Stdout = stdout
	cmd.Stderr = io.Discard
	cmd.Env = scrubClaudeSubprocessEnvironment(os.Environ())
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("claude status-line command failed: %w", err)
	}
	return nil
}
