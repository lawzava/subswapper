package subswapper

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode"
)

const claudeSetupTokenLifetime = 365 * 24 * time.Hour

var (
	ErrClaudeSetupTokenNotConfigured     = errors.New("claude setup token is not configured")
	ErrClaudeSetupTokenExpired           = errors.New("claude setup token is expired")
	ErrClaudeSetupTokenDuplicate         = errors.New("claude setup token duplicates another account")
	ErrClaudeSetupTokenIdentityDuplicate = errors.New("claude account identity duplicates another account")
	ErrClaudeSetupTokenStorageUnsafe     = errors.New("claude setup token storage is unsafe")
	ErrClaudeSetupTokenRevisionMismatch  = errors.New("claude setup token metadata does not match secure storage")
)

var (
	claudeSetupTokenNow         = time.Now
	replaceClaudeSetupTokenFile = os.Rename
	saveClaudeSetupTokenState   = SaveState
)

// ClaudeSetupTokenIdentity is empty when the trusted provider cannot expose
// an account identity for an inference-only setup token.
type ClaudeSetupTokenIdentity struct {
	AccountUUID string
}

// ClaudeSetupTokenIdentityLookup receives the token only in memory. Callers
// must not persist it or include provider output in returned errors.
type ClaudeSetupTokenIdentityLookup func(context.Context, string) (ClaudeSetupTokenIdentity, error)

type ClaudeSetupTokenStatus struct {
	Configured    bool
	Usable        bool
	Expired       bool
	IdentityKnown bool
	AccountUUID   string
	Revision      string
	StoredAt      time.Time
	ExpiresAt     time.Time
}

type claudeSetupTokenEnvelope struct {
	Token     string    `json:"token"`
	Revision  string    `json:"revision"`
	StoredAt  time.Time `json:"stored_at"`
	ExpiresAt time.Time `json:"expires_at"`
}

// ReplaceClaudeSetupToken atomically replaces one account token. The token
// file is committed before non-secret state so a partial failure fails closed.
func ReplaceClaudeSetupToken(ctx context.Context, cfg Config, serviceName, accountName, token string, lookup ClaudeSetupTokenIdentityLookup) (ClaudeSetupTokenStatus, error) {
	token = strings.TrimSpace(token)
	if token == "" || strings.IndexFunc(token, unicode.IsSpace) >= 0 || strings.ContainsRune(token, '\x00') {
		return ClaudeSetupTokenStatus{}, errors.New("claude setup token is empty or malformed")
	}
	identity := ClaudeSetupTokenIdentity{}
	var err error
	if lookup != nil {
		identity, err = lookup(ctx, token)
		if err != nil {
			return ClaudeSetupTokenStatus{}, errors.New("claude setup token identity lookup failed")
		}
		identity.AccountUUID = strings.TrimSpace(identity.AccountUUID)
	}

	lock, err := AcquireStateLock(ctx, cfg)
	if err != nil {
		return ClaudeSetupTokenStatus{}, err
	}
	defer lock.Release()

	state, err := LoadState(cfg.StatePath)
	if err != nil {
		return ClaudeSetupTokenStatus{}, err
	}
	account, err := claudeSetupTokenAccount(cfg, state, serviceName, accountName)
	if err != nil {
		return ClaudeSetupTokenStatus{}, err
	}
	if err := rejectDuplicateClaudeSetupToken(cfg, state, token); err != nil {
		return ClaudeSetupTokenStatus{}, err
	}
	if identity.AccountUUID != "" && duplicateClaudeAccountUUID(state, serviceName, accountName, identity.AccountUUID) {
		return ClaudeSetupTokenStatus{}, ErrClaudeSetupTokenIdentityDuplicate
	}

	revision, err := newSetupTokenRevision()
	if err != nil {
		return ClaudeSetupTokenStatus{}, errors.New("generate Claude setup token revision")
	}
	storedAt := claudeSetupTokenNow().UTC()
	envelope := claudeSetupTokenEnvelope{
		Token:     token,
		Revision:  revision,
		StoredAt:  storedAt,
		ExpiresAt: storedAt.Add(claudeSetupTokenLifetime),
	}
	path, err := prepareClaudeSetupTokenPath(cfg, serviceName, accountName)
	if err != nil {
		return ClaudeSetupTokenStatus{}, err
	}
	if err := writeClaudeSetupTokenEnvelope(path, envelope); err != nil {
		return ClaudeSetupTokenStatus{}, err
	}

	account.AccountUUID = identity.AccountUUID
	account.SetupTokenRevision = revision
	clearSetupTokenProbeState(&account)
	state.Service(serviceName).Accounts[accountName] = account
	if err := saveClaudeSetupTokenState(cfg.StatePath, state); err != nil {
		return ClaudeSetupTokenStatus{}, err
	}
	return setupTokenStatus(account, envelope, storedAt), nil
}

