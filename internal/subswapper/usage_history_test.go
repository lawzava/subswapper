package subswapper

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func historyStatus(service, account string, active, selectable bool, usage UsageSnapshot) AccountStatus {
	state := AccountState{Name: account, Email: account + "@example.com", Usage: usage}
	state.Usage.TokenRevision = "revision-secret"
	state.SetupTokenRevision = "revision-secret"
	return AccountStatus{Service: service, Account: state, Active: active, Selectable: selectable, Reason: "ready"}
}

func historyUsage(now time.Time, fiveHour *float64, weekly float64) UsageSnapshot {
	usage := UsageSnapshot{
		Weekly:     LimitWindow{Pct: PtrFloat64(weekly), ResetsAt: now.Add(48 * time.Hour)},
		ObservedAt: now,
		Source:     "status-line",
	}
	if fiveHour != nil {
		usage.FiveHour = LimitWindow{Pct: fiveHour, ResetsAt: now.Add(2 * time.Hour)}
	}
	return usage
}

func historyConfig(dir string) Config {
	return Config{StatePath: filepath.Join(dir, "state", "state.json")}
}

func readHistory(t *testing.T, cfg Config) []UsageHistoryRecord {
	t.Helper()
	records, err := ReadUsageHistory(usageHistoryPath(cfg), time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	return records
}

func TestUsageHistoryRecordsCompactSamplesNextToState(t *testing.T) {
	cfg := historyConfig(t.TempDir())
	now := time.Now().UTC().Truncate(time.Second)
	results := []ServiceStatus{{
		Service: ServiceConfig{Name: "claude", Kind: "claude"},
		Accounts: []AccountStatus{
			historyStatus("claude", "a", true, true, historyUsage(now, PtrFloat64(12.34), 40)),
			historyStatus("claude", "b", false, false, historyUsage(now, nil, 100)),
		},
	}}
	recordUsageSamples(cfg, now, results)

	if want := filepath.Join(filepath.Dir(cfg.StatePath), "usage-history.jsonl"); usageHistoryPath(cfg) != want {
		t.Fatalf("history path = %q, want %q", usageHistoryPath(cfg), want)
	}
	raw, err := os.ReadFile(usageHistoryPath(cfg))
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"revision-secret", "example.com", "token"} {
		if strings.Contains(string(raw), secret) {
			t.Fatalf("history leaks %q:\n%s", secret, raw)
		}
	}
	records := readHistory(t, cfg)
	if len(records) != 2 {
		t.Fatalf("records = %d, want 2:\n%s", len(records), raw)
	}
	a, b := records[0], records[1]
	if a.Type != HistoryTypeSample || a.Service != "claude" || a.Account != "a" || !a.Active || !a.Selectable || !a.Time.Equal(now) {
		t.Fatalf("sample a = %+v", a)
	}
	if a.FiveHour == nil || a.FiveHour.Pct != 12.3 || a.Weekly == nil || a.Weekly.Pct != 40 || a.Weekly.ResetsAt.IsZero() {
		t.Fatalf("sample a windows = %+v %+v", a.FiveHour, a.Weekly)
	}
	if a.FableWeekly != nil || a.Source != "status-line" || a.ObservedAt.IsZero() {
		t.Fatalf("sample a extras = %+v", a)
	}
	// An account without a five-hour window omits it entirely.
	if b.FiveHour != nil || b.Weekly == nil || b.Selectable || b.State != "limit reached" {
		t.Fatalf("sample b = %+v", b)
	}
	if strings.Contains(strings.SplitN(string(raw), "\n", 3)[1], "five_hour") {
		t.Fatalf("absent five-hour window was written:\n%s", raw)
	}
}

