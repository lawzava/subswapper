package subswapper

import (
	"context"
	"errors"
	"fmt"
	"time"
)

type CycleResult struct {
	Results  []ServiceStatus
	Switches []SwitchEvent
	Errors   []error
}

type SwitchEvent struct {
	Service string
	Account string
}

const (
	defaultAutoSwitchThreshold      = 0.90
	defaultAutoSwitchMinImprovement = 0.10
	defaultAutoSwitchCooldown       = 30 * time.Minute
)

func StatusOnce(ctx context.Context, cfg Config) (CycleResult, error) {
	probed, results, err := collectUsageSnapshot(ctx, cfg)
	if err != nil {
		return CycleResult{}, err
	}
	lock, err := AcquireStateLock(ctx, cfg)
	if err != nil {
		return CycleResult{}, err
	}
	defer lock.Release()
	current, err := LoadState(cfg.StatePath)
	if err != nil {
		return CycleResult{}, err
	}
	mergeProbeState(current, probed)
	if err := SaveState(cfg.StatePath, current); err != nil {
		return CycleResult{}, err
	}
	return CycleResult{Results: results}, nil
}

func MonitorOnce(ctx context.Context, cfg Config, autoSwitch bool) CycleResult {
	probed, results, err := collectUsageSnapshot(ctx, cfg)
	if err != nil {
		return CycleResult{Errors: []error{err}}
	}
	cycle := CycleResult{Results: results}
	lock, err := AcquireStateLock(ctx, cfg)
	if err != nil {
		return CycleResult{Errors: []error{err}}
	}
	defer lock.Release()
	state, err := LoadState(cfg.StatePath)
	if err != nil {
		return CycleResult{Errors: []error{err}}
	}
	mergeProbeState(state, probed)
	if autoSwitch {
		now := time.Now().UTC()
		for index := range cycle.Results {
			result := &cycle.Results[index]
			serviceState := state.Service(result.Service.Name)
			if !serviceStateMatchesSnapshot(serviceState, probed.Service(result.Service.Name)) {
				cycle.Errors = append(cycle.Errors, fmt.Errorf("service %q changed during usage probe; automatic switch skipped", result.Service.Name))
				continue
			}
			if result.Service.Disabled || len(serviceState.Accounts) == 0 {
				continue
			}
			best, ok := BestAccount(result.Accounts)
			if !ok {
				cycle.Errors = append(cycle.Errors, fmt.Errorf("service %q has no selectable accounts", result.Service.Name))
				continue
			}
			if best.Active {
				continue
			}
			if !shouldAutoSwitch(cfg.Monitor, *result, best, serviceState.LastSwitchedAt, now) {
				continue
			}
			if err := switchServiceFiles(cfg, result.Service, state, best.Account.Name, now); err != nil {
				cycle.Errors = append(cycle.Errors, fmt.Errorf("switch %s to %s: %w", result.Service.Name, best.Account.Name, err))
				continue
			}
			markActive(result, best.Account.Name)
			cycle.Switches = append(cycle.Switches, SwitchEvent{Service: result.Service.Name, Account: best.Account.Name})
		}
	}
	if err := SaveState(cfg.StatePath, state); err != nil {
		cycle.Errors = append(cycle.Errors, err)
	}
	return cycle
}

func SwitchBest(ctx context.Context, cfg Config, serviceName string) ([]SwitchEvent, error) {
	probed, results, err := collectUsageSnapshot(ctx, cfg)
	if err != nil {
		return nil, err
	}
	lock, err := AcquireStateLock(ctx, cfg)
	if err != nil {
		return nil, err
	}
	defer lock.Release()
	state, err := LoadState(cfg.StatePath)
	if err != nil {
		return nil, err
	}
	mergeProbeState(state, probed)
	all := serviceName == "all"
	var switches []SwitchEvent
	var errs []error
	matched := false
	actionable := false
	for index, service := range cfg.Services {
		if !all && service.Name != serviceName {
			continue
		}
		matched = true
		if service.Disabled {
			if !all {
				return nil, fmt.Errorf("service %q is disabled", serviceName)
			}
			continue
		}
		serviceState := state.Service(service.Name)
		if len(serviceState.Accounts) == 0 {
			if !all {
				return nil, fmt.Errorf("service %q has no captured accounts", serviceName)
			}
			continue
		}
		actionable = true
		if !serviceStateMatchesSnapshot(serviceState, probed.Service(service.Name)) {
			err := fmt.Errorf("service %q changed during usage probe; switch skipped", service.Name)
			if !all {
				return nil, err
			}
			errs = append(errs, err)
			continue
		}
		result := results[index]
		best, ok := BestAccount(result.Accounts)
		if !ok {
			err := fmt.Errorf("service %q has no selectable accounts", service.Name)
			if !all {
				return nil, err
			}
			errs = append(errs, err)
			continue
		}
		if best.Active {
			continue
		}
		if err := switchServiceFiles(cfg, service, state, best.Account.Name, time.Now().UTC()); err != nil {
			err = fmt.Errorf("switch %s to %s: %w", service.Name, best.Account.Name, err)
			if !all {
				return nil, err
			}
			errs = append(errs, err)
			continue
		}
		switches = append(switches, SwitchEvent{Service: service.Name, Account: best.Account.Name})
	}
	if !matched {
		return nil, fmt.Errorf("service %q not found", serviceName)
	}
	if !actionable {
		return nil, errors.New("no service has captured accounts")
	}
	if err := SaveState(cfg.StatePath, state); err != nil {
		errs = append(errs, err)
	}
	return switches, errors.Join(errs...)
}

