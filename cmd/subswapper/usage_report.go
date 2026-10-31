package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"time"

	"github.com/lawzava/subswapper/internal/subswapper"
)

// runUsageReport summarizes the usage history the hub's monitor wrote. A
// hub client holds no history, so it points the user at the hub.
func runUsageReport(args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("usage-report", flag.ContinueOnError)
	configPath := fs.String("config", defaultConfigPath, "config file")
	serviceName := fs.String("service", "", "report only this service")
	since := fs.Duration("since", subswapper.DefaultUsageReportSince, "how far back to report, e.g. 336h")
	asJSON := fs.Bool("json", false, "print a machine-readable report")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *since <= 0 {
		return errors.New("-since must be a positive duration")
	}
	cfg, err := subswapper.LoadConfig(*configPath)
	if err != nil {
		return err
	}
	if err := requireLocalHistory(*cfg, *serviceName); err != nil {
		return err
	}
	now := time.Now().UTC()
	records, err := subswapper.ReadUsageHistory(subswapper.UsageHistoryPath(*cfg), now.Add(-*since))
	if err != nil {
		return err
	}
	report := subswapper.BuildUsageReport(records, subswapper.UsageReportOptions{
		Service:   *serviceName,
		Since:     now.Add(-*since),
		Until:     now,
		Threshold: cfg.Monitor.SwitchThresholdRatio(),
	})
	if *asJSON {
		encoder := json.NewEncoder(stdout)
		encoder.SetIndent("", "  ")
		return encoder.Encode(report)
	}
	_, err = io.WriteString(stdout, subswapper.RenderUsageReport(report))
	return err
}

// requireLocalHistory fails when the named service, or every service, is
// served by a hub: only the hub owns accounts and writes history.
func requireLocalHistory(cfg subswapper.Config, serviceName string) error {
	if serviceName != "" {
		service, ok := cfg.Service(serviceName)
		if !ok {
			return fmt.Errorf("service %q not found", serviceName)
		}
		if service.HubClient() {
			return errHistoryOnHub(serviceName)
		}
		return nil
	}
	clients := 0
	for _, service := range cfg.Services {
		if service.HubClient() {
			clients++
		}
	}
	if clients > 0 && clients == len(cfg.Services) {
		return errHistoryOnHub("")
	}
	return nil
}

func errHistoryOnHub(serviceName string) error {
	if serviceName == "" {
		return errors.New("this machine uses a hub for its accounts and keeps no usage history; run it on the hub")
	}
	return fmt.Errorf("service %q runs on a hub, which keeps the usage history; run it on the hub", serviceName)
}
