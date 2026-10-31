package subswapper

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"time"
)

// The usage history is an append-only JSONL log next to the state file. It
// records what the monitor already observes so an operator can judge whether
// a pool has too few or too many accounts. It holds no credentials, token
// revisions, or emails. Every writer already holds the state lock, which
// also serializes the daily prune against appends.
const (
	usageHistoryFile = "usage-history.jsonl"
	// usageHistoryRetention is how far back records are kept.
	usageHistoryRetention = 120 * 24 * time.Hour
	// usageHistoryPruneSlack delays a prune until the oldest record is this
	// far past retention, so the file is rewritten about once a day.
	usageHistoryPruneSlack = 24 * time.Hour
	// usageHistoryHeartbeat bounds how long an unchanged account goes
	// without a record; readers treat longer gaps as the monitor being down.
	usageHistoryHeartbeat = time.Hour
	// usageHistoryTailBytes is how much of the file the dedupe check reads.
	// A prior sample missing from the tail only costs one extra record.
	usageHistoryTailBytes = 256 << 10
	usageHistoryMaxLine   = 1 << 20

	HistoryTypeSample = "sample"
	HistoryTypeSwitch = "switch"

	HistorySwitchAuto   = "auto"   // monitor cycle
	HistorySwitchBest   = "best"   // `switch <service> auto`
	HistorySwitchManual = "manual" // explicit account
)

// UsageHistoryRecord is one line of the usage history. Sample records carry
// the account's usage; switch records carry From, To, and Reason. A window
// that is absent or already reset is omitted.
type UsageHistoryRecord struct {
	Time        time.Time      `json:"t"`
	Type        string         `json:"type"`
	Service     string         `json:"service"`
	Account     string         `json:"account,omitempty"`
	Active      bool           `json:"active,omitempty"`
	Selectable  bool           `json:"selectable,omitempty"`
	State       string         `json:"state,omitempty"`
	FiveHour    *HistoryWindow `json:"five_hour,omitempty"`
	Weekly      *HistoryWindow `json:"weekly,omitempty"`
	FableWeekly *HistoryWindow `json:"fable_weekly,omitempty"`
	ObservedAt  time.Time      `json:"observed_at,omitzero"`
	Source      string         `json:"source,omitempty"`
	From        string         `json:"from,omitempty"`
	To          string         `json:"to,omitempty"`
	Reason      string         `json:"reason,omitempty"`
}

type HistoryWindow struct {
	Pct      float64   `json:"pct"`
	ResetsAt time.Time `json:"resets_at,omitzero"`
}

// usageHistoryLogf reports history failures, which never fail the caller.
var usageHistoryLogf = func(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
}

func usageHistoryPath(cfg Config) string {
	return filepath.Join(filepath.Dir(ExpandPath(cfg.StatePath)), usageHistoryFile)
}

// recordUsageSamples appends one sample per account of every service this
// machine owns. A hub client holds no accounts and writes nothing. The
// caller holds the state lock.
func recordUsageSamples(cfg Config, now time.Time, results []ServiceStatus) {
	now = now.UTC().Truncate(time.Second)
	var samples []UsageHistoryRecord
	for _, result := range results {
		if result.Service.Disabled || result.Service.HubClient() {
			continue
		}
		for _, account := range result.Accounts {
			samples = append(samples, historySample(now, result.Service.Name, account))
		}
	}
	if len(samples) == 0 {
		return
	}
	path := usageHistoryPath(cfg)
	last, err := lastHistorySamples(path)
	if err != nil {
		usageHistoryLogf("usage history: %v", err)
		return
	}
	fresh := samples[:0]
	for _, sample := range samples {
		if previous, ok := last[historyKey(sample)]; ok && sameHistorySample(previous, sample) &&
			now.Sub(previous.Time) < usageHistoryHeartbeat {
			continue
		}
		fresh = append(fresh, sample)
	}
	appendUsageHistory(path, now, fresh...)
}

