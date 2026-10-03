package subswapper

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestFetchClaudeUsageWithSetupToken(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer setup-secret" {
			t.Fatalf("unexpected authorization header %q", got)
		}
		if got := r.Header.Get("anthropic-beta"); got != claudeOAuthBetaHeader {
			t.Fatalf("unexpected beta header %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"five_hour": {"utilization": 12, "resets_at": "2030-07-02T01:49:59Z"},
			"seven_day": {"utilization": 34, "resets_at": "2030-07-05T03:59:59Z"}
		}`))
	}))
	t.Cleanup(server.Close)

	oldURL := claudeUsageURL
	claudeUsageURL = server.URL
	t.Cleanup(func() { claudeUsageURL = oldURL })

	usage, err := fetchClaudeUsageWithSetupToken(context.Background(), "setup-secret")
	if err != nil {
		t.Fatal(err)
	}
	if !usage.HasCoreLimits() {
		t.Fatalf("setup-token usage lacks core limits: %#v", usage)
	}
}

func TestFetchClaudeUsageWithSetupTokenClassifiesUnsupportedAndRejected(t *testing.T) {
	for _, test := range []struct {
		name   string
		status int
		want   error
	}{
		{name: "unsupported", status: http.StatusForbidden, want: errSetupTokenUsageUnavailable},
		{name: "rejected", status: http.StatusUnauthorized, want: errClaudeUnauthorized},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(test.status)
				_, _ = w.Write([]byte(`{"token":"must-not-escape"}`))
			}))
			t.Cleanup(server.Close)

			oldURL := claudeUsageURL
			claudeUsageURL = server.URL
			t.Cleanup(func() { claudeUsageURL = oldURL })

			_, err := fetchClaudeUsageWithSetupToken(context.Background(), "setup-secret")
			if !errors.Is(err, test.want) {
				t.Fatalf("error = %v, want %v", err, test.want)
			}
			if err != nil && (strings.Contains(err.Error(), "setup-secret") || strings.Contains(err.Error(), "must-not-escape")) {
				t.Fatalf("secret-bearing error: %v", err)
			}
		})
	}
}

func TestFetchClaudeUsageWithSetupTokenRejectsMalformedUsage(t *testing.T) {
	for _, body := range []string{
		`{"five_hour":{"utilization":12}}`,
		`{"five_hour":{"utilization":12},"seven_day":{"utilization":34}}`,
		`{"five_hour":{"utilization":-1,"resets_at":"2030-07-02T01:49:59Z"},"seven_day":{"utilization":34,"resets_at":"2030-07-05T03:59:59Z"}}`,
		`{"five_hour":{"utilization":12,"resets_at":"2020-07-02T01:49:59Z"},"seven_day":{"utilization":34,"resets_at":"2030-07-05T03:59:59Z"}}`,
	} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(body))
		}))
		oldURL := claudeUsageURL
		claudeUsageURL = server.URL
		_, err := fetchClaudeUsageWithSetupToken(context.Background(), "setup-secret")
		claudeUsageURL = oldURL
		server.Close()
		if !errors.Is(err, errSetupTokenUsageUnavailable) {
			t.Fatalf("body %s: error = %v, want setup-token usage unavailable", body, err)
		}
	}
}

func TestLookupClaudeSetupTokenIdentity(t *testing.T) {
	for _, test := range []struct {
		name     string
		status   int
		body     string
		wantUUID string
		wantErr  error
	}{
		{name: "known", status: http.StatusOK, body: `{"account":{"uuid":"account-a"}}`, wantUUID: "account-a"},
		{name: "inference only", status: http.StatusForbidden, body: `{"error":"missing user:profile"}`},
		{name: "rejected", status: http.StatusUnauthorized, body: `{"token":"must-not-escape"}`, wantErr: errClaudeUnauthorized},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if got := r.Header.Get("Authorization"); got != "Bearer setup-secret" {
					t.Fatalf("authorization = %q", got)
				}
				w.WriteHeader(test.status)
				_, _ = w.Write([]byte(test.body))
			}))
			t.Cleanup(server.Close)

			oldURL := claudeProfileURL
			claudeProfileURL = server.URL
			t.Cleanup(func() { claudeProfileURL = oldURL })

			got, err := lookupClaudeSetupTokenIdentity(context.Background(), "setup-secret")
			if !errors.Is(err, test.wantErr) {
				t.Fatalf("error = %v, want %v", err, test.wantErr)
			}
			if got != test.wantUUID {
				t.Fatalf("account UUID = %q, want %q", got, test.wantUUID)
			}
			if err != nil && strings.Contains(err.Error(), "must-not-escape") {
				t.Fatalf("secret-bearing error: %v", err)
			}
		})
	}
}
