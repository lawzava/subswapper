package subswapper

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

const (
	claudeOAuthClientID   = "9d1c250a-e61b-44d9-88ed-5944d1962f5e"
	claudeOAuthBetaHeader = "oauth-2025-04-20"
)

var (
	claudeUsageURL = "https://api.anthropic.com/api/oauth/usage"
	claudeTokenURL = "https://platform.claude.com/v1/oauth/token"
	httpClient     = &http.Client{Timeout: 10 * time.Second}
)

type claudeCredentials struct {
	ClaudeAiOauth *claudeOAuth `json:"claudeAiOauth"`
}

type claudeOAuth struct {
	AccessToken  string   `json:"accessToken"`
	RefreshToken string   `json:"refreshToken,omitempty"`
	ExpiresAt    int64    `json:"expiresAt,omitempty"`
	Scopes       []string `json:"scopes,omitempty"`
}

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

type claudeTokenResponse struct {
	AccessToken  string `json:"access_token"`
	ExpiresIn    int64  `json:"expires_in"`
	RefreshToken string `json:"refresh_token"`
	Scope        string `json:"scope"`
}

func fetchClaudeUsage(ctx context.Context, cfg Config, service ServiceConfig, account AccountState, active bool) (UsageSnapshot, error) {
	source, err := findClaudeCredentials(cfg, service, account, active)
	if err != nil {
		return UsageSnapshot{}, err
	}

	usage, err := fetchClaudeUsageWithCredentials(ctx, source.data)
	if err == nil {
		if source.fromLive && !backupMatches(source.backupPath, source.data) {
			// Opportunistically sync the live credentials into the backup so
			// tokens rotated by the running client are never lost on switch.
			if writeErr := writeFileAtomic(source.backupPath, source.data); writeErr != nil {
				return UsageSnapshot{}, writeErr
			}
		}
		return usage, nil
	}
	if !shouldRefreshClaudeCredentials(err, source.data) {
		if errors.Is(err, errClaudeUnauthorized) || errors.Is(err, errClaudeTokenMissing) {
			return UsageSnapshot{}, fmt.Errorf("%w: %v", errCredentialsInvalid, err)
		}
		return UsageSnapshot{}, err
	}

	refreshed, refreshErr := refreshClaudeCredentials(ctx, source.data)
	if refreshErr != nil {
		return UsageSnapshot{}, refreshErr
	}
	if err := writeFileAtomic(source.backupPath, refreshed); err != nil {
		return UsageSnapshot{}, err
	}
	if active {
		if err := writeFileAtomic(source.livePath, refreshed); err != nil {
			return UsageSnapshot{}, err
		}
	}
	usage, err = fetchClaudeUsageWithCredentials(ctx, refreshed)
	if errors.Is(err, errClaudeUnauthorized) {
		return UsageSnapshot{}, fmt.Errorf("%w: %v", errCredentialsInvalid, err)
	}
	return usage, err
}

func backupMatches(path string, data []byte) bool {
	existing, err := os.ReadFile(path)
	return err == nil && bytes.Equal(existing, data)
}

func findClaudeCredentials(cfg Config, service ServiceConfig, account AccountState, active bool) (credentialSource, error) {
	return findCredentialSource(cfg, service, account, active,
		func(data []byte) bool {
			_, err := parseClaudeOAuth(data)
			return err == nil
		},
		"no managed file contains Claude OAuth credentials",
		"read stored Claude credentials")
}

