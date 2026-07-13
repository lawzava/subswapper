package subswapper

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

type UsageSnapshot struct {
	FiveHour    LimitWindow `json:"five_hour,omitzero"`
	Weekly      LimitWindow `json:"weekly,omitzero"`
	FableWeekly LimitWindow `json:"fable_weekly,omitzero"`
	ObservedAt  time.Time   `json:"observed_at,omitzero"`
}

type LimitWindow struct {
	Used     float64   `json:"used,omitempty"`
	Limit    float64   `json:"limit,omitempty"`
	Pct      *float64  `json:"pct,omitempty"`
	ResetsAt time.Time `json:"resets_at,omitzero"`
}

type AccountStatus struct {
	Service    string
	Account    AccountState
	Active     bool
	Selectable bool
	Reason     string
	Score      float64
}

type ServiceStatus struct {
	Service  ServiceConfig
	Accounts []AccountStatus
}

// errCredentialsInvalid marks usage-fetch failures caused by rejected or
// unusable stored credentials; accounts failing this way must not stay
// selectable off a cached snapshot.
var errCredentialsInvalid = errors.New("stored credentials unusable")

var errCredentialSourceChanged = errors.New("credential source changed during probe")

// credentialSource is the managed file that holds an account's credentials.
// For the active account the live file is preferred, since the running
// client keeps it fresh; the backup copy is the fallback and the destination
// for refresh write-backs.
type credentialSource struct {
	data       []byte
	livePath   string
	backupPath string
	fromLive   bool
}

func snapshotCredentialSource(
	ctx context.Context,
	cfg Config,
	service ServiceConfig,
	account AccountState,
	active bool,
	find func() (credentialSource, error),
) (credentialSource, error) {
	lock, err := AcquireStateLock(ctx, cfg)
	if err != nil {
		return credentialSource{}, err
	}
	defer lock.Release()
	source, err := find()
	if err != nil {
		return credentialSource{}, err
	}
	if active && source.fromLive {
		if err := verifyActiveIdentity(cfg, service, account); err != nil {
			return credentialSource{}, fmt.Errorf("%w: %v", errCredentialsInvalid, err)
		}
	}
	return source, nil
}

func applyCredentialUpdate(
	ctx context.Context,
	cfg Config,
	service ServiceConfig,
	account AccountState,
	source credentialSource,
	updated []byte,
	updateLiveIfActive bool,
) error {
	lock, err := AcquireStateLock(ctx, cfg)
	if err != nil {
		return err
	}
	defer lock.Release()
	state, err := LoadState(cfg.StatePath)
	if err != nil {
		return err
	}
	if !account.AddedAt.IsZero() {
		current, ok := state.Account(service.Name, account.Name)
		if !ok || !current.AddedAt.Equal(account.AddedAt) {
			return errCredentialSourceChanged
		}
	}
	currentActive := state.Service(service.Name).ActiveAccount == account.Name
	if currentActive {
		if err := verifyActiveIdentity(cfg, service, account); err != nil {
			return fmt.Errorf("%w: %v", errCredentialsInvalid, err)
		}
	}
	sourcePath := source.backupPath
	if source.fromLive && currentActive {
		sourcePath = source.livePath
	}
	currentSource, err := os.ReadFile(sourcePath)
	if err != nil || !bytes.Equal(currentSource, source.data) {
		return errCredentialSourceChanged
	}
	if currentActive && !source.fromLive {
		currentLive, err := os.ReadFile(source.livePath)
		if err != nil || !bytes.Equal(currentLive, source.data) {
			return errCredentialSourceChanged
		}
	}
	staged := make([]stagedFile, 0, 2)
	backup, err := stageFile(source.backupPath, bytes.NewReader(updated))
	if err != nil {
		return err
	}
	staged = append(staged, backup)
	if updateLiveIfActive && currentActive {
		live, err := stageFile(source.livePath, bytes.NewReader(updated))
		if err != nil {
			backup.discard()
			return err
		}
		staged = append(staged, live)
	}
	if err := executeFileTransaction(cfg, staged); err != nil {
		for _, file := range staged {
			file.discard()
		}
		return err
	}
	for _, file := range staged {
		file.discard()
	}
	return nil
}

