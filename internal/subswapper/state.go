package subswapper

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const StateVersion = 1

type State struct {
	Version  int                      `json:"version"`
	Services map[string]*ServiceState `json:"services"`
}

type ServiceState struct {
	ActiveAccount  string                  `json:"active_account,omitempty"`
	LastSwitchedAt time.Time               `json:"last_switched_at,omitzero"`
	Accounts       map[string]AccountState `json:"accounts"`
}

type AccountState struct {
	Name     string        `json:"name"`
	Email    string        `json:"email,omitempty"`
	Provider string        `json:"provider,omitempty"`
	Slot     string        `json:"slot,omitempty"`
	AddedAt  time.Time     `json:"added_at"`
	Usage    UsageSnapshot `json:"usage,omitzero"`
	// FetchBackoffUntil pauses usage fetches for this account after rate
	// limiting or a credentials failure; the cached snapshot is used instead.
	FetchBackoffUntil time.Time `json:"fetch_backoff_until,omitzero"`
	// CredentialsError records why the stored credentials were rejected; the
	// account stays unselectable until a fetch succeeds or it is re-captured.
	CredentialsError string `json:"credentials_error,omitempty"`
	// LastProbeError is a bounded, terminal-safe summary of the latest
	// transient provider failure. Raw provider output is never persisted.
	LastProbeError string `json:"last_probe_error,omitempty"`
	// LastProbeStartedAt orders overlapping provider probes so an older result
	// cannot replace a newer snapshot.
	LastProbeStartedAt time.Time `json:"last_probe_started_at,omitzero"`
}

func LoadState(path string) (*State, error) {
	data, err := os.ReadFile(ExpandPath(path))
	if errors.Is(err, os.ErrNotExist) {
		return NewState(), nil
	}
	if err != nil {
		return nil, err
	}
	var state State
	if err := json.Unmarshal(data, &state); err != nil {
		return nil, err
	}
	if state.Version == 0 {
		state.Version = StateVersion
	}
	if state.Services == nil {
		state.Services = map[string]*ServiceState{}
	}
	for name := range state.Services {
		if state.Services[name] == nil {
			state.Services[name] = &ServiceState{}
		}
		if state.Services[name].Accounts == nil {
			state.Services[name].Accounts = map[string]AccountState{}
		}
		for accountName, account := range state.Services[name].Accounts {
			if account.CredentialsError != "" {
				account.CredentialsError = sanitizeStoredProbeError(account.CredentialsError, true)
			}
			if account.LastProbeError != "" {
				account.LastProbeError = sanitizeStoredProbeError(account.LastProbeError, false)
			}
			state.Services[name].Accounts[accountName] = account
		}
	}
	return &state, nil
}

func sanitizeStoredProbeError(message string, credentials bool) string {
	safe := sanitizeProbeError(errors.New(message))
	lower := strings.ToLower(safe)
	switch {
	case strings.Contains(lower, "claude usage api returned"):
		return "claude usage API request failed"
	case strings.Contains(lower, "claude token refresh returned"):
		if credentials {
			return "stored credentials unusable: claude token refresh failed"
		}
		return "claude token refresh failed"
	case strings.Contains(lower, "codex app-server"):
		if credentials {
			return "stored credentials unusable: codex app-server authentication failed"
		}
		return "codex app-server probe failed"
	case strings.Contains(lower, "usage command failed"):
		return "usage command failed"
	default:
		return safe
	}
}

func NewState() *State {
	return &State{
		Version:  StateVersion,
		Services: map[string]*ServiceState{},
	}
}

func SaveState(path string, state *State) error {
	targetPath := ExpandPath(path)
	if err := os.MkdirAll(filepath.Dir(targetPath), 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(targetPath, data)
}

func (s *State) Service(name string) *ServiceState {
	if s.Services == nil {
		s.Services = map[string]*ServiceState{}
	}
	service := s.Services[name]
	if service == nil {
		service = &ServiceState{Accounts: map[string]AccountState{}}
		s.Services[name] = service
	}
	if service.Accounts == nil {
		service.Accounts = map[string]AccountState{}
	}
	return service
}

func (s *State) Account(serviceName, accountName string) (AccountState, bool) {
	service := s.Service(serviceName)
	account, ok := service.Accounts[accountName]
	return account, ok
}