func fetchClaudeUsageWithCredentials(ctx context.Context, credentials []byte) (UsageSnapshot, error) {
	oauth, err := parseClaudeOAuth(credentials)
	if err != nil {
		return UsageSnapshot{}, err
	}
	if oauth.AccessToken == "" {
		return UsageSnapshot{}, errClaudeTokenMissing
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, claudeUsageURL, nil)
	if err != nil {
		return UsageSnapshot{}, err
	}
	req.Header.Set("Authorization", "Bearer "+oauth.AccessToken)
	req.Header.Set("anthropic-beta", claudeOAuthBetaHeader)
	req.Header.Set("User-Agent", "subswapper/1.0")

	resp, err := httpClient.Do(req)
	if err != nil {
		return UsageSnapshot{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return UsageSnapshot{}, fmt.Errorf("%w (%s)", errClaudeUnauthorized, resp.Status)
	}
	if resp.StatusCode == http.StatusTooManyRequests {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return UsageSnapshot{}, &rateLimitedError{
			retryAfter: parseRetryAfter(resp.Header.Get("Retry-After")),
			message:    fmt.Sprintf("Claude usage API returned %s: %s", resp.Status, strings.TrimSpace(string(body))),
		}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return UsageSnapshot{}, fmt.Errorf("Claude usage API returned %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}

	var raw claudeUsageAPIResponse
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		return UsageSnapshot{}, err
	}
	usage := convertClaudeUsage(raw)
	if !usage.HasLimits() {
		return UsageSnapshot{}, errors.New("Claude usage API returned missing limits")
	}
	usage.ObservedAt = time.Now().UTC()
	return usage, nil
}

var (
	errClaudeUnauthorized = errors.New("Claude usage API unauthorized")
	errClaudeTokenMissing = errors.New("Claude OAuth access token missing")
)

func shouldRefreshClaudeCredentials(err error, credentials []byte) bool {
	oauth, parseErr := parseClaudeOAuth(credentials)
	if parseErr != nil || oauth.RefreshToken == "" {
		return false
	}
	// A missing access token (e.g. captured mid-logout) is recoverable with
	// the refresh token, same as an expired or rejected one.
	return errors.Is(err, errClaudeUnauthorized) ||
		errors.Is(err, errClaudeTokenMissing) ||
		claudeCredentialsExpired(credentials)
}

func claudeCredentialsExpired(credentials []byte) bool {
	oauth, err := parseClaudeOAuth(credentials)
	if err != nil || oauth.ExpiresAt <= 0 {
		return false
	}
	return time.Now().Add(5*time.Minute).UnixMilli() >= oauth.ExpiresAt
}

func refreshClaudeCredentials(ctx context.Context, credentials []byte) ([]byte, error) {
	var data map[string]any
	if err := json.Unmarshal(credentials, &data); err != nil {
		return nil, err
	}
	oauthAny, ok := data["claudeAiOauth"].(map[string]any)
	if !ok {
		return nil, errors.New("Claude OAuth payload missing")
	}
	refreshToken, ok := oauthAny["refreshToken"].(string)
	if !ok || refreshToken == "" {
		return nil, errors.New("Claude OAuth refresh token missing")
	}

	body, err := json.Marshal(map[string]string{
		"grant_type":    "refresh_token",
		"refresh_token": refreshToken,
		"client_id":     claudeOAuthClientID,
	})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, claudeTokenURL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "subswapper/1.0")

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		refreshErr := fmt.Errorf("Claude token refresh returned %s: %s", resp.Status, strings.TrimSpace(string(body)))
		switch resp.StatusCode {
		case http.StatusBadRequest, http.StatusUnauthorized, http.StatusForbidden:
			return nil, fmt.Errorf("%w: %v", errCredentialsInvalid, refreshErr)
		case http.StatusTooManyRequests:
			return nil, &rateLimitedError{
				retryAfter: parseRetryAfter(resp.Header.Get("Retry-After")),
				message:    refreshErr.Error(),
			}
		}
		return nil, refreshErr
	}

	var token claudeTokenResponse
	if err := json.NewDecoder(resp.Body).Decode(&token); err != nil {
		return nil, err
	}
	if token.AccessToken == "" {
		return nil, errors.New("Claude token refresh response missing access token")
	}
	nowMS := time.Now().UnixMilli()
	oauthAny["accessToken"] = token.AccessToken
	oauthAny["expiresAt"] = nowMS + token.ExpiresIn*1000
	if token.RefreshToken != "" {
		oauthAny["refreshToken"] = token.RefreshToken
	}
	if token.Scope != "" {
		oauthAny["scopes"] = strings.Fields(token.Scope)
	}
	return json.Marshal(data)
}

func parseClaudeOAuth(credentials []byte) (claudeOAuth, error) {
	var decoded claudeCredentials
	if err := json.Unmarshal(credentials, &decoded); err != nil {
		return claudeOAuth{}, err
	}
	if decoded.ClaudeAiOauth == nil {
		return claudeOAuth{}, errors.New("Claude OAuth payload missing")
	}
	return *decoded.ClaudeAiOauth, nil
}

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