func collectUsageSnapshot(ctx context.Context, cfg Config) (*State, []ServiceStatus, error) {
	lock, err := AcquireStateLock(ctx, cfg)
	if err != nil {
		return nil, nil, err
	}
	state, err := LoadState(cfg.StatePath)
	lock.Release()
	if err != nil {
		return nil, nil, err
	}
	startedAt := time.Now().UTC()
	for _, service := range state.Services {
		if service == nil {
			continue
		}
		for accountName, account := range service.Accounts {
			account.LastProbeStartedAt = startedAt
			service.Accounts[accountName] = account
		}
	}
	return state, CollectAll(ctx, cfg, state), nil
}

func mergeProbeState(current, probed *State) {
	for serviceName, probedService := range probed.Services {
		if probedService == nil {
			continue
		}
		currentService := current.Services[serviceName]
		if currentService == nil {
			continue
		}
		for accountName, probedAccount := range probedService.Accounts {
			currentAccount, ok := currentService.Accounts[accountName]
			if !ok || !currentAccount.AddedAt.Equal(probedAccount.AddedAt) {
				continue
			}
			if currentAccount.LastProbeStartedAt.After(probedAccount.LastProbeStartedAt) {
				continue
			}
			currentAccount.Usage = probedAccount.Usage
			currentAccount.FetchBackoffUntil = probedAccount.FetchBackoffUntil
			currentAccount.CredentialsError = probedAccount.CredentialsError
			currentAccount.LastProbeError = probedAccount.LastProbeError
			currentAccount.LastProbeStartedAt = probedAccount.LastProbeStartedAt
			currentService.Accounts[accountName] = currentAccount
		}
	}
}

func serviceStateMatchesSnapshot(current, snapshot *ServiceState) bool {
	if current == nil || snapshot == nil || current.ActiveAccount != snapshot.ActiveAccount ||
		!current.LastSwitchedAt.Equal(snapshot.LastSwitchedAt) || len(current.Accounts) != len(snapshot.Accounts) {
		return false
	}
	for name, snapshotAccount := range snapshot.Accounts {
		currentAccount, ok := current.Accounts[name]
		if !ok || !currentAccount.AddedAt.Equal(snapshotAccount.AddedAt) ||
			!currentAccount.LastProbeStartedAt.Equal(snapshotAccount.LastProbeStartedAt) {
			return false
		}
	}
	return true
}

func shouldAutoSwitch(monitor MonitorConfig, result ServiceStatus, best AccountStatus, lastSwitchedAt, now time.Time) bool {
	active, ok := activeAccountStatus(result)
	if !ok || !active.Selectable {
		// The active account is exhausted, has dead credentials, or has no
		// usage data at all: staying on it costs the user every minute, so
		// escape immediately — the cooldown and improvement margin only pace
		// optimization switches between healthy accounts.
		return true
	}
	if !active.Account.Usage.AtOrAbove(monitor.SwitchThresholdRatio()) {
		return false
	}
	if active.Score-best.Score < monitor.MinImprovementRatio() {
		return false
	}
	return lastSwitchedAt.IsZero() || !now.Before(lastSwitchedAt.Add(monitor.CooldownDuration()))
}

func activeAccountStatus(result ServiceStatus) (AccountStatus, bool) {
	for _, account := range result.Accounts {
		if account.Active {
			return account, true
		}
	}
	return AccountStatus{}, false
}

func markActive(result *ServiceStatus, accountName string) {
	for index := range result.Accounts {
		result.Accounts[index].Active = result.Accounts[index].Account.Name == accountName
	}
}