// findCredentialSource returns the first managed file whose contents satisfy
// usable, preferring the live file over the backup for the active account.
func findCredentialSource(cfg Config, service ServiceConfig, account AccountState, active bool, usable func([]byte) bool, missingMsg, readMsg string) (credentialSource, error) {
	accountDir := AccountDir(cfg, service.Name, account.Name)
	var firstErr error
	for _, file := range service.Files {
		backupPath := filepath.Join(accountDir, file.BackupName)
		livePath := ExpandPath(file.Path)
		candidates := []struct {
			path     string
			fromLive bool
		}{{livePath, true}, {backupPath, false}}
		if !active {
			candidates = candidates[1:]
		}
		for _, candidate := range candidates {
			data, err := os.ReadFile(candidate.path)
			if err != nil {
				if firstErr == nil {
					firstErr = err
				}
				continue
			}
			if !usable(data) {
				continue
			}
			return credentialSource{
				data:       data,
				livePath:   livePath,
				backupPath: backupPath,
				fromLive:   candidate.fromLive,
			}, nil
		}
	}
	if firstErr == nil {
		firstErr = errors.New(missingMsg)
	}
	return credentialSource{}, fmt.Errorf("%w: %s: %v", errCredentialsInvalid, readMsg, firstErr)
}

const (
	// rateLimitBackoffMin/Max bound how long usage fetches pause after a 429.
	rateLimitBackoffMin = 2 * time.Minute
	rateLimitBackoffMax = 15 * time.Minute
	// credentialsErrorBackoff is how long a dead-credential account waits
	// before its stored credentials are probed again.
	credentialsErrorBackoff = 30 * time.Minute
	// inactiveUsageTTL is how long an inactive account's cached usage is
	// considered fresh enough to skip the network fetch entirely.
	inactiveUsageTTL = 5 * time.Minute
)

const maxProbeErrorBytes = 512

var (
	ansiEscapePattern           = regexp.MustCompile(`\x1b\[[0-?]*[ -/]*[@-~]`)
	bearerCredentialPattern     = regexp.MustCompile(`(?i)(\bbearer\s+)[^"\s,;}]+`)
	credentialAssignmentPattern = regexp.MustCompile(`(?i)((?:"?(?:access[_-]?token|refresh[_-]?token|id[_-]?token|openai[_-]?api[_-]?key|anthropic[_-]?api[_-]?key|api[_-]?key|token)"?\s*[:=]\s*"?))([^"\s,;}]+)`)
	standaloneAPIKeyPattern     = regexp.MustCompile(`\b(?:sk|rk)-[A-Za-z0-9._-]{8,}`)
)

// rateLimitedError is a usage-fetch failure caused by provider rate limiting;
// retryAfter carries the server's Retry-After hint when present.
type rateLimitedError struct {
	retryAfter time.Duration
	message    string
}

func (e *rateLimitedError) Error() string { return e.message }

func (e *rateLimitedError) backoff() time.Duration {
	return min(max(e.retryAfter, rateLimitBackoffMin), rateLimitBackoffMax)
}

func CollectAll(ctx context.Context, cfg Config, state *State) []ServiceStatus {
	results := make([]ServiceStatus, 0, len(cfg.Services))
	for _, service := range cfg.Services {
		results = append(results, CollectService(ctx, cfg, state, service))
	}
	return results
}

