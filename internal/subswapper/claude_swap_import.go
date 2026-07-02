package subswapper

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"time"
)

type ImportResult struct {
	Imported []AccountState
	Skipped  []string
	Errors   []error
	Active   string
}

type claudeSwapSequence struct {
	ActiveAccountNumber int                          `json:"activeAccountNumber"`
	Sequence            []int                        `json:"sequence"`
	Accounts            map[string]claudeSwapAccount `json:"accounts"`
}

type claudeSwapAccount struct {
	Email string `json:"email"`
	Added string `json:"added"`
}

type claudeSwapUsageCache struct {
	Data map[string]claudeSwapCachedUsage `json:"data"`
}

type claudeSwapCachedUsage struct {
	FiveHour *claudeSwapCachedWindow `json:"five_hour"`
	SevenDay *claudeSwapCachedWindow `json:"seven_day"`
}

type claudeSwapCachedWindow struct {
	Pct      float64 `json:"pct"`
	ResetsAt string  `json:"resets_at"`
}

func DefaultClaudeSwapRoot() string {
	if runtime.GOOS == "linux" {
		if xdg := os.Getenv("XDG_DATA_HOME"); xdg != "" {
			expanded := ExpandPath(xdg)
			if filepath.IsAbs(expanded) {
				return filepath.Join(expanded, "claude-swap")
			}
		}
		return filepath.Join(mustHomeDir(), ".local", "share", "claude-swap")
	}
	return filepath.Join(mustHomeDir(), ".claude-swap-backup")
}

func ImportClaudeSwap(cfg Config, root string) (ImportResult, error) {
	service, ok := cfg.Service("claude")
	if !ok {
		return ImportResult{}, errors.New(`config must include service "claude"`)
	}
	if root == "" {
		root = DefaultClaudeSwapRoot()
	}
	root = ExpandPath(root)

	sequence, err := readClaudeSwapSequence(root)
	if err != nil {
		return ImportResult{}, err
	}
	usageCache := readClaudeSwapUsageCache(root)

	lock, err := AcquireStateLock(cfg)
	if err != nil {
		return ImportResult{}, err
	}
	defer lock.Release()

	state, err := LoadState(cfg.StatePath)
	if err != nil {
		return ImportResult{}, err
	}
	serviceState := state.Service(service.Name)
	order := sequence.Sequence
	if len(order) == 0 {
		for number := range sequence.Accounts {
			parsed, err := strconv.Atoi(number)
			if err == nil {
				order = append(order, parsed)
			}
		}
		sort.Ints(order)
	}

	result := ImportResult{}
	for _, number := range order {
		numberString := strconv.Itoa(number)
		entry, ok := sequence.Accounts[numberString]
		if !ok {
			continue
		}
		accountName := "cswap-" + numberString
		if _, exists := serviceState.Accounts[accountName]; exists {
			// Never clobber an already-captured account: its stored
			// credentials may be fresher than the cswap backup.
			result.Skipped = append(result.Skipped, accountName)
			continue
		}
		credentials, err := readClaudeSwapCredentials(root, numberString, entry.Email)
		if err != nil {
			result.Errors = append(result.Errors, fmt.Errorf("slot %s: %w", numberString, err))
			continue
		}
		config, err := os.ReadFile(filepath.Join(root, "configs", fmt.Sprintf(".claude-config-%s-%s.json", numberString, entry.Email)))
		if err != nil {
			result.Errors = append(result.Errors, fmt.Errorf("slot %s: %w", numberString, err))
			continue
		}
		accountDir := AccountDir(cfg, service.Name, accountName)
		if err := os.MkdirAll(accountDir, 0o700); err != nil {
			return ImportResult{}, err
		}
		if err := writeFileAtomic(filepath.Join(accountDir, "credentials.json"), credentials); err != nil {
			return ImportResult{}, err
		}
		if err := writeFileAtomic(filepath.Join(accountDir, "claude.json"), config); err != nil {
			return ImportResult{}, err
		}

		account := AccountState{
			Name:     accountName,
			Email:    entry.Email,
			Provider: "claude-swap",
			Slot:     numberString,
			AddedAt:  parseOptionalTime(entry.Added),
			Usage:    usageCache[numberString],
		}
		if account.AddedAt.IsZero() {
			account.AddedAt = time.Now().UTC()
		}
		serviceState.Accounts[accountName] = account
		// Only adopt cswap's active slot when nothing is active yet AND the
		// live files really are this slot's credentials. The import never
		// touches live files, so marking anything else active would desync
		// state from disk — and the sync-before-switch step would then copy
		// a different login over this slot's backup.
		if sequence.ActiveAccountNumber == number && serviceState.ActiveAccount == "" &&
			liveFilesMatchBackup(service, accountDir) {
			serviceState.ActiveAccount = accountName
			result.Active = accountName
		}
		result.Imported = append(result.Imported, account)
	}
	if len(result.Imported) == 0 && len(result.Skipped) == 0 {
		return ImportResult{}, errors.Join(append([]error{errors.New("no claude-swap accounts found")}, result.Errors...)...)
	}
	if err := SaveState(cfg.StatePath, state); err != nil {
		return ImportResult{}, err
	}
	return result, nil
}