// recordUsageSwitch appends a switch event. The caller holds the state lock.
func recordUsageSwitch(cfg Config, now time.Time, service ServiceConfig, from, to, reason string) {
	if service.HubClient() {
		return
	}
	now = now.UTC().Truncate(time.Second)
	appendUsageHistory(usageHistoryPath(cfg), now, UsageHistoryRecord{
		Time: now, Type: HistoryTypeSwitch, Service: service.Name, From: from, To: to, Reason: reason,
	})
}

func historySample(now time.Time, service string, status AccountStatus) UsageHistoryRecord {
	usage := status.Account.Usage
	record := UsageHistoryRecord{
		Time:        now,
		Type:        HistoryTypeSample,
		Service:     service,
		Account:     status.Account.Name,
		Active:      status.Active,
		Selectable:  status.Selectable,
		State:       historyState(status),
		FiveHour:    historyWindow(usage.FiveHour, now),
		Weekly:      historyWindow(usage.Weekly, now),
		FableWeekly: historyWindow(usage.FableWeekly, now),
		Source:      usage.Source,
	}
	if !usage.ObservedAt.IsZero() {
		record.ObservedAt = usage.ObservedAt.UTC().Truncate(time.Second)
	}
	return record
}

// historyState is a coarse, free-text-free reason, so provider errors never
// reach the log.
func historyState(status AccountStatus) string {
	switch {
	case status.Selectable:
		return "ready"
	case status.Account.CredentialsError != "":
		return "credentials"
	case !status.Account.Usage.HasLimits():
		return "no usage"
	case status.Account.Usage.Exhausted():
		return "limit reached"
	default:
		return "unavailable"
	}
}

// historyWindow returns nil for a missing window and for one whose reset has
// passed: its recorded percentage no longer describes anything.
func historyWindow(w LimitWindow, now time.Time) *HistoryWindow {
	if w.Pct == nil && w.Limit <= 0 {
		return nil
	}
	if !w.ResetsAt.IsZero() && !now.Before(w.ResetsAt) {
		return nil
	}
	pct := 0.0
	if w.Pct != nil {
		pct = *w.Pct
	} else {
		pct = w.Used / w.Limit * 100
	}
	out := &HistoryWindow{Pct: math.Round(max(0, pct)*10) / 10}
	if !w.ResetsAt.IsZero() {
		out.ResetsAt = w.ResetsAt.UTC().Truncate(time.Second)
	}
	return out
}

func historyKey(record UsageHistoryRecord) string {
	return record.Service + "\x00" + record.Account
}

func sameHistorySample(a, b UsageHistoryRecord) bool {
	return a.Active == b.Active && a.Selectable == b.Selectable && a.State == b.State &&
		sameHistoryWindow(a.FiveHour, b.FiveHour) &&
		sameHistoryWindow(a.Weekly, b.Weekly) &&
		sameHistoryWindow(a.FableWeekly, b.FableWeekly)
}

func sameHistoryWindow(a, b *HistoryWindow) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.Pct == b.Pct && a.ResetsAt.Equal(b.ResetsAt)
}

// appendUsageHistory writes records and prunes when due. Failures are logged:
// history must never break a monitor cycle or a switch.
func appendUsageHistory(path string, now time.Time, records ...UsageHistoryRecord) {
	if len(records) == 0 {
		return
	}
	var buffer bytes.Buffer
	for _, record := range records {
		line, err := json.Marshal(record)
		if err != nil {
			usageHistoryLogf("usage history: %v", err)
			return
		}
		buffer.Write(line)
		buffer.WriteByte('\n')
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		usageHistoryLogf("usage history: %v", err)
		return
	}
	file, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		usageHistoryLogf("usage history: %v", err)
		return
	}
	_, err = file.Write(buffer.Bytes())
	if closeErr := file.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		usageHistoryLogf("usage history: %v", err)
		return
	}
	if err := pruneUsageHistory(path, now); err != nil {
		usageHistoryLogf("usage history prune: %v", err)
	}
}

