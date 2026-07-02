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
	lock, err := AcquireStateLock(cfg)
	if err != nil {
		return CycleResult{}, err
	}
	defer lock.Release()

	state, err := LoadState(cfg.StatePath)
	if err != nil {
		return CycleResult{}, err
	}
	cycle := CycleResult{Results: CollectAll(ctx, cfg, state)}
	if err := SaveState(cfg.StatePath, state); err != nil {
		return CycleResult{}, err
	}
	return cycle, nil
}

func MonitorOnce(ctx context.Context, cfg Config, autoSwitch bool) CycleResult {
	lock, err := AcquireStateLock(cfg)
	if err != nil {
		return CycleResult{Errors: []error{err}}
	}
	defer lock.Release()

	state, err := LoadState(cfg.StatePath)
	if err != nil {
		return CycleResult{Errors: []error{err}}
	}
	cycle := CycleResult{Results: CollectAll(ctx, cfg, state)}
	if autoSwitch {
		now := time.Now().UTC()
		for index := range cycle.Results {
			result := &cycle.Results[index]
			serviceState := state.Service(result.Service.Name)
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
			if err := switchServiceFiles(cfg, result.Service, state, best.Account.Name); err != nil {
				cycle.Errors = append(cycle.Errors, fmt.Errorf("switch %s to %s: %w", result.Service.Name, best.Account.Name, err))
				continue
			}
			serviceState.ActiveAccount = best.Account.Name
			serviceState.LastSwitchedAt = now
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
	lock, err := AcquireStateLock(cfg)
	if err != nil {
		return nil, err
	}
	defer lock.Release()

	state, err := LoadState(cfg.StatePath)
	if err != nil {
		return nil, err
	}
	all := serviceName == "all"
	var switches []SwitchEvent
	var errs []error
	matched := false
	actionable := false
	for _, service := range cfg.Services {
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
		result := CollectService(ctx, cfg, state, service)
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
		if err := switchServiceFiles(cfg, service, state, best.Account.Name); err != nil {
			err = fmt.Errorf("switch %s to %s: %w", service.Name, best.Account.Name, err)
			if !all {
				return nil, err
			}
			errs = append(errs, err)
			continue
		}
		serviceState.ActiveAccount = best.Account.Name
		serviceState.LastSwitchedAt = time.Now().UTC()
		// Persist immediately so a later failure cannot leave the swap
		// unrecorded while the files are already on disk.
		if err := SaveState(cfg.StatePath, state); err != nil {
			return switches, err
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