// liveFilesMatchBackup reports whether every required live managed file is
// byte-identical to the account's backup copy.
func liveFilesMatchBackup(service ServiceConfig, accountDir string) bool {
	matched := false
	for _, file := range service.Files {
		if !file.IsRequired() {
			continue
		}
		backup, err := os.ReadFile(filepath.Join(accountDir, file.BackupName))
		if err != nil {
			return false
		}
		live, err := os.ReadFile(ExpandPath(file.Path))
		if err != nil || !bytes.Equal(live, backup) {
			return false
		}
		matched = true
	}
	return matched
}

func readClaudeSwapSequence(root string) (claudeSwapSequence, error) {
	data, err := os.ReadFile(filepath.Join(root, "sequence.json"))
	if err != nil {
		return claudeSwapSequence{}, err
	}
	var sequence claudeSwapSequence
	if err := json.Unmarshal(data, &sequence); err != nil {
		return claudeSwapSequence{}, err
	}
	if len(sequence.Accounts) == 0 {
		return claudeSwapSequence{}, errors.New("claude-swap sequence has no accounts")
	}
	return sequence, nil
}

func readClaudeSwapCredentials(root, number, email string) ([]byte, error) {
	encoded, err := os.ReadFile(filepath.Join(root, "credentials", fmt.Sprintf(".creds-%s-%s.enc", number, email)))
	if err != nil {
		return nil, err
	}
	decoded, err := base64.StdEncoding.DecodeString(string(bytes.Trim(encoded, " \n\r\t")))
	if err != nil {
		return nil, err
	}
	if len(decoded) == 0 {
		return nil, errors.New("empty claude-swap credential backup")
	}
	return decoded, nil
}

func readClaudeSwapUsageCache(root string) map[string]UsageSnapshot {
	result := map[string]UsageSnapshot{}
	data, err := os.ReadFile(filepath.Join(root, "cache", "usage.json"))
	if err != nil {
		return result
	}
	var cache claudeSwapUsageCache
	if err := json.Unmarshal(data, &cache); err != nil {
		return result
	}
	for number, usage := range cache.Data {
		result[number] = convertClaudeSwapCachedUsage(usage)
	}
	return result
}

func convertClaudeSwapCachedUsage(cached claudeSwapCachedUsage) UsageSnapshot {
	var usage UsageSnapshot
	if cached.FiveHour != nil {
		usage.FiveHour = LimitWindow{
			Pct:      PtrFloat64(cached.FiveHour.Pct),
			ResetsAt: parseOptionalTime(cached.FiveHour.ResetsAt),
		}
	}
	if cached.SevenDay != nil {
		usage.Weekly = LimitWindow{
			Pct:      PtrFloat64(cached.SevenDay.Pct),
			ResetsAt: parseOptionalTime(cached.SevenDay.ResetsAt),
		}
	}
	return usage
}

func mustHomeDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return "."
	}
	return home
}