func LoadClaudeSetupToken(cfg Config, serviceName, accountName string) (string, error) {
	token, _, err := LoadClaudeSetupTokenWithStatus(cfg, serviceName, accountName)
	return token, err
}

// LoadClaudeSetupTokenWithStatus returns the token and its revision from one
// locked snapshot. Callers use the revision to reject concurrent replacement.
func LoadClaudeSetupTokenWithStatus(cfg Config, serviceName, accountName string) (string, ClaudeSetupTokenStatus, error) {
	lock, err := AcquireStateLock(context.Background(), cfg)
	if err != nil {
		return "", ClaudeSetupTokenStatus{}, err
	}
	defer lock.Release()
	state, err := LoadState(cfg.StatePath)
	if err != nil {
		return "", ClaudeSetupTokenStatus{}, err
	}
	account, err := claudeSetupTokenAccount(cfg, state, serviceName, accountName)
	if err != nil {
		return "", ClaudeSetupTokenStatus{}, err
	}
	envelope, exists, err := readClaudeSetupTokenEnvelope(cfg, serviceName, accountName)
	if err != nil {
		return "", ClaudeSetupTokenStatus{}, err
	}
	if !exists {
		return "", ClaudeSetupTokenStatus{}, ErrClaudeSetupTokenNotConfigured
	}
	if account.SetupTokenRevision == "" || account.SetupTokenRevision != envelope.Revision {
		return "", ClaudeSetupTokenStatus{}, ErrClaudeSetupTokenRevisionMismatch
	}
	now := claudeSetupTokenNow().UTC()
	status := setupTokenStatus(account, envelope, now)
	if status.Expired {
		return "", status, ErrClaudeSetupTokenExpired
	}
	return envelope.Token, status, nil
}

func ClaudeSetupTokenStatusForAccount(cfg Config, serviceName, accountName string) (ClaudeSetupTokenStatus, error) {
	lock, err := AcquireStateLock(context.Background(), cfg)
	if err != nil {
		return ClaudeSetupTokenStatus{}, err
	}
	defer lock.Release()
	state, err := LoadState(cfg.StatePath)
	if err != nil {
		return ClaudeSetupTokenStatus{}, err
	}
	account, err := claudeSetupTokenAccount(cfg, state, serviceName, accountName)
	if err != nil {
		return ClaudeSetupTokenStatus{}, err
	}
	envelope, exists, err := readClaudeSetupTokenEnvelope(cfg, serviceName, accountName)
	if err != nil {
		return ClaudeSetupTokenStatus{}, err
	}
	if !exists {
		if account.SetupTokenRevision != "" {
			return ClaudeSetupTokenStatus{}, ErrClaudeSetupTokenRevisionMismatch
		}
		return ClaudeSetupTokenStatus{}, nil
	}
	if account.SetupTokenRevision == "" || account.SetupTokenRevision != envelope.Revision {
		return ClaudeSetupTokenStatus{}, ErrClaudeSetupTokenRevisionMismatch
	}
	return setupTokenStatus(account, envelope, claudeSetupTokenNow().UTC()), nil
}

