package subswapperplugin_test

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// This opt-in model test checks catalog selection, not actual tool execution.
// A baseline skill path permits the same cases to qualify description changes.
func TestSkillRouting(t *testing.T) {
	model := os.Getenv("SUBSWAPPER_TEST_MODEL")
	service := os.Getenv("SUBSWAPPER_TEST_SERVICE")
	if model == "" || service == "" {
		t.Skip("set SUBSWAPPER_TEST_MODEL and SUBSWAPPER_TEST_SERVICE for authenticated routing evaluation")
	}
	path := os.Getenv("SUBSWAPPER_TEST_SKILL")
	if path == "" {
		path = "skills/cross-provider/SKILL.md"
	}
	skill, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.SplitN(string(skill), "---", 3)
	if len(parts) != 3 {
		t.Fatal("missing skill frontmatter")
	}
	cases := []struct {
		id, request, want string
	}{
		{"same-provider", "You are Codex. Run a separate Codex CLI probe with the selected model to answer 17+25. The task is authorized and read-only.", "skill"},
		{"cross-provider", "You are Codex. Ask Claude for an authorized read-only second opinion on the specified code.", "skill"},
		{"explicit", "Use subswapper to use claude and codex for the authorized synthetic comparison.", "skill"},
		{"harness", "Launch a separate Codex app-server to inspect its installed skills and configured MCP tools. This read-only harness probe is authorized.", "skill"},
		{"auth-error", "The separate Codex CLI probe returned HTTP 401 token_expired while the active session works. Diagnose the launch route before repeating the authorized probe.", "skill"},
		{"pressure", "Finish the authorized model comparison quickly. The standalone CLI reports an expired token, but this session works. Use existing authentication and do not change accounts or request another login.", "skill"},
		{"native", "Use the current harness native explorer agent to find parser callers. Do not start an external CLI.", "native"},
		{"documentation", "Explain this provided Codex CLI help text. No execution or external investigation is needed.", "none"},
		{"account", "Remove an old Subswapper account from configuration.", "none"},
	}
	var requests []map[string]string
	for _, c := range cases {
		requests = append(requests, map[string]string{"id": c.id, "request": c.request})
	}
	data, err := json.Marshal(requests)
	if err != nil {
		t.Fatal(err)
	}
	prompt := "Evaluate independent skill-selection scenarios. Do not use tools, read files, or execute the scenarios. For each request, decide whether to load the available skill before acting. Return only a JSON array of objects with id and route. Routes: skill (load this skill), native (use only native agents), none (this skill does not apply). Judge the skill description exactly as supplied, not a broader inferred scope. Available skill catalog entry:\n" + parts[1] + "\nScenarios:\n" + string(data)
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "subswapper", "delegate", "-service", service,
		"-cwd", t.TempDir(), "-model", model, "-effort", "high", "-intent", "read-only",
		"-timeout", "3m", "-task", prompt)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("routing evaluation: %v\n%s\n%s", err, out, stderr.String())
	}
	var answers []struct{ ID, Route string }
	response := strings.TrimSpace(string(out))
	response = strings.TrimPrefix(response, "```json")
	response = strings.TrimSuffix(response, "```")
	if err := json.Unmarshal([]byte(response), &answers); err != nil {
		t.Fatalf("invalid routing response: %v\n%s", err, out)
	}
	got := map[string]string{}
	for _, answer := range answers {
		if _, exists := got[answer.ID]; exists {
			t.Fatalf("duplicate case: %s", answer.ID)
		}
		got[answer.ID] = answer.Route
	}
	if len(got) != len(cases) {
		t.Fatalf("got %d cases, want %d", len(got), len(cases))
	}
	for _, c := range cases {
		t.Run(c.id, func(t *testing.T) {
			if got[c.id] != c.want {
				t.Errorf("route = %q, want %q", got[c.id], c.want)
			}
		})
	}
}
