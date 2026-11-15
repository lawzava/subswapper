package subswapper

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"io"
	"math"
	"net/http"
	"time"
)

const (
	// codexSocketInspectLimit bounds the copy of one text message kept for
	// classification. Error frames are small; larger messages are relayed
	// unclassified.
	codexSocketInspectLimit = 64 << 10
	// codexSocketQuotaFallback is how long an in-socket quota rejection keeps
	// the account ranked last when neither the frame nor the usage endpoint
	// gives a reset time. It matches the usage TTL: after it, a fresh sample
	// or another rejection decides again, so a wrong guess costs one turn.
	codexSocketQuotaFallback = codexProxyUsageTTL
	// codexSocketQuotaMaxReset rejects reset times beyond the weekly window,
	// so a malformed frame cannot park an account for months.
	codexSocketQuotaMaxReset = 8 * 24 * time.Hour
	// codexSocketCloseTryAgainLater (RFC 6455 registry, 1013) tells the
	// client to reconnect later; its next handshake routes to another account.
	codexSocketCloseTryAgainLater = 1013
)

// codexSocketRejectionWindow bounds everything an in-socket rejection does
// after the error frame: the marker write, the usage fetch and the refined
// write. The fetch alone may take codexProxyUsageFetchWindow (10s); 5s more
// covers two state-lock waits, which normally last milliseconds because
// every holder only loads and saves the state file. If another process holds
// the lock longer, the handler gives up instead of waiting without bound.
// A variable so tests can shorten it.
var codexSocketRejectionWindow = codexProxyUsageFetchWindow + 5*time.Second

type codexSocketRejection int

const (
	codexSocketAccepted codexSocketRejection = iota
	codexSocketQuota
	codexSocketUnauthorized
)

// codexSocketVerdict is what one complete server text message revealed
// about the account that holds the socket.
type codexSocketVerdict struct {
	kind     codexSocketRejection
	resetsAt time.Time
}

// relayCodexServerFrames copies server frames to dst unchanged and returns
// at the first message that rejects the account, after forwarding it in
// full. Every byte is written as soon as it is read, so normal streaming
// latency is unchanged. A frame with RSV bits set is compressed or uses an
// extension this relay did not negotiate, so the rest of the stream passes
// through uninspected rather than being mis-parsed.
func relayCodexServerFrames(dst io.Writer, src io.Reader) (codexSocketVerdict, error) {
	reader := bufio.NewReaderSize(src, 32<<10)
	chunk := make([]byte, 32<<10)
	var header [14]byte
	var message []byte
	collecting, overflow := false, false
	passThrough := func(prefix []byte) (codexSocketVerdict, error) {
		if _, err := dst.Write(prefix); err != nil {
			return codexSocketVerdict{}, err
		}
		_, err := io.Copy(dst, reader)
		return codexSocketVerdict{}, err
	}
	for {
		if n, err := io.ReadFull(reader, header[:2]); err != nil {
			_, _ = dst.Write(header[:n])
			return codexSocketVerdict{}, err
		}
		if header[0]&0x70 != 0 {
			return passThrough(header[:2])
		}
		fin := header[0]&0x80 != 0
		opcode := header[0] & 0x0f
		masked := header[1]&0x80 != 0
		length := uint64(header[1] & 0x7f)
		extended := 0
		switch length {
		case 126:
			extended = 2
		case 127:
			extended = 8
		}
		size := 2 + extended
		if masked {
			size += 4
		}
		if n, err := io.ReadFull(reader, header[2:size]); err != nil {
			_, _ = dst.Write(header[:2+n])
			return codexSocketVerdict{}, err
		}
		switch extended {
		case 2:
			length = uint64(binary.BigEndian.Uint16(header[2:4]))
		case 8:
			length = binary.BigEndian.Uint64(header[2:10])
		}
		if length > math.MaxInt64 {
			// RFC 6455 forbids the top bit; stop parsing a broken stream.
			return passThrough(header[:size])
		}
		var mask [4]byte
		if masked {
			// Servers must not mask, but tolerate it for the inspected copy.
			copy(mask[:], header[size-4:size])
		}
		if _, err := dst.Write(header[:size]); err != nil {
			return codexSocketVerdict{}, err
		}
		control := opcode&0x08 != 0
		if !control {
			switch opcode {
			case 0x1:
				collecting, overflow, message = true, false, message[:0]
			case 0x0:
				// A continuation extends the current message.
			default:
				collecting = false
			}
		}
		keep := !control && collecting
		for offset := uint64(0); offset < length; {
			n, err := reader.Read(chunk[:min(uint64(len(chunk)), length-offset)])
			if n > 0 {
				if _, werr := dst.Write(chunk[:n]); werr != nil {
					return codexSocketVerdict{}, werr
				}
				if keep && !overflow {
					// Messages over the limit are deliberately not
					// classified: Codex error frames are a few hundred
					// bytes, and bounding the copy bounds memory per socket.
					if len(message)+n > codexSocketInspectLimit {
						overflow, message = true, message[:0]
					} else {
						start := len(message)
						message = append(message, chunk[:n]...)
						if masked {
							for index := range n {
								message[start+index] ^= mask[(offset+uint64(index))%4]
							}
						}
					}
				}
				offset += uint64(n)
			}
			if err != nil {
				if err == io.EOF {
					err = io.ErrUnexpectedEOF
				}
				return codexSocketVerdict{}, err
			}
		}
		if control || !fin {
			continue
		}
		if collecting && !overflow {
			if verdict := classifyCodexSocketMessage(message); verdict.kind != codexSocketAccepted {
				return verdict, nil
			}
		}
		collecting, overflow, message = false, false, message[:0]
	}
}

