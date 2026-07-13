package subswapper

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

var errLiveIdentityMismatch = errors.New("live credential identity does not match captured account")

type accountIdentity struct {
	Provider string
	Value    string
}

func verifyActiveIdentity(cfg Config, service ServiceConfig, account AccountState) error {
	accountDir := AccountDir(cfg, service.Name, account.Name)
	liveIdentity, liveOK := credentialIdentity(service, func(file ManagedFile) string {
		return ExpandPath(file.Path)
	})
	backupIdentity, backupOK := credentialIdentity(service, func(file ManagedFile) string {
		return filepath.Join(accountDir, file.BackupName)
	})
	if liveOK || backupOK {
		if !liveOK || !backupOK || liveIdentity != backupIdentity {
			return fmt.Errorf("%w for %s account %q", errLiveIdentityMismatch, service.Name, account.Name)
		}
		return nil
	}

	for _, file := range service.Files {
		if !file.IsRequired() {
			continue
		}
		live, liveErr := os.ReadFile(ExpandPath(file.Path))
		backup, backupErr := os.ReadFile(filepath.Join(accountDir, file.BackupName))
		if liveErr != nil || backupErr != nil {
			continue
		}
		if !bytes.Equal(live, backup) {
			if credentialFileUnusable(service, live, backup) {
				continue
			}
			return fmt.Errorf("%w for %s account %q: stable provider identity unavailable", errLiveIdentityMismatch, service.Name, account.Name)
		}
	}
	return nil
}

func credentialIdentity(service ServiceConfig, pathFor func(ManagedFile) string) (accountIdentity, bool) {
	provider := strings.ToLower(service.Kind)
	for _, file := range service.Files {
		data, err := os.ReadFile(pathFor(file))
		if err != nil {
			continue
		}
		switch {
		case isClaudeService(service):
			var config struct {
				OAuthAccount struct {
					AccountUUID string `json:"accountUuid"`
				} `json:"oauthAccount"`
			}
			if json.Unmarshal(data, &config) == nil && config.OAuthAccount.AccountUUID != "" {
				return accountIdentity{Provider: provider, Value: config.OAuthAccount.AccountUUID}, true
			}
		case isCodexService(service):
			var auth struct {
				Tokens *struct {
					AccountID string `json:"account_id"`
				} `json:"tokens"`
			}
			if json.Unmarshal(data, &auth) == nil && auth.Tokens != nil && auth.Tokens.AccountID != "" {
				return accountIdentity{Provider: provider, Value: auth.Tokens.AccountID}, true
			}
		}
	}
	return accountIdentity{}, false
}

func credentialFileUnusable(service ServiceConfig, live, backup []byte) bool {
	switch {
	case isClaudeService(service):
		return claudeCredentialsUsable(backup) && !claudeCredentialsUsable(live)
	case isCodexService(service):
		return validateCodexAuth(backup) == nil && validateCodexAuth(live) != nil
	default:
		return false
	}
}