func RemoveClaudeSetupToken(ctx context.Context, cfg Config, serviceName, accountName string) error {
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
	path := claudeSetupTokenPath(cfg, serviceName, accountName)
	if err := removeClaudeSetupTokenFile(path); err != nil {
		return err
	}
	account.AccountUUID = ""
	account.SetupTokenRevision = ""
	clearSetupTokenProbeState(&account)
	state.Service(serviceName).Accounts[accountName] = account
	return saveClaudeSetupTokenState(cfg.StatePath, state)
}

func claudeSetupTokenAccount(cfg Config, state *State, serviceName, accountName string) (AccountState, error) {
	service, ok := cfg.Service(serviceName)
	if !ok {
		return AccountState{}, fmt.Errorf("service %q not found", serviceName)
	}
	if !isClaudeService(service) || !service.UsesAccountHomes() {
		return AccountState{}, fmt.Errorf("service %q does not support Claude setup tokens", serviceName)
	}
	account, ok := state.Account(serviceName, accountName)
	if !ok {
		return AccountState{}, fmt.Errorf("account %q not found for service %q", accountName, serviceName)
	}
	return account, nil
}

func rejectDuplicateClaudeSetupToken(cfg Config, state *State, token string) error {
	for _, candidate := range cfg.Services {
		if !isClaudeService(candidate) {
			continue
		}
		service := state.Services[candidate.Name]
		if service == nil {
			continue
		}
		for otherName := range service.Accounts {
			envelope, exists, err := readClaudeSetupTokenEnvelope(cfg, candidate.Name, otherName)
			if err != nil {
				return err
			}
			if exists && subtle.ConstantTimeCompare([]byte(token), []byte(envelope.Token)) == 1 {
				return ErrClaudeSetupTokenDuplicate
			}
		}
	}
	return nil
}

func duplicateClaudeAccountUUID(state *State, serviceName, accountName, accountUUID string) bool {
	for otherService, service := range state.Services {
		if service == nil {
			continue
		}
		for otherName, account := range service.Accounts {
			if otherService == serviceName && otherName == accountName {
				continue
			}
			if account.AccountUUID == accountUUID {
				return true
			}
		}
	}
	return false
}

func clearSetupTokenProbeState(account *AccountState) {
	account.Usage = UsageSnapshot{}
	account.FetchBackoffUntil = time.Time{}
	account.CredentialsError = ""
	account.LastProbeError = ""
	account.LastProbeStartedAt = time.Time{}
}

func setupTokenStatus(account AccountState, envelope claudeSetupTokenEnvelope, now time.Time) ClaudeSetupTokenStatus {
	expired := !now.Before(envelope.ExpiresAt)
	return ClaudeSetupTokenStatus{
		Configured:    true,
		Usable:        !expired,
		Expired:       expired,
		IdentityKnown: account.AccountUUID != "",
		AccountUUID:   account.AccountUUID,
		Revision:      envelope.Revision,
		StoredAt:      envelope.StoredAt,
		ExpiresAt:     envelope.ExpiresAt,
	}
}

func newSetupTokenRevision() (string, error) {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(value[:]), nil
}

func claudeSetupTokenRoot(cfg Config) string {
	// The state carries the matching random revision, so keep both stores under
	// the same data root when callers override account-home locations.
	return filepath.Join(filepath.Dir(ExpandPath(cfg.StatePath)), "tokens")
}

func claudeSetupTokenPath(cfg Config, serviceName, accountName string) string {
	return filepath.Join(claudeSetupTokenRoot(cfg), safeName(serviceName), safeName(accountName), "setup-token.json")
}

func prepareClaudeSetupTokenPath(cfg Config, serviceName, accountName string) (string, error) {
	root := claudeSetupTokenRoot(cfg)
	if err := os.MkdirAll(filepath.Dir(root), 0o700); err != nil {
		return "", err
	}
	for _, path := range []string{root, filepath.Join(root, safeName(serviceName)), filepath.Join(root, safeName(serviceName), safeName(accountName))} {
		if err := ensurePrivateSetupTokenDirectory(path); err != nil {
			return "", err
		}
	}
	return claudeSetupTokenPath(cfg, serviceName, accountName), nil
}

