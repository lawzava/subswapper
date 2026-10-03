package subswapper

import "context"

// These wrappers keep the setup-token tests readable after the commands
// that used them were removed.

func LoadClaudeSetupToken(cfg Config, serviceName, accountName string) (string, error) {
	token, _, err := LoadClaudeSetupTokenWithStatus(cfg, serviceName, accountName)
	return token, err
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

func RemoveAccount(cfg Config, serviceName, accountName string, force bool) error {
	return RemoveAccountWithOptions(cfg, serviceName, accountName, force, false)
}