func TestUsageHistorySkipsIdenticalSamplesUntilAnHourPasses(t *testing.T) {
	cfg := historyConfig(t.TempDir())
	now := time.Now().UTC().Truncate(time.Second)
	sample := func(at time.Time, weekly float64, active bool) {
		results := []ServiceStatus{{
			Service:  ServiceConfig{Name: "codex", Kind: "codex"},
			Accounts: []AccountStatus{historyStatus("codex", "a", active, true, historyUsage(now, nil, weekly))},
		}}
		recordUsageSamples(cfg, at, results)
	}
	sample(now, 10, true)
	sample(now.Add(5*time.Minute), 10, true)   // identical: skipped
	sample(now.Add(10*time.Minute), 11, true)  // pct changed
	sample(now.Add(15*time.Minute), 11, false) // active changed
	sample(now.Add(20*time.Minute), 11, false) // identical again
	if got := len(readHistory(t, cfg)); got != 3 {
		t.Fatalf("records = %d, want 3", got)
	}
	sample(now.Add(20*time.Minute+usageHistoryHeartbeat), 11, false) // unchanged but an hour later
	if got := len(readHistory(t, cfg)); got != 4 {
		t.Fatalf("records after the hourly heartbeat = %d, want 4", got)
	}
}

func TestUsageHistoryPrunesOldRecordsAtMostDaily(t *testing.T) {
	cfg := historyConfig(t.TempDir())
	now := time.Now().UTC().Truncate(time.Second)
	old := now.Add(-usageHistoryRetention - 2*usageHistoryPruneSlack)
	fresh := now.Add(-usageHistoryRetention + 24*time.Hour)
	var seeded strings.Builder
	for _, at := range []time.Time{old, old.Add(time.Hour), fresh} {
		line, err := json.Marshal(UsageHistoryRecord{Time: at, Type: HistoryTypeSwitch, Service: "codex", From: "a", To: "b", Reason: HistorySwitchAuto})
		if err != nil {
			t.Fatal(err)
		}
		seeded.Write(line)
		seeded.WriteByte('\n')
	}
	if err := os.MkdirAll(filepath.Dir(usageHistoryPath(cfg)), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(usageHistoryPath(cfg), []byte(seeded.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	codex := ServiceConfig{Name: "codex", Kind: "codex"}
	recordUsageSwitch(cfg, now, codex, "b", "a", HistorySwitchAuto)
	records := readHistory(t, cfg)
	if len(records) != 2 || !records[0].Time.Equal(fresh) || !records[1].Time.Equal(now) {
		t.Fatalf("after prune: %+v", records)
	}
	// The oldest record is now inside the retention window plus the prune
	// slack, so the next write must not rewrite the file.
	before, err := os.Stat(usageHistoryPath(cfg))
	if err != nil {
		t.Fatal(err)
	}
	recordUsageSwitch(cfg, now.Add(time.Minute), codex, "a", "b", HistorySwitchAuto)
	after, err := os.Stat(usageHistoryPath(cfg))
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(before, after) {
		t.Fatal("history was rewritten again without need")
	}
	if got := len(readHistory(t, cfg)); got != 3 {
		t.Fatalf("records = %d, want 3", got)
	}
}

func TestSwitchesAppendHistoryRecords(t *testing.T) {
	dir := t.TempDir()
	active := filepath.Join(dir, "active.json")
	cfg := testConfig(dir, active)
	captureWithUsage(t, cfg, "codex", active, "a1", "a", 10, 95)
	captureWithUsage(t, cfg, "codex", active, "b1", "b", 10, 20)
	if err := SwitchAccount(cfg, "codex", "a"); err != nil {
		t.Fatal(err)
	}
	if err := SwitchAccount(cfg, "codex", "a"); err != nil { // no-op: no record
		t.Fatal(err)
	}
	setServiceLastSwitchedAt(t, cfg, "codex", time.Now().Add(-defaultAutoSwitchCooldown-time.Minute))

	result := MonitorOnce(testContext(t), cfg, true)
	if err := errors.Join(result.Errors...); err != nil {
		t.Fatal(err)
	}
	if len(result.Switches) != 1 {
		t.Fatalf("switches = %+v", result.Switches)
	}
	if _, err := SwitchBest(testContext(t), cfg, "codex"); err != nil {
		t.Fatal(err)
	}
	if err := SwitchAccount(cfg, "codex", "a"); err != nil {
		t.Fatal(err)
	}

	var switches []UsageHistoryRecord
	samples := 0
	for _, record := range readHistory(t, cfg) {
		switch record.Type {
		case HistoryTypeSwitch:
			switches = append(switches, record)
		case HistoryTypeSample:
			samples++
		}
	}
	want := []struct{ from, to, reason string }{
		{"b", "a", HistorySwitchManual}, // registering b selected it
		{"a", "b", HistorySwitchAuto},
		{"b", "a", HistorySwitchManual},
	}
	if len(switches) != len(want) {
		t.Fatalf("switch records = %+v", switches)
	}
	for index, w := range want {
		got := switches[index]
		if got.Service != "codex" || got.From != w.from || got.To != w.to || got.Reason != w.reason || got.Time.IsZero() {
			t.Fatalf("switch %d = %+v, want %+v", index, got, w)
		}
	}
	if samples != 2 {
		t.Fatalf("monitor cycle wrote %d samples, want one per account", samples)
	}
}

func TestMonitorOnceSurvivesHistoryWriteFailure(t *testing.T) {
	dir := t.TempDir()
	active := filepath.Join(dir, "active.json")
	cfg := testConfig(dir, active)
	captureWithUsage(t, cfg, "codex", active, "a1", "a", 100, 100)
	captureWithUsage(t, cfg, "codex", active, "b1", "b", 10, 20)
	if err := SwitchAccount(cfg, "codex", "a"); err != nil {
		t.Fatal(err)
	}
	// A directory where the history file belongs makes every write fail.
	if err := os.Remove(usageHistoryPath(cfg)); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(usageHistoryPath(cfg), 0o700); err != nil {
		t.Fatal(err)
	}
	var logged []string
	restore := usageHistoryLogf
	usageHistoryLogf = func(format string, args ...any) { logged = append(logged, format) }
	t.Cleanup(func() { usageHistoryLogf = restore })

	result := MonitorOnce(testContext(t), cfg, true)
	if err := errors.Join(result.Errors...); err != nil {
		t.Fatalf("history failure leaked into the cycle: %v", err)
	}
	if len(result.Switches) != 1 {
		t.Fatalf("switches = %+v", result.Switches)
	}
	assertActive(t, cfg, "codex", "b")
	if len(logged) == 0 {
		t.Fatal("history failure was not logged")
	}
	if _, err := SwitchBest(testContext(t), cfg, "codex"); err != nil {
		t.Fatalf("SwitchBest failed on history error: %v", err)
	}
	if err := SwitchAccount(cfg, "codex", "a"); err != nil {
		t.Fatalf("SwitchAccount failed on history error: %v", err)
	}
}

func TestHubClientServicesWriteNoHistory(t *testing.T) {
	cfg := historyConfig(t.TempDir())
	client := ServiceConfig{Name: "claude", Kind: "claude", AccountMode: AccountModeHome, HubURL: "http://hub-host:7878", SharedRuntimeHome: NativeRuntimeHome}
	if !client.HubClient() {
		t.Fatal("fixture is not a hub client")
	}
	now := time.Now().UTC()
	results := []ServiceStatus{{
		Service:  client,
		Accounts: []AccountStatus{historyStatus("claude", "hub-account", true, true, historyUsage(now, nil, 10))},
	}}
	recordUsageSamples(cfg, now, results)
	recordUsageSwitch(cfg, now, client, "a", "b", HistorySwitchAuto)
	if _, err := os.Stat(usageHistoryPath(cfg)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("hub client wrote a history file (stat err %v)", err)
	}

	cfg.Services = []ServiceConfig{client}
	cfg.ApplyDefaults()
	if result := MonitorOnce(testContext(t), cfg, true); len(result.Errors) != 0 {
		t.Fatalf("client monitor errors: %v", result.Errors)
	}
	if _, err := os.Stat(usageHistoryPath(cfg)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("hub client monitor wrote a history file (stat err %v)", err)
	}
}