func ensurePrivateSetupTokenDirectory(path string) error {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		if err := os.Mkdir(path, 0o700); err != nil {
			return err
		}
		info, err = os.Lstat(path)
	}
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return ErrClaudeSetupTokenStorageUnsafe
	}
	if info.Mode().Perm() != 0o700 {
		if err := os.Chmod(path, 0o700); err != nil {
			return err
		}
	}
	return verifyPrivateSetupTokenDirectory(path)
}

func verifyPrivateSetupTokenDirectory(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() || info.Mode().Perm() != 0o700 {
		return ErrClaudeSetupTokenStorageUnsafe
	}
	return nil
}

func verifyClaudeSetupTokenDirectories(cfg Config, serviceName, accountName string) error {
	root := claudeSetupTokenRoot(cfg)
	for _, path := range []string{root, filepath.Join(root, safeName(serviceName)), filepath.Join(root, safeName(serviceName), safeName(accountName))} {
		if err := verifyPrivateSetupTokenDirectory(path); err != nil {
			return ErrClaudeSetupTokenStorageUnsafe
		}
	}
	return nil
}

func writeClaudeSetupTokenEnvelope(path string, envelope claudeSetupTokenEnvelope) error {
	data, err := json.Marshal(envelope)
	if err != nil {
		return errors.New("encode Claude setup token")
	}
	if err := verifySetupTokenTarget(path, true); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".setup-token-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer func() { _ = os.Remove(tmpPath) }()
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := verifySetupTokenTarget(path, true); err != nil {
		return err
	}
	if err := replaceClaudeSetupTokenFile(tmpPath, path); err != nil {
		return errors.New("replace Claude setup token file")
	}
	if err := syncSetupTokenDirectory(filepath.Dir(path)); err != nil {
		return err
	}
	return verifySetupTokenTarget(path, false)
}

func readClaudeSetupTokenEnvelope(cfg Config, serviceName, accountName string) (claudeSetupTokenEnvelope, bool, error) {
	path := claudeSetupTokenPath(cfg, serviceName, accountName)
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return claudeSetupTokenEnvelope{}, false, nil
	}
	if err != nil {
		return claudeSetupTokenEnvelope{}, false, err
	}
	if err := verifyClaudeSetupTokenDirectories(cfg, serviceName, accountName); err != nil {
		return claudeSetupTokenEnvelope{}, false, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
		return claudeSetupTokenEnvelope{}, false, ErrClaudeSetupTokenStorageUnsafe
	}
	file, err := os.Open(path)
	if err != nil {
		return claudeSetupTokenEnvelope{}, false, err
	}
	defer func() { _ = file.Close() }()
	openedInfo, err := file.Stat()
	if err != nil || !openedInfo.Mode().IsRegular() || openedInfo.Mode().Perm() != 0o600 || !os.SameFile(info, openedInfo) {
		return claudeSetupTokenEnvelope{}, false, ErrClaudeSetupTokenStorageUnsafe
	}
	data, err := io.ReadAll(file)
	if err != nil {
		return claudeSetupTokenEnvelope{}, false, err
	}
	var envelope claudeSetupTokenEnvelope
	if json.Unmarshal(data, &envelope) != nil || envelope.Token == "" || envelope.Revision == "" || envelope.StoredAt.IsZero() || !envelope.ExpiresAt.After(envelope.StoredAt) {
		return claudeSetupTokenEnvelope{}, false, errors.New("claude setup token file is malformed")
	}
	return envelope, true, nil
}

func verifySetupTokenTarget(path string, allowMissing bool) error {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) && allowMissing {
		return nil
	}
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return ErrClaudeSetupTokenStorageUnsafe
	}
	if info.Mode().Perm() != 0o600 {
		return ErrClaudeSetupTokenStorageUnsafe
	}
	return nil
}

func removeClaudeSetupTokenFile(path string) error {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
		return ErrClaudeSetupTokenStorageUnsafe
	}
	if err := os.Remove(path); err != nil {
		return err
	}
	return syncSetupTokenDirectory(filepath.Dir(path))
}

func syncSetupTokenDirectory(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() { _ = directory.Close() }()
	return directory.Sync()
}
