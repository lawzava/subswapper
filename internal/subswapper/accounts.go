package subswapper

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
)

// ensureAccount registers accountName unless it exists and reports whether
// it was created. A non-empty email replaces the stored label.
func ensureAccount(cfg Config, service ServiceConfig, accountName, email string) (bool, error) {
	state, err := LoadState(cfg.StatePath)
	if err != nil {
		return false, err
	}
	if _, ok := state.Account(service.Name, accountName); !ok {
		if _, _, err := CreateAccountHome(cfg, service.Name, accountName, email); err != nil {
			return false, err
		}
		return true, nil
	}
	if email == "" {
		return false, nil
	}
	lock, err := AcquireStateLock(context.Background(), cfg)
	if err != nil {
		return false, err
	}
	defer lock.Release()
	state, err = LoadState(cfg.StatePath)
	if err != nil {
		return false, err
	}
	account, ok := state.Account(service.Name, accountName)
	if !ok || account.Email == email {
		return false, nil
	}
	account.Email = email
	state.Service(service.Name).Accounts[accountName] = account
	return false, SaveState(cfg.StatePath, state)
}

func accountService(cfg Config, serviceName string, kind func(ServiceConfig) bool, label string) (ServiceConfig, error) {
	service, ok := cfg.Service(serviceName)
	if !ok {
		return ServiceConfig{}, fmt.Errorf("service %q not found", serviceName)
	}
	if !kind(service) || !service.UsesAccountHomes() || service.HubClient() {
		return ServiceConfig{}, fmt.Errorf("service %q does not hold %s accounts on this machine", serviceName, label)
	}
	return service, nil
}

// AddClaudeAccount registers accountName if needed and stores its setup
// token. It reports whether the account is new. Adding an existing account
// replaces its token.
func AddClaudeAccount(ctx context.Context, cfg Config, serviceName, accountName, email, token string, lookup ClaudeSetupTokenIdentityLookup) (bool, error) {
	service, err := accountService(cfg, serviceName, isClaudeService, "Claude")
	if err != nil {
		return false, err
	}
	token = strings.TrimSpace(token)
	if !strings.HasPrefix(token, "sk-ant-") {
		return false, errors.New("claude setup token is empty or malformed; it starts with sk-ant-")
	}
	if err := validateAccountName(accountName); err != nil {
		return false, err
	}
	created, err := ensureAccount(cfg, service, accountName, email)
	if err != nil {
		return false, err
	}
	if _, err := ReplaceClaudeSetupToken(ctx, cfg, service.Name, accountName, token, lookup); err != nil {
		if created {
			_ = RemoveAccountWithOptions(cfg, service.Name, accountName, true, true)
		}
		return false, err
	}
	return created, ResetAccountProbeState(cfg, service.Name, accountName)
}

// validateCodexLogin accepts a real file-stored ChatGPT login only.
func validateCodexLogin(data []byte) error {
	var auth codexProxyAuthFile
	if json.Unmarshal(data, &auth) != nil {
		return errors.New("codex login is not valid JSON")
	}
	if auth.AuthMode != "" && auth.AuthMode != "chatgpt" {
		return fmt.Errorf("codex login mode %q is not a ChatGPT subscription login", auth.AuthMode)
	}
	if auth.Tokens.AccessToken == "" || auth.Tokens.RefreshToken == "" {
		return errors.New("codex login has no ChatGPT tokens")
	}
	if IsCodexProxyAuthFile(data) {
		return errors.New("codex login is the subswapper proxy placeholder, not a real login")
	}
	return nil
}

// CodexLoginEmail returns the email claim of a ChatGPT login's ID token, or "".
func CodexLoginEmail(data []byte) string {
	var auth codexProxyAuthFile
	if json.Unmarshal(data, &auth) != nil {
		return ""
	}
	parts := strings.Split(auth.Tokens.IDToken, ".")
	if len(parts) != 3 {
		return ""
	}
	payload, err := base64URLDecode(strings.TrimRight(parts[1], "="))
	if err != nil {
		return ""
	}
	var claims struct {
		Email string `json:"email"`
	}
	if json.Unmarshal(payload, &claims) != nil {
		return ""
	}
	return claims.Email
}

// AddCodexAccount registers accountName if needed and installs a ChatGPT
// login into its home. Adding an existing account replaces its login. The
// email defaults to the login's own.
func AddCodexAccount(cfg Config, serviceName, accountName, email string, login []byte) (bool, error) {
	service, err := accountService(cfg, serviceName, isCodexService, "Codex")
	if err != nil {
		return false, err
	}
	if err := validateCodexLogin(login); err != nil {
		return false, err
	}
	if err := validateAccountName(accountName); err != nil {
		return false, err
	}
	if email == "" {
		email = CodexLoginEmail(login)
	}
	created, err := ensureAccount(cfg, service, accountName, email)
	if err != nil {
		return false, err
	}
	if err := writePrivateFile(filepath.Join(AccountDir(cfg, service.Name, accountName), "auth.json"), login); err != nil {
		if created {
			_ = RemoveAccountWithOptions(cfg, service.Name, accountName, true, true)
		}
		return false, err
	}
	return created, ResetAccountProbeState(cfg, service.Name, accountName)
}

// UnregisterAccount unregisters an account and deletes its Claude setup token.
// The home stays unless deleteHome is set.
func UnregisterAccount(ctx context.Context, cfg Config, serviceName, accountName string, force, deleteHome bool) error {
	service, ok := cfg.Service(serviceName)
	if !ok {
		return fmt.Errorf("service %q not found", serviceName)
	}
	state, err := LoadState(cfg.StatePath)
	if err != nil {
		return err
	}
	serviceState := state.Service(service.Name)
	account, ok := serviceState.Accounts[accountName]
	if !ok {
		return fmt.Errorf("account %q not found for service %q", accountName, service.Name)
	}
	if serviceState.ActiveAccount == accountName && !force {
		return fmt.Errorf("account %q is active; switch away first or pass -force", accountName)
	}
	if isClaudeService(service) && service.UsesAccountHomes() {
		_, tokenExists, err := readClaudeSetupTokenEnvelope(cfg, service.Name, accountName)
		if err != nil {
			return err
		}
		if tokenExists || account.SetupTokenRevision != "" {
			if err := RemoveClaudeSetupToken(ctx, cfg, service.Name, accountName); err != nil {
				return err
			}
		}
	}
	return RemoveAccountWithOptions(cfg, service.Name, accountName, force, deleteHome)
}
