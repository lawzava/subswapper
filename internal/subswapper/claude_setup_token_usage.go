package subswapper

import (
	"context"
	"errors"
	"time"
)

// collectClaudeSetupTokenAccount uses a setup token only for a new usage
// probe. It never refreshes the token or changes a running Claude process.
func collectClaudeSetupTokenAccount(ctx context.Context, cfg Config, serviceState *ServiceState, status *AccountStatus, service ServiceConfig) {
	now := time.Now().UTC()
	token, tokenStatus, err := LoadClaudeSetupTokenWithStatus(cfg, service.Name, status.Account.Name)
	if err != nil {
		status.Account.Usage = UsageSnapshot{}
		serviceState.Accounts[status.Account.Name] = status.Account
		switch {
		case errors.Is(err, ErrClaudeSetupTokenNotConfigured):
			status.Reason = "setup token missing"
		case errors.Is(err, ErrClaudeSetupTokenExpired):
			status.Reason = "setup token expired"
		default:
			status.Reason = "setup token unusable"
		}
		return
	}
	if tokenStatus.Revision == "" || tokenStatus.Revision != status.Account.SetupTokenRevision {
		status.Account.Usage = UsageSnapshot{}
		serviceState.Accounts[status.Account.Name] = status.Account
		status.Reason = "setup token changed during usage probe"
		return
	}
	if !status.Active && trustedClaudeSetupTokenUsage(status.Account.Usage, now, tokenStatus.Revision) &&
		(status.Account.Usage.Source == claudeUsageSourceSetupTokenDirect ||
			status.Account.Usage.Source == claudeUsageSourceCommand || status.Account.LastProbeError != "") {
		markClaudeSetupTokenSelectable(status)
		return
	}

	if now.Before(status.Account.FetchBackoffUntil) {
		if status.Account.CredentialsError != "" {
			status.Reason = status.Account.CredentialsError
			return
		}
		if trustedClaudeStatusLineUsage(status.Account.Usage, now, tokenStatus.Revision) {
			markClaudeSetupTokenSelectable(status)
			return
		}
		status.Account.Usage = UsageSnapshot{}
		serviceState.Accounts[status.Account.Name] = status.Account
		status.Reason = "usage unavailable"
		return
	}

	usage, fetchErr := fetchClaudeUsageWithSetupToken(ctx, token)
	if errors.Is(fetchErr, errClaudeUnauthorized) || errors.Is(fetchErr, errClaudeTokenMissing) {
		status.Account.Usage = UsageSnapshot{}
		status.Account.FetchBackoffUntil = now.Add(credentialsErrorBackoff)
		status.Account.CredentialsError = "setup token authentication rejected"
		status.Account.LastProbeError = ""
		serviceState.Accounts[status.Account.Name] = status.Account
		status.Reason = status.Account.CredentialsError
		return
	}
	if len(service.UsageCommand) > 0 {
		commandUsage, commandErr := runUsageCommand(ctx, cfg, service, status.Account)
		if commandErr != nil {
			status.Account.Usage = UsageSnapshot{}
			status.Account.FetchBackoffUntil = now.Add(transientProbeBackoff(cfg.Monitor.Interval.Duration))
			status.Account.CredentialsError = ""
			status.Account.LastProbeError = "usage command unavailable"
			serviceState.Accounts[status.Account.Name] = status.Account
			status.Reason = "usage unavailable"
			return
		}
		commandUsage.Source = claudeUsageSourceCommand
		commandUsage.TokenRevision = tokenStatus.Revision
		status.Account.Usage = commandUsage
		status.Account.FetchBackoffUntil = time.Time{}
		status.Account.CredentialsError = ""
		status.Account.LastProbeError = ""
		serviceState.Accounts[status.Account.Name] = status.Account
		markClaudeSetupTokenSelectable(status)
		return
	}
	if fetchErr == nil {
		usage.Source = claudeUsageSourceSetupTokenDirect
		usage.TokenRevision = tokenStatus.Revision
		if usage.ObservedAt.IsZero() {
			usage.ObservedAt = now
		}
		status.Account.Usage = usage
		status.Account.FetchBackoffUntil = time.Time{}
		status.Account.CredentialsError = ""
		status.Account.LastProbeError = ""
		serviceState.Accounts[status.Account.Name] = status.Account
		markClaudeSetupTokenSelectable(status)
		return
	}

	// A status-line sample is trusted only while it is fresh and bound to the
	// same random token revision. Provider errors are reduced to a fixed label.
	if trustedClaudeStatusLineUsage(status.Account.Usage, now, tokenStatus.Revision) {
		status.Account.FetchBackoffUntil = now.Add(transientProbeBackoff(cfg.Monitor.Interval.Duration))
		status.Account.CredentialsError = ""
		status.Account.LastProbeError = "setup-token usage unavailable; using fresh Claude status-line data"
		serviceState.Accounts[status.Account.Name] = status.Account
		markClaudeSetupTokenSelectable(status)
		return
	}

	status.Account.FetchBackoffUntil = now.Add(transientProbeBackoff(cfg.Monitor.Interval.Duration))
	status.Account.Usage = UsageSnapshot{}
	status.Account.CredentialsError = ""
	status.Account.LastProbeError = "setup-token usage unavailable"
	serviceState.Accounts[status.Account.Name] = status.Account
	status.Reason = "usage unavailable"
}

