package subswapper

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const (
	claudeOAuthBetaHeader = "oauth-2025-04-20"
)

var (
	claudeUsageURL   = "https://api.anthropic.com/api/oauth/usage"
	claudeProfileURL = "https://api.anthropic.com/api/oauth/profile"
	httpClient       = &http.Client{Timeout: 10 * time.Second}
)

type claudeUsageAPIResponse struct {
	FiveHour *struct {
		Utilization float64 `json:"utilization"`
		ResetsAt    string  `json:"resets_at"`
	} `json:"five_hour"`
	SevenDay *struct {
		Utilization float64 `json:"utilization"`
		ResetsAt    string  `json:"resets_at"`
	} `json:"seven_day"`
	Limits []claudeLimit `json:"limits"`
}

type claudeLimit struct {
	Kind     string  `json:"kind"`
	Group    string  `json:"group"`
	Percent  float64 `json:"percent"`
	ResetsAt string  `json:"resets_at"`
	Scope    *struct {
		Model *struct {
			DisplayName string `json:"display_name"`
		} `json:"model"`
	} `json:"scope"`
}

func fetchClaudeUsageWithSetupToken(ctx context.Context, token string) (UsageSnapshot, error) {
	if token == "" {
		return UsageSnapshot{}, errClaudeTokenMissing
	}
	usage, err := fetchClaudeUsageWithAccessToken(ctx, token, true)
	if err != nil {
		return UsageSnapshot{}, err
	}
	if !usage.HasCoreLimits() {
		return UsageSnapshot{}, errSetupTokenUsageUnavailable
	}
	now := usage.ObservedAt
	if now.IsZero() {
		now = time.Now().UTC()
	}
	if !validClaudeRoutingWindow(usage.FiveHour, now) || !validClaudeRoutingWindow(usage.Weekly, now) {
		return UsageSnapshot{}, errSetupTokenUsageUnavailable
	}
	if usage.FableWeekly.Pct != nil && !validClaudeRoutingWindow(usage.FableWeekly, now) {
		return UsageSnapshot{}, errSetupTokenUsageUnavailable
	}
	return usage, nil
}

func fetchClaudeUsageWithAccessToken(ctx context.Context, accessToken string, setupToken bool) (UsageSnapshot, error) {

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, claudeUsageURL, nil)
	if err != nil {
		return UsageSnapshot{}, err
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("anthropic-beta", claudeOAuthBetaHeader)
	req.Header.Set("User-Agent", "subswapper/1.0")

	resp, err := httpClient.Do(req)
	if err != nil {
		return UsageSnapshot{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusForbidden && setupToken {
		return UsageSnapshot{}, errSetupTokenUsageUnavailable
	}
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return UsageSnapshot{}, fmt.Errorf("%w (%s)", errClaudeUnauthorized, resp.Status)
	}
	if resp.StatusCode == http.StatusTooManyRequests {
		return UsageSnapshot{}, &rateLimitedError{
			retryAfter: parseRetryAfter(resp.Header.Get("Retry-After")),
			message:    fmt.Sprintf("Claude usage API returned %s", resp.Status),
		}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return UsageSnapshot{}, fmt.Errorf("claude usage API returned %s", resp.Status)
	}

	var raw claudeUsageAPIResponse
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		return UsageSnapshot{}, err
	}
	usage := convertClaudeUsage(raw)
	if !usage.HasLimits() {
		return UsageSnapshot{}, errors.New("claude usage API returned missing limits")
	}
	usage.ObservedAt = time.Now().UTC()
	return usage, nil
}

// lookupClaudeSetupTokenIdentity returns a provider-issued account UUID when
// the token has profile scope. Inference-only setup tokens normally return 403,
// which is a valid unknown-identity result rather than a label-derived guess.
func lookupClaudeSetupTokenIdentity(ctx context.Context, token string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, claudeProfileURL, nil)
	if err != nil {
		return "", nil
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("anthropic-beta", claudeOAuthBetaHeader)
	req.Header.Set("User-Agent", "subswapper/1.0")

	resp, err := httpClient.Do(req)
	if err != nil {
		return "", nil
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusUnauthorized {
		return "", errClaudeUnauthorized
	}
	if resp.StatusCode != http.StatusOK {
		return "", nil
	}
	var profile struct {
		Account struct {
			UUID        string `json:"uuid"`
			AccountUUID string `json:"account_uuid"`
		} `json:"account"`
	}
	if json.NewDecoder(resp.Body).Decode(&profile) != nil {
		return "", nil
	}
	if profile.Account.AccountUUID != "" {
		return profile.Account.AccountUUID, nil
	}
	return profile.Account.UUID, nil
}

// LookupClaudeSetupTokenIdentity returns only provider-issued identity data.
// An inference-only token normally has no profile scope, so identity is unknown.
func LookupClaudeSetupTokenIdentity(ctx context.Context, token string) (ClaudeSetupTokenIdentity, error) {
	accountUUID, err := lookupClaudeSetupTokenIdentity(ctx, token)
	if err != nil {
		return ClaudeSetupTokenIdentity{}, err
	}
	return ClaudeSetupTokenIdentity{AccountUUID: accountUUID}, nil
}

var (
	errClaudeUnauthorized         = errors.New("claude usage API unauthorized")
	errClaudeTokenMissing         = errors.New("claude OAuth access token missing")
	errSetupTokenUsageUnavailable = errors.New("setup-token usage unavailable")
)

func convertClaudeUsage(raw claudeUsageAPIResponse) UsageSnapshot {
	var usage UsageSnapshot
	if raw.FiveHour != nil {
		usage.FiveHour = LimitWindow{
			Pct:      PtrFloat64(raw.FiveHour.Utilization),
			ResetsAt: parseOptionalTime(raw.FiveHour.ResetsAt),
		}
	}
	if raw.SevenDay != nil {
		usage.Weekly = LimitWindow{
			Pct:      PtrFloat64(raw.SevenDay.Utilization),
			ResetsAt: parseOptionalTime(raw.SevenDay.ResetsAt),
		}
	}
	if fable, ok := claudeFableLimit(raw.Limits); ok {
		usage.FableWeekly = LimitWindow{
			Pct:      PtrFloat64(fable.Percent),
			ResetsAt: parseOptionalTime(fable.ResetsAt),
		}
	}
	return usage
}

func claudeFableLimit(limits []claudeLimit) (claudeLimit, bool) {
	var best claudeLimit
	for _, limit := range limits {
		if !isClaudeFableLimit(limit) {
			continue
		}
		if best.Scope == nil || limit.Percent > best.Percent {
			best = limit
		}
	}
	return best, best.Scope != nil
}

func isClaudeFableLimit(limit claudeLimit) bool {
	if limit.Kind != "weekly_scoped" || limit.Group != "weekly" {
		return false
	}
	if limit.Scope == nil || limit.Scope.Model == nil {
		return false
	}
	name := strings.ToLower(limit.Scope.Model.DisplayName)
	return strings.Contains(name, "fable")
}

func parseRetryAfter(value string) time.Duration {
	seconds, err := strconv.Atoi(strings.TrimSpace(value))
	if err != nil || seconds <= 0 {
		return 0
	}
	return time.Duration(seconds) * time.Second
}

func parseOptionalTime(value string) time.Time {
	if value == "" {
		return time.Time{}
	}
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return time.Time{}
	}
	return parsed
}