// pruneUsageHistory drops records older than the retention window. The file
// is time-ordered, so the oldest record decides cheaply whether a rewrite is
// due; with the slack that happens about once a day.
func pruneUsageHistory(path string, now time.Time) error {
	oldest, ok, err := oldestHistoryTime(path)
	if err != nil || !ok || now.Sub(oldest) <= usageHistoryRetention+usageHistoryPruneSlack {
		return err
	}
	cutoff := now.Add(-usageHistoryRetention)
	source, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() { _ = source.Close() }()
	staged, err := os.CreateTemp(filepath.Dir(path), ".usage-history-*")
	if err != nil {
		return err
	}
	stagedPath := staged.Name()
	committed := false
	defer func() {
		if !committed {
			_ = staged.Close()
			_ = os.Remove(stagedPath)
		}
	}()
	writer := bufio.NewWriter(staged)
	err = scanHistoryLines(source, func(record UsageHistoryRecord, line []byte) {
		if !record.Time.Before(cutoff) {
			_, _ = writer.Write(line)
			_ = writer.WriteByte('\n')
		}
	})
	if err != nil {
		return err
	}
	if err := writer.Flush(); err != nil {
		return err
	}
	if err := staged.Sync(); err != nil {
		return err
	}
	if err := staged.Close(); err != nil {
		return err
	}
	if err := os.Chmod(stagedPath, 0o600); err != nil {
		return err
	}
	if err := os.Rename(stagedPath, path); err != nil {
		return err
	}
	committed = true
	return nil
}

// oldestHistoryTime returns the time of the first parseable record among the
// first few lines.
func oldestHistoryTime(path string) (time.Time, bool, error) {
	file, err := os.Open(path)
	if err != nil {
		return time.Time{}, false, err
	}
	defer func() { _ = file.Close() }()
	reader := bufio.NewReader(file)
	for range 16 {
		line, err := reader.ReadBytes('\n')
		var record UsageHistoryRecord
		if json.Unmarshal(bytes.TrimSpace(line), &record) == nil && !record.Time.IsZero() {
			return record.Time, true, nil
		}
		if err != nil {
			break
		}
	}
	return time.Time{}, false, nil
}

// lastHistorySamples returns the newest sample per account found in the tail
// of the file.
func lastHistorySamples(path string) (map[string]UsageHistoryRecord, error) {
	last := map[string]UsageHistoryRecord{}
	file, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return last, nil
	}
	if err != nil {
		return nil, err
	}
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	offset := max(0, info.Size()-usageHistoryTailBytes)
	data := make([]byte, info.Size()-offset)
	if _, err := file.ReadAt(data, offset); err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	if offset > 0 {
		// Drop the partial first line.
		if index := bytes.IndexByte(data, '\n'); index >= 0 {
			data = data[index+1:]
		} else {
			data = nil
		}
	}
	err = scanHistoryLines(bytes.NewReader(data), func(record UsageHistoryRecord, _ []byte) {
		if record.Type == HistoryTypeSample {
			last[historyKey(record)] = record
		}
	})
	return last, err
}

func scanHistoryLines(r io.Reader, visit func(UsageHistoryRecord, []byte)) error {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64<<10), usageHistoryMaxLine)
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		var record UsageHistoryRecord
		if len(line) == 0 || json.Unmarshal(line, &record) != nil {
			continue
		}
		visit(record, line)
	}
	return scanner.Err()
}

// ReadUsageHistory returns the records at or after since, oldest first. A
// missing file is an empty history; malformed lines are skipped.
func ReadUsageHistory(path string, since time.Time) ([]UsageHistoryRecord, error) {
	file, err := os.Open(ExpandPath(path))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer func() { _ = file.Close() }()
	var records []UsageHistoryRecord
	err = scanHistoryLines(file, func(record UsageHistoryRecord, _ []byte) {
		if !record.Time.Before(since) {
			records = append(records, record)
		}
	})
	return records, err
}

// UsageHistoryPath is where cfg's usage history lives.
func UsageHistoryPath(cfg Config) string { return usageHistoryPath(cfg) }
