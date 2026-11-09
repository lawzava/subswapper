package subswapper

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestCodexProxyReadinessIsAuthenticatedAndDoesNotProbe(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { t.Error("readiness called upstream"); w.WriteHeader(500) }))
	defer upstream.Close()
	cfg, proxy := setupCodexProxyAccounts(t, upstream.URL)
	healthRequest := httptest.NewRequest(http.MethodGet, claudeProxyHealthPath, nil)
	healthRequest.Header.Set("Authorization", "Bearer "+proxy.placeholder.Token)
	healthResponse := httptest.NewRecorder()
	proxy.ServeHTTP(healthResponse, healthRequest)
	var health struct {
		Service      string   `json:"service"`
		Version      string   `json:"version"`
		Proxy        string   `json:"proxy"`
		Capabilities []string `json:"capabilities"`
	}
	if err := json.Unmarshal(healthResponse.Body.Bytes(), &health); err != nil {
		t.Fatal(err)
	}
	if healthResponse.Code != http.StatusOK || health.Service != "codex" || health.Version != Version() || health.Proxy != "subswapper" || !slices.Contains(health.Capabilities, "codex-readiness-v1") {
		t.Fatalf("health does not advertise read-only readiness: %d %s", healthResponse.Code, healthResponse.Body.String())
	}
	check := func(token string, status int, ready bool) {
		t.Helper()
		req := httptest.NewRequest("GET", "/subswapper/codex/readiness", nil)
		req.Header.Set("Authorization", "Bearer "+token)
		w := httptest.NewRecorder()
		proxy.ServeHTTP(w, req)
		if w.Code != status || (status == 200 && !strings.Contains(w.Body.String(), fmt.Sprintf(`"selected_ready":%t`, ready))) {
			t.Fatalf("readiness: %d %s", w.Code, w.Body.String())
		}
	}
	check("wrong", 401, false)
	unauthenticated := httptest.NewRecorder()
	proxy.ServeHTTP(unauthenticated, httptest.NewRequest("GET", "/subswapper/codex/readiness", nil))
	if unauthenticated.Code != 401 {
		t.Fatalf("anonymous readiness = %d", unauthenticated.Code)
	}
	check(proxy.placeholder.Token, 200, false)
	state, err := LoadState(cfg.StatePath)
	if err != nil {
		t.Fatal(err)
	}
	account := state.Service("codex").Accounts["a"]
	account.Usage = UsageSnapshot{Weekly: LimitWindow{Pct: PtrFloat64(10), ResetsAt: time.Now().Add(time.Hour)}}
	state.Service("codex").Accounts["a"] = account
	if err := SaveState(cfg.StatePath, state); err != nil {
		t.Fatal(err)
	}
	check(proxy.placeholder.Token, 200, true)
	before, err := os.ReadFile(cfg.StatePath)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(proxy)
	defer server.Close()
	if err := CheckOpenCodeProxy(context.Background(), OpenCodeRoute{Origin: server.URL, Service: "codex"}, proxy.placeholder); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(cfg.StatePath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("health/readiness check mutated state")
	}
}

func TestCheckOpenCodeProxyRequiresAdvertisedReadiness(t *testing.T) {
	for _, capabilities := range []string{"", `,"capabilities":[]`, `,"capabilities":["unrelated-v1"]`} {
		t.Run(capabilities, func(t *testing.T) {
			statePath := filepath.Join(t.TempDir(), "state.json")
			before := []byte(`{"synthetic":"unchanged"}`)
			if err := os.WriteFile(statePath, before, 0o600); err != nil {
				t.Fatal(err)
			}
			var requests, healthRequests, readinessRequests atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				if r.Header.Get("Authorization") != "Bearer synthetic.fixture.signature" {
					t.Error("missing synthetic placeholder")
					w.WriteHeader(http.StatusUnauthorized)
					return
				}
				if r.URL.Path == claudeProxyHealthPath {
					healthRequests.Add(1)
					_, _ = fmt.Fprintf(w, `{"service":"codex","proxy":"subswapper","version":"old"%s}`, capabilities)
					return
				}
				if r.URL.Path == codexProxyReadinessPath {
					readinessRequests.Add(1)
				}
				// Old proxies forward unknown paths and can persist observations.
				if err := os.WriteFile(statePath, []byte(`{"synthetic":"mutated"}`), 0o600); err != nil {
					t.Error(err)
				}
				w.WriteHeader(http.StatusNotFound)
			}))
			defer server.Close()
			err := CheckOpenCodeProxy(context.Background(), OpenCodeRoute{Origin: server.URL, Service: "codex"}, CodexProxyPlaceholder{Token: "synthetic.fixture.signature"})
			after, readErr := os.ReadFile(statePath)
			if readErr != nil {
				t.Fatal(readErr)
			}
			if err == nil || !strings.Contains(err.Error(), "upgrade") || !strings.Contains(err.Error(), "proxy or hub") {
				t.Errorf("expected proxy/hub upgrade instruction, got %v", err)
			}
			if requests.Load() != 1 || healthRequests.Load() != 1 || readinessRequests.Load() != 0 || !bytes.Equal(before, after) {
				t.Fatalf("old proxy was not read-only: requests=%d health=%d readiness=%d state unchanged=%t", requests.Load(), healthRequests.Load(), readinessRequests.Load(), bytes.Equal(before, after))
			}
		})
	}
}

