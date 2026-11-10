package main

import (
	"bytes"
	"io"
	"os"
	"reflect"
	"strings"
	"testing"
)

func TestParseOptions(t *testing.T) {
	for _, tt := range []struct {
		name string
		args []string
		want options
		bad  bool
	}{
		{name: "defaults", want: options{tier: "fast", scope: "all"}},
		{name: "strict full Go", args: []string{"-tier=full", "-scope=go", "-ci"}, want: options{tier: "full", scope: "go", ci: true}},
		{name: "plugin", args: []string{"-tier=full", "-scope=plugin"}, want: options{tier: "full", scope: "plugin"}},
		{name: "menu", args: []string{"-list"}, want: options{tier: "fast", scope: "all", list: true}},
		{name: "unknown tier", args: []string{"-tier=slow"}, bad: true},
		{name: "unknown scope", args: []string{"-scope=other"}, bad: true},
		{name: "empty plugin tier", args: []string{"-scope=plugin"}, bad: true},
		{name: "positional", args: []string{"full"}, bad: true},
		{name: "unknown flag", args: []string{"-wat"}, bad: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseOptions(tt.args, io.Discard)
			if (err != nil) != tt.bad {
				t.Fatalf("error = %v, want error %v", err, tt.bad)
			}
			if !tt.bad && got != tt.want {
				t.Errorf("options = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestStepSelection(t *testing.T) {
	for _, tt := range []struct {
		name, tier, scope, goos string
		want                    []string
	}{
		{"fast Linux", "fast", "all", "linux", []string{"gofmt", "vet", "test"}},
		{"fast macOS", "fast", "go", "darwin", []string{"gofmt", "vet", "test"}},
		{"full Linux", "full", "all", "linux", []string{"gofmt", "vet", "coverage", "race", "lint", "vuln", "windows", "plugin-typecheck", "plugin-test", "opencode-typecheck", "opencode-test"}},
		{"full macOS", "full", "all", "darwin", []string{"gofmt", "vet", "coverage", "race"}},
		{"CI Go Linux", "full", "go", "linux", []string{"gofmt", "vet", "coverage", "race", "lint", "vuln", "windows"}},
		{"CI plugin", "full", "plugin", "linux", []string{"plugin-typecheck", "plugin-test", "opencode-typecheck", "opencode-test"}},
		{"CI plugin macOS", "full", "plugin", "darwin", nil},
	} {
		t.Run(tt.name, func(t *testing.T) {
			steps := selectSteps(options{tier: tt.tier, scope: tt.scope}, tt.goos, "coverage.out")
			var names []string
			for _, s := range steps {
				if s.notRun == "" {
					names = append(names, s.name)
				}
			}
			if !reflect.DeepEqual(names, tt.want) {
				t.Fatalf("steps = %v, want %v", names, tt.want)
			}
		})
	}
}

func TestFullCommands(t *testing.T) {
	steps := selectSteps(options{tier: "full", scope: "all"}, "linux", "coverage.out")
	want := [][]string{
		{"gofmt", "-l", "."}, {"go", "vet", "./..."},
		{"go", "test", "-coverprofile=coverage.out", "-count=1", "./..."},
		{"go", "test", "-race", "-count=1", "./..."},
		{"golangci-lint", "run"}, {"govulncheck", "./..."},
		{"go", "build", "./..."}, {"npm", "run", "typecheck"}, {"npm", "test"},
		{"npm", "run", "typecheck"}, {"npm", "test"},
	}
	for i, s := range steps {
		if !reflect.DeepEqual(s.command, want[i]) {
			t.Errorf("%s command = %v, want %v", s.name, s.command, want[i])
		}
		if s.optional != (i == 4 || i == 5 || i >= 7) {
			t.Errorf("%s optional = %v", s.name, s.optional)
		}
		var versionArgs []string
		switch s.command[0] {
		case "golangci-lint":
			versionArgs = []string{"version"}
		case "govulncheck":
			versionArgs = []string{"-version"}
		case "npm":
			versionArgs = []string{"--version"}
		}
		if !reflect.DeepEqual(s.versionArgs, versionArgs) {
			t.Errorf("%s version arguments = %v, want %v", s.name, s.versionArgs, versionArgs)
		}
		if i >= 7 && i < 9 && s.dir != "paseo-plugin" {
			t.Errorf("%s directory = %q", s.name, s.dir)
		}
		if i >= 9 && s.dir != "plugins/opencode" {
			t.Errorf("%s directory = %q", s.name, s.dir)
		}
	}
	if !reflect.DeepEqual(steps[6].env, []string{"GOOS=windows", "GOARCH=amd64"}) {
		t.Errorf("windows environment = %v", steps[6].env)
	}
	if !steps[0].cleanOutput {
		t.Error("gofmt must reject nonempty output, even with exit status zero")
	}
}

func TestMissingTools(t *testing.T) {
	for _, strict := range []bool{false, true} {
		var out bytes.Buffer
		err := runSteps([]step{processStep(t, "success", "pass"), {name: "missing", command: []string{"subswapper-check-nonexistent-tool"}, optional: true}}, strict, &out)
		if (err != nil) != strict {
			t.Errorf("strict=%v: error = %v", strict, err)
		}
		for _, text := range []string{"SKIPPED missing", "ran=1 passed=1 failed=0 skipped=1"} {
			if !strings.Contains(out.String(), text) {
				t.Errorf("missing %q in %q", text, out.String())
			}
		}
	}
	if err := runSteps([]step{{name: "required", command: []string{"subswapper-check-nonexistent-tool"}}}, false, io.Discard); err == nil {
		t.Error("missing required tool must fail locally")
	}
}

func TestRunSteps(t *testing.T) {
	makeStep := func(name, mode string, clean bool) step {
		s := processStep(t, name, mode)
		s.cleanOutput = clean
		return s
	}
	var out bytes.Buffer
	err := runSteps([]step{makeStep("exit failure", "fail", false), makeStep("format failure", "output", true), makeStep("success", "pass", false)}, false, &out)
	if err == nil {
		t.Fatal("failed checks must fail the gate")
	}
	for _, text := range []string{"FAILED exit failure", "FAILED format failure", "PASSED success", "unformatted.go", "ran=3 passed=1 failed=2 skipped=0"} {
		if !strings.Contains(out.String(), text) {
			t.Errorf("missing %q in %q", text, out.String())
		}
	}
}

func processStep(t *testing.T, name, mode string) step {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return step{name: name, command: []string{exe, "-test.run=^TestStepProcess$"}, env: []string{"CHECK_TEST_PROCESS=" + mode}}
}

func TestOSExclusions(t *testing.T) {
	steps := selectSteps(options{tier: "full", scope: "all"}, "darwin", "coverage.out")[4:]
	var out bytes.Buffer
	if err := runSteps(steps, false, &out); err == nil {
		t.Error("an OS-excluded-only selection must fail without running tools")
	}
	for _, name := range []string{"lint", "vuln", "windows", "plugin-typecheck", "plugin-test", "opencode-typecheck", "opencode-test"} {
		if text := "NOT RUN " + name + " (linux-only)"; !strings.Contains(out.String(), text) {
			t.Errorf("missing %q in %q", text, out.String())
		}
	}
	if !strings.Contains(out.String(), "ran=0 passed=0 failed=0 skipped=0 not-run=7") {
		t.Errorf("unexpected summary: %s", out.String())
	}
}

func TestNoChecksRan(t *testing.T) {
	for _, strict := range []bool{false, true} {
		if err := runSteps(nil, strict, io.Discard); err == nil {
			t.Errorf("strict=%v: empty gate must fail", strict)
		}
		if err := runSteps([]step{{name: "missing", command: []string{"subswapper-check-nonexistent-tool"}, optional: true}}, strict, io.Discard); err == nil {
			t.Errorf("strict=%v: skipped-only gate must fail", strict)
		}
	}
}

func TestToolVersionBeforeChecks(t *testing.T) {
	s := processStep(t, "tool-check", "args")
	s.command = append(s.command, "tool-check")
	s.versionArgs = []string{"-test.run=^TestStepProcess$", "tool-version"}
	s.optional = true
	var out bytes.Buffer
	if err := runSteps([]step{s, s}, false, &out); err != nil {
		t.Fatal(err)
	}
	text := out.String()
	version := strings.Index(text, "\n-test.run=^TestStepProcess$ tool-version\n")
	check := strings.Index(text, "\n-test.run=^TestStepProcess$ tool-check\n")
	if version < 0 || check < 0 || version > check {
		t.Errorf("tool version must run before checks: %s", text)
	}
	if strings.Count(text, "VERSION ") != 1 {
		t.Errorf("shared tool version must print once: %s", text)
	}
}

func TestToolVersionFailure(t *testing.T) {
	s := processStep(t, "tool-check", "fail")
	s.versionArgs = []string{"-test.run=^TestStepProcess$"}
	var out bytes.Buffer
	if err := runSteps([]step{s}, false, &out); err == nil {
		t.Error("failed version command must fail the gate")
	}
	if !strings.Contains(out.String(), "FAILED tool-check: tool version:") || strings.Contains(out.String(), "RUN tool-check:") {
		t.Errorf("failed version command must prevent the check: %s", out.String())
	}
}

func TestRunStepsIsolatesInheritedSubswapperEnvironment(t *testing.T) {
	for key, value := range map[string]string{
		"SUBSWAPPER_CODEX_PLACEHOLDER": "/synthetic/inherited-placeholder.json",
		"SUBSWAPPER_CODEX_PROXY_URL":   "http://127.0.0.1:1",
		"SUBSWAPPER_ARBITRARY":         "synthetic-inherited-value",
		"SUBSWAPPER_LIVE_TEST":         "1",
		"CHECK_TEST_RETAIN":            "synthetic-retained-value",
	} {
		t.Setenv(key, value)
	}
	s := processStep(t, "isolated environment", "environment")
	s.env = append(s.env, "SUBSWAPPER_TEST_FIXTURE=synthetic-explicit-value")
	s.versionArgs = []string{"-test.run=^TestStepProcess$"}
	var out bytes.Buffer
	err := runSteps([]step{s}, false, &out)
	// Never include captured child output or inherited values in diagnostics.
	if err != nil {
		t.Fatal("child environment was not isolated")
	}
	if strings.Count(out.String(), "synthetic environment isolated\n") != 2 {
		t.Fatal("both version and check processes must use the isolated environment")
	}
}

func TestStepProcess(t *testing.T) {
	switch os.Getenv("CHECK_TEST_PROCESS") {
	case "pass":
		os.Exit(0)
	case "output":
		_, _ = os.Stdout.WriteString("unformatted.go\n")
		os.Exit(0)
	case "fail":
		os.Exit(1)
	case "args":
		_, _ = os.Stdout.WriteString(strings.Join(os.Args[1:], " ") + "\n")
		os.Exit(0)
	case "environment":
		for _, entry := range os.Environ() {
			if strings.HasPrefix(entry, "SUBSWAPPER_") && entry != "SUBSWAPPER_TEST_FIXTURE=synthetic-explicit-value" {
				os.Exit(1)
			}
		}
		if os.Getenv("SUBSWAPPER_TEST_FIXTURE") != "synthetic-explicit-value" || os.Getenv("CHECK_TEST_RETAIN") != "synthetic-retained-value" {
			os.Exit(1)
		}
		_, _ = os.Stdout.WriteString("synthetic environment isolated\n")
		os.Exit(0)
	}
}

func TestList(t *testing.T) {
	var out bytes.Buffer
	printMenu(&out)
	for _, text := range []string{"fast", "full", "-ci", "-scope", "Linux only", "coverage", "plugin-typecheck", "SKIPPED", "all steps"} {
		if !strings.Contains(out.String(), text) {
			t.Errorf("menu missing %q: %s", text, out.String())
		}
	}
}