func CollectService(ctx context.Context, cfg Config, state *State, service ServiceConfig) ServiceStatus {
	serviceState := state.Service(service.Name)
	statuses := make([]AccountStatus, 0, len(serviceState.Accounts))
	for _, name := range slices.Sorted(maps.Keys(serviceState.Accounts)) {
		account := serviceState.Accounts[name]
		status := AccountStatus{
			Service: service.Name,
			Account: account,
			Active:  serviceState.ActiveAccount == account.Name,
		}
		if service.Disabled {
			status.Reason = "service disabled"
			statuses = append(statuses, status)
			continue
		}
		if missing := missingRequiredBackups(cfg, service, account.Name); len(missing) > 0 {
			status.Reason = "missing backup files: " + strings.Join(missing, ", ")
			statuses = append(statuses, status)
			continue
		}
		var fetch func() (UsageSnapshot, error)
		switch {
		case len(service.UsageCommand) > 0:
			fetch = func() (UsageSnapshot, error) { return runUsageCommand(ctx, cfg, service, account) }
		case isClaudeService(service):
			fetch = func() (UsageSnapshot, error) {
				return fetchClaudeUsage(ctx, cfg, service, account, status.Active)
			}
		case isCodexService(service):
			fetch = func() (UsageSnapshot, error) {
				return fetchCodexUsage(ctx, cfg, service, account, status.Active)
			}
		}
		now := time.Now().UTC()
		var fetchErr error
		switch {
		case fetch == nil:
		case now.Before(status.Account.FetchBackoffUntil):
			retryAt := status.Account.FetchBackoffUntil.Local().Format("15:04")
			if status.Account.CredentialsError != "" {
				status.Reason = fmt.Sprintf("%s (retry at %s)", status.Account.CredentialsError, retryAt)
				statuses = append(statuses, status)
				continue
			}
			reason := status.Account.LastProbeError
			if reason == "" {
				reason = "previous provider failure"
			}
			fetchErr = fmt.Errorf("usage checks paused until %s after %s", retryAt, reason)
		case !status.Active && status.Account.CredentialsError == "" &&
			!status.Account.Usage.ObservedAt.IsZero() &&
			now.Sub(status.Account.Usage.ObservedAt) < inactiveUsageTTL:
			// Inactive accounts barely change between resets; the cached
			// snapshot is fresh enough to skip the network round-trip.
		default:
			usage, err := fetch()
			var rateLimited *rateLimitedError
			switch {
			case err == nil:
				status.Account.Usage = usage
				status.Account.FetchBackoffUntil = time.Time{}
				status.Account.CredentialsError = ""
				status.Account.LastProbeError = ""
				serviceState.Accounts[account.Name] = status.Account
			case errors.Is(err, errCredentialsInvalid):
				status.Account.FetchBackoffUntil = now.Add(credentialsErrorBackoff)
				status.Account.CredentialsError = sanitizeProbeError(err)
				status.Account.LastProbeError = ""
				serviceState.Accounts[account.Name] = status.Account
				status.Reason = status.Account.CredentialsError
				statuses = append(statuses, status)
				continue
			case errors.As(err, &rateLimited):
				status.Account.FetchBackoffUntil = now.Add(rateLimited.backoff())
				status.Account.LastProbeError = sanitizeProbeError(err)
				serviceState.Accounts[account.Name] = status.Account
				if !status.Account.Usage.HasLimits() {
					status.Reason = status.Account.LastProbeError
					statuses = append(statuses, status)
					continue
				}
				fetchErr = errors.New(status.Account.LastProbeError)
			case !status.Account.Usage.HasLimits():
				status.Account.FetchBackoffUntil = now.Add(transientProbeBackoff(cfg.Monitor.Interval.Duration))
				status.Account.LastProbeError = sanitizeProbeError(err)
				serviceState.Accounts[account.Name] = status.Account
				status.Reason = status.Account.LastProbeError
				statuses = append(statuses, status)
				continue
			default:
				// Fall back to the cached snapshot, but surface the failure.
				status.Account.FetchBackoffUntil = now.Add(transientProbeBackoff(cfg.Monitor.Interval.Duration))
				status.Account.LastProbeError = sanitizeProbeError(err)
				serviceState.Accounts[account.Name] = status.Account
				fetchErr = errors.New(status.Account.LastProbeError)
			}
		}
		markAccountSelectable(&status)
		if fetchErr != nil && status.Selectable {
			status.Reason = fmt.Sprintf("ready (stale usage from %s: %v)",
				staleObservedAt(status.Account.Usage.ObservedAt), fetchErr)
		}
		statuses = append(statuses, status)
	}
	return ServiceStatus{Service: service, Accounts: statuses}
}

func transientProbeBackoff(configured time.Duration) time.Duration {
	return max(configured, time.Minute)
}

func sanitizeProbeError(err error) string {
	if err == nil {
		return ""
	}
	message := ansiEscapePattern.ReplaceAllString(err.Error(), " ")
	message = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, message)
	message = strings.Join(strings.Fields(message), " ")
	message = bearerCredentialPattern.ReplaceAllString(message, "${1}[REDACTED]")
	message = credentialAssignmentPattern.ReplaceAllString(message, "${1}[REDACTED]")
	message = standaloneAPIKeyPattern.ReplaceAllString(message, "[REDACTED]")
	lower := strings.ToLower(message)
	for _, marker := range []string{"; body=", ": warning:", ": {\"error\"", ": { \"error\""} {
		if index := strings.Index(lower, marker); index >= 0 {
			message = strings.TrimSpace(message[:index]) + " (provider detail omitted)"
			break
		}
	}
	if len(message) <= maxProbeErrorBytes {
		return message
	}
	message = message[:maxProbeErrorBytes]
	for !utf8.ValidString(message) {
		message = message[:len(message)-1]
	}
	return strings.TrimSpace(message)
}

// missingRequiredBackups lists required managed files absent from the
// account's backup — an account that cannot be restored must never be
// selected, whatever its cached usage says.
func missingRequiredBackups(cfg Config, service ServiceConfig, accountName string) []string {
	accountDir := AccountDir(cfg, service.Name, accountName)
	var missing []string
	for _, file := range service.Files {
		if !file.IsRequired() {
			continue
		}
		if _, err := os.Stat(filepath.Join(accountDir, file.BackupName)); err != nil {
			missing = append(missing, file.BackupName)
		}
	}
	return missing
}

func staleObservedAt(observedAt time.Time) string {
	if observedAt.IsZero() {
		return "unknown time"
	}
	return observedAt.Local().Format("Jan02 15:04")
}