func trustedClaudeSetupTokenUsage(usage UsageSnapshot, now time.Time, tokenRevision string) bool {
	if usage.TokenRevision == "" || usage.TokenRevision != tokenRevision ||
		usage.ObservedAt.IsZero() || usage.ObservedAt.After(now) ||
		now.Sub(usage.ObservedAt) >= inactiveUsageTTL || !usage.HasCoreLimits() {
		return false
	}
	return validClaudeRoutingWindow(usage.FiveHour, now) && validClaudeRoutingWindow(usage.Weekly, now)
}

func trustedClaudeStatusLineUsage(usage UsageSnapshot, now time.Time, tokenRevision string) bool {
	return usage.Source == claudeUsageSourceStatusLine && trustedClaudeSetupTokenUsage(usage, now, tokenRevision)
}

func markClaudeSetupTokenSelectable(status *AccountStatus) {
	if !status.Account.Usage.HasCoreLimits() {
		status.Reason = "usage unavailable"
		return
	}
	if status.Account.Usage.Exhausted() {
		status.Reason = "limit reached"
		return
	}
	status.Selectable = true
	status.Score = status.Account.Usage.Score()
	status.Reason = "ready"
}

// RecordClaudeStatusLineUsage stores only fresh, complete rate limits. The
// random revision prevents a delayed process from updating another token.
func RecordClaudeStatusLineUsage(ctx context.Context, cfg Config, serviceName, accountName string, sample ClaudeStatusLineSample) error {
	lock, err := AcquireStateLock(ctx, cfg)
	if err != nil {
		return err
	}
	defer lock.Release()
	state, err := LoadState(cfg.StatePath)
	if err != nil {
		return err
	}
	account, err := claudeSetupTokenAccount(cfg, state, serviceName, accountName)
	if err != nil {
		return err
	}
	envelope, exists, err := readClaudeSetupTokenEnvelope(cfg, serviceName, accountName)
	if err != nil {
		return err
	}
	if !exists || account.SetupTokenRevision == "" || account.SetupTokenRevision != envelope.Revision {
		return ErrClaudeSetupTokenRevisionMismatch
	}
	if account.CredentialsError != "" {
		return errors.New("claude setup-token authentication is rejected")
	}
	if !account.LastProbeStartedAt.IsZero() && sample.ObservedAt.Before(account.LastProbeStartedAt) {
		return errors.New("claude status-line usage is older than the current probe")
	}
	usage, ok := sample.RoutingUsage(time.Now().UTC(), envelope.Revision, inactiveUsageTTL)
	if !ok {
		return errors.New("claude status-line usage is not trusted")
	}
	usage.Source = claudeUsageSourceStatusLine
	usage.TokenRevision = envelope.Revision
	account.Usage = usage
	account.LastProbeStartedAt = sample.ObservedAt
	state.Service(serviceName).Accounts[accountName] = account
	return SaveState(cfg.StatePath, state)
}