func TestCodexProxyQuotaExhaustionAndBodyFidelity(t *testing.T) {
	for _, marker := range codexQuotaMarkers {
		t.Run(marker, func(t *testing.T) {
			log := &codexCallLog{}
			last := `{"error":{"type":"` + marker + `"},"padding":"` + strings.Repeat("x", codexProxyErrorBodyLimit+20) + `"}`
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == codexProxyUsagePath {
					w.WriteHeader(503)
					return
				}
				log.record(r)
				w.Header().Set("X-Last", "yes")
				w.WriteHeader(429)
				_, _ = w.Write([]byte(last))
			}))
			defer upstream.Close()
			_, proxy := setupCodexProxyAccounts(t, upstream.URL)
			response := codexProxyRequest(t, proxy, proxy.placeholder.Token, "  {\"stream\":true}\n")
			if response.Code != 429 || response.Body.String() != last || response.Header().Get("X-Last") != "yes" {
				t.Fatalf("final response changed: status=%d bytes=%d want=%d", response.Code, response.Body.Len(), len(last))
			}
			calls := log.responses()
			if len(calls) != 2 || calls[0].AccountID == calls[1].AccountID {
				t.Fatalf("accounts not tried exactly once: %#v", calls)
			}
		})
	}
}

func TestCodexProxyTransient429FullBody(t *testing.T) {
	body := strings.Repeat("slow down ", codexProxyErrorBodyLimit/5)
	log := &codexCallLog{}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		log.record(r)
		w.Header().Set("Retry-After", "7")
		w.WriteHeader(429)
		_, _ = w.Write([]byte(body))
	}))
	defer upstream.Close()
	_, proxy := setupCodexProxyAccounts(t, upstream.URL)
	response := codexProxyRequest(t, proxy, proxy.placeholder.Token, `{}`)
	if response.Body.String() != body || response.Code != 429 || response.Header().Get("Retry-After") != "7" || len(log.responses()) != 1 {
		t.Fatalf("transient throttle changed: status=%d bytes=%d want=%d calls=%d", response.Code, response.Body.Len(), len(body), len(log.responses()))
	}
}

func TestCodexProxyQuotaCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	log := &codexCallLog{}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == codexProxyUsagePath {
			cancel()
			<-r.Context().Done()
			return
		}
		log.record(r)
		w.WriteHeader(429)
		_, _ = w.Write([]byte(`{"error":"usage_limit_reached"}`))
	}))
	defer upstream.Close()
	_, proxy := setupCodexProxyAccounts(t, upstream.URL)
	req := httptest.NewRequest("POST", "/backend-api/codex/responses", strings.NewReader(`{}`)).WithContext(ctx)
	req.Header.Set("Authorization", "Bearer "+proxy.placeholder.Token)
	w := httptest.NewRecorder()
	proxy.ServeHTTP(w, req)
	if len(log.responses()) != 1 || w.Body.Len() != 0 {
		t.Fatalf("cancelled request retried or wrote bytes: calls=%d body=%q", len(log.responses()), w.Body.String())
	}
}

func TestCodexProxyConcurrentQuotaThen401(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == codexProxyUsagePath {
			w.WriteHeader(503)
			return
		}
		if codexAccountHeader(r) == "acct-a" {
			w.WriteHeader(429)
			_, _ = fmt.Fprint(w, `{"error":"usage_limit_reached"}`)
			return
		}
		w.WriteHeader(401)
		_, _ = fmt.Fprint(w, `{"error":"token_revoked"}`)
	}))
	defer upstream.Close()
	cfg, proxy := setupCodexProxyAccounts(t, upstream.URL)
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			response := codexProxyRequest(t, proxy, proxy.placeholder.Token, `{}`)
			// Requests starting after b's rejection skip b and end at a's quota.
			if response.Code != 401 && response.Code != 429 {
				t.Errorf("status = %d", response.Code)
			}
		})
	}
	wg.Wait()
	state, err := LoadState(cfg.StatePath)
	if err != nil {
		t.Fatal(err)
	}
	if state.Service("codex").Accounts["b"].CredentialsError == "" {
		t.Fatal("fallback 401 observation lost")
	}
}
