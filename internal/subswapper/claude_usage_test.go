package subswapper

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestFetchClaudeUsageWithCredentials(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer access-token" {
			t.Fatalf("unexpected authorization header %q", got)
		}
		if got := r.Header.Get("anthropic-beta"); got != claudeOAuthBetaHeader {
			t.Fatalf("unexpected beta header %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"five_hour": {"utilization": 12, "resets_at": "2030-07-02T01:49:59Z"},
			"seven_day": {"utilization": 34, "resets_at": "2030-07-05T03:59:59Z"},
			"limits": [
				{
					"kind": "weekly_scoped",
					"group": "weekly",
					"percent": 56,
					"resets_at": "2030-07-04T13:00:00Z",
					"scope": {"model": {"display_name": "Fable"}}
				}
			]
		}`))
	}))
	t.Cleanup(server.Close)

	oldURL := claudeUsageURL
	claudeUsageURL = server.URL
	t.Cleanup(func() { claudeUsageURL = oldURL })

	credentials := []byte(`{"claudeAiOauth":{"accessToken":"access-token"}}`)
	usage, err := fetchClaudeUsageWithCredentials(context.Background(), credentials)
	if err != nil {
		t.Fatal(err)
	}
	fiveHour, ok := usage.FiveHour.Ratio()
	if !ok || fiveHour != 0.12 {
		t.Fatalf("unexpected five-hour ratio %v %v", fiveHour, ok)
	}
	weekly, ok := usage.Weekly.Ratio()
	if !ok || weekly != 0.34 {
		t.Fatalf("unexpected weekly ratio %v %v", weekly, ok)
	}
	fableWeekly, ok := usage.FableWeekly.Ratio()
	if !ok || fableWeekly != 0.56 {
		t.Fatalf("unexpected Fable weekly ratio %v %v", fableWeekly, ok)
	}
}

func TestRefreshClaudeCredentials(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]string
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if body["refresh_token"] != "refresh-token" {
			t.Fatalf("unexpected refresh token %q", body["refresh_token"])
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"access_token": "new-access",
			"expires_in": 3600,
			"refresh_token": "new-refresh",
			"scope": "one two"
		}`))
	}))
	t.Cleanup(server.Close)

	oldURL := claudeTokenURL
	claudeTokenURL = server.URL
	t.Cleanup(func() { claudeTokenURL = oldURL })

	credentials := []byte(`{"claudeAiOauth":{"accessToken":"old","refreshToken":"refresh-token","expiresAt":1}}`)
	refreshed, err := refreshClaudeCredentials(context.Background(), credentials)
	if err != nil {
		t.Fatal(err)
	}
	oauth, err := parseClaudeOAuth(refreshed)
	if err != nil {
		t.Fatal(err)
	}
	if oauth.AccessToken != "new-access" || oauth.RefreshToken != "new-refresh" {
		t.Fatalf("unexpected refreshed oauth %#v", oauth)
	}
	if len(oauth.Scopes) != 2 || oauth.Scopes[0] != "one" || oauth.Scopes[1] != "two" {
		t.Fatalf("unexpected scopes %#v", oauth.Scopes)
	}
}