// classifyCodexSocketMessage reads a server text message the way Codex does
// (codex-rs responses_websocket.rs): only {"type":"error"} with a status is
// an HTTP-equivalent error. A 429 needs the same quota markers as the HTTP
// path; other errors, including websocket_connection_limit_reached and
// throttles, stay on the socket. response.failed events are not classified,
// because the HTTP path never classifies them inside a 200 stream either.
func classifyCodexSocketMessage(message []byte) codexSocketVerdict {
	if !bytes.Contains(message, []byte(`"error"`)) {
		return codexSocketVerdict{}
	}
	var frame struct {
		Type       string          `json:"type"`
		Status     *int            `json:"status"`
		StatusCode *int            `json:"status_code"`
		Error      json.RawMessage `json:"error"`
	}
	if json.Unmarshal(message, &frame) != nil || frame.Type != "error" {
		return codexSocketVerdict{}
	}
	status := frame.Status
	if status == nil {
		status = frame.StatusCode
	}
	if status == nil {
		return codexSocketVerdict{}
	}
	switch *status {
	case http.StatusUnauthorized:
		return codexSocketVerdict{kind: codexSocketUnauthorized}
	case http.StatusTooManyRequests:
		if !codexQuotaRejected(frame.Error) {
			return codexSocketVerdict{}
		}
		verdict := codexSocketVerdict{kind: codexSocketQuota}
		var detail struct {
			ResetsAt float64 `json:"resets_at"`
		}
		if json.Unmarshal(frame.Error, &detail) == nil && detail.ResetsAt > 0 && detail.ResetsAt < 1e12 {
			verdict.resetsAt = time.Unix(int64(detail.ResetsAt), 0).UTC()
		}
		return verdict
	}
	return codexSocketVerdict{}
}

// codexSocketQuotaUsage is the usage recorded after an in-socket quota
// rejection. A fetched sample that already reads exhausted wins. Otherwise
// one window is forced to 100% until the rejection's reset, so the account
// ranks last on the next handshake and recovers on its own at the reset.
func codexSocketQuotaUsage(fetched *UsageSnapshot, resetsAt, now time.Time) UsageSnapshot {
	if fetched != nil && fetched.Exhausted() {
		return *fetched
	}
	if !resetsAt.After(now) || resetsAt.After(now.Add(codexSocketQuotaMaxReset)) {
		resetsAt = now.Add(codexSocketQuotaFallback)
	}
	// A merged sample keeps the fetch's start time as its ordering stamp;
	// the provisional marker is observed now.
	usage := UsageSnapshot{Source: claudeUsageSourceProxy, ObservedAt: now}
	if fetched != nil {
		usage = *fetched
	}
	window := LimitWindow{ResetsAt: resetsAt}
	forceLimitReached(&window)
	// Same slotting as parseCodexWhamUsage: windows up to six hours are the
	// short one.
	if resetsAt.Sub(now) <= 6*time.Hour {
		usage.FiveHour = window
	} else {
		usage.Weekly = window
	}
	return usage
}

// recordCodexSocketRejection stores what an in-socket rejection revealed
// before the socket closes, so the reconnect the close triggers already
// routes elsewhere. It runs no network call; ctx bounds the state-lock wait.
func (p *CodexProxy) recordCodexSocketRejection(ctx context.Context, route codexProxyRoute, verdict codexSocketVerdict) {
	var err error
	switch verdict.kind {
	case codexSocketUnauthorized:
		p.logf("codex proxy: WebSocket token rejected for %s; closing the socket", route.Account)
		err = recordCodexProxyState(ctx, p.cfg, p.service, route, nil, true, false, false)
	case codexSocketQuota:
		p.logf("codex proxy: WebSocket quota reached for %s; closing the socket", route.Account)
		usage := codexSocketQuotaUsage(nil, verdict.resetsAt, codexProxyNow().UTC())
		err = recordCodexProxyState(ctx, p.cfg, p.service, route, &usage, false, false, true)
	default:
		return
	}
	if err != nil {
		p.logf("codex proxy: record state for %s: %v", route.Account, err)
	}
}

// refineCodexSocketQuota replaces the provisional quota marker with real
// numbers when the usage endpoint answers. ctx must outlive the socket and
// bounds both the fetch and the state-lock wait.
func (p *CodexProxy) refineCodexSocketQuota(ctx context.Context, route codexProxyRoute, verdict codexSocketVerdict) {
	fetched, err := p.fetchUsage(ctx, route)
	if err != nil {
		p.logf("codex proxy: usage for %s unavailable: %v", route.Account, sanitizeProbeError(err))
		return
	}
	usage := codexSocketQuotaUsage(&fetched, verdict.resetsAt, codexProxyNow().UTC())
	if err := recordCodexProxyState(ctx, p.cfg, p.service, route, &usage, false, false, true); err != nil {
		p.logf("codex proxy: record state for %s: %v", route.Account, err)
	}
}

// codexSocketCloseFrame builds an unmasked server close frame.
func codexSocketCloseFrame(code uint16, reason string) []byte {
	if len(reason) > 123 {
		reason = reason[:123]
	}
	frame := []byte{0x88, byte(2 + len(reason)), byte(code >> 8), byte(code)}
	return append(frame, reason...)
}