func isClaudeService(service ServiceConfig) bool {
	// Kind only: ApplyDefaults copies the name into an empty kind, and an
	// explicit non-claude kind (e.g. "custom") opts out of the built-in fetcher.
	kind := strings.ToLower(service.Kind)
	return kind == "claude" || kind == "claude-code"
}

func isCodexService(service ServiceConfig) bool {
	return strings.ToLower(service.Kind) == "codex"
}

func markAccountSelectable(status *AccountStatus) {
	switch {
	case !status.Account.Usage.HasLimits():
		status.Reason = "missing usage limits"
	case status.Account.Usage.Exhausted():
		status.Reason = "limit reached"
	default:
		status.Selectable = true
		status.Score = status.Account.Usage.Score()
		status.Reason = "ready"
	}
}

func runUsageCommand(ctx context.Context, cfg Config, service ServiceConfig, account AccountState) (UsageSnapshot, error) {
	command := service.UsageCommand
	cmd := exec.CommandContext(ctx, command[0], command[1:]...)
	cmd.Env = append(os.Environ(),
		"SUBSWAPPER_SERVICE="+service.Name,
		"SUBSWAPPER_ACCOUNT="+account.Name,
		"SUBSWAPPER_EMAIL="+account.Email,
		"SUBSWAPPER_ACCOUNT_DIR="+AccountDir(cfg, service.Name, account.Name),
		"SUBSWAPPER_BACKUP_ROOT="+ExpandPath(cfg.BackupRoot),
	)

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	// Bound Wait after cancellation even if a grandchild keeps the pipes open.
	cmd.WaitDelay = 10 * time.Second
	if err := cmd.Run(); err != nil {
		return UsageSnapshot{}, fmt.Errorf("usage command failed: %w", err)
	}
	var usage UsageSnapshot
	if err := json.NewDecoder(&stdout).Decode(&usage); err != nil {
		return UsageSnapshot{}, fmt.Errorf("usage command returned invalid JSON: %w", err)
	}
	if !usage.HasCoreLimits() {
		return UsageSnapshot{}, fmt.Errorf("usage command returned missing limits")
	}
	if usage.ObservedAt.IsZero() {
		usage.ObservedAt = time.Now().UTC()
	}
	return usage, nil
}

func (u UsageSnapshot) HasLimits() bool {
	return u.HasAnyLimit()
}

func (u UsageSnapshot) HasAnyLimit() bool {
	return len(u.ratios()) > 0
}

func (u UsageSnapshot) HasCoreLimits() bool {
	_, hasFiveHour := u.FiveHour.Ratio()
	_, hasWeekly := u.Weekly.Ratio()
	return hasFiveHour && hasWeekly
}

func (u UsageSnapshot) Exhausted() bool {
	return u.AtOrAbove(1)
}

func (u UsageSnapshot) AtOrAbove(threshold float64) bool {
	for _, ratio := range u.ratios() {
		if ratio >= threshold {
			return true
		}
	}
	return false
}

func (u UsageSnapshot) Score() float64 {
	ratios := u.ratios()
	if len(ratios) == 0 {
		return math.Inf(1)
	}
	score := ratios[0]
	for _, ratio := range ratios[1:] {
		score = max(score, ratio)
	}
	return score
}

func (u UsageSnapshot) AverageRatio() float64 {
	ratios := u.ratios()
	if len(ratios) == 0 {
		return math.Inf(1)
	}
	var total float64
	for _, ratio := range ratios {
		total += ratio
	}
	return total / float64(len(ratios))
}

func (u UsageSnapshot) ratios() []float64 {
	ratios := make([]float64, 0, 3)
	if fiveHour, ok := u.FiveHour.Ratio(); ok {
		ratios = append(ratios, fiveHour)
	}
	if weekly, ok := u.Weekly.Ratio(); ok {
		ratios = append(ratios, weekly)
	}
	if fableWeekly, ok := u.FableWeekly.Ratio(); ok {
		ratios = append(ratios, fableWeekly)
	}
	return ratios
}

func (w LimitWindow) Ratio() (float64, bool) {
	if w.Pct == nil && w.Limit <= 0 {
		return 0, false
	}
	// A window whose reset time has passed no longer counts at its recorded
	// usage; treat it as fresh until a live fetch replaces the snapshot.
	if !w.ResetsAt.IsZero() && !time.Now().Before(w.ResetsAt) {
		return 0, true
	}
	if w.Pct != nil {
		return max(0, *w.Pct/100), true
	}
	return max(0, w.Used/w.Limit), true
}

func PtrFloat64(value float64) *float64 {
	return &value
}
