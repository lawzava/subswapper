// Command check is the development/CI gate; it is not part of cmd/subswapper.
package main

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
)

type options struct {
	tier, scope string
	ci, list    bool
}

type step struct {
	name        string
	command     []string
	versionArgs []string
	dir         string
	env         []string
	onlyOS      string
	notRun      string
	optional    bool
	cleanOutput bool
}

func main() {
	if err := run(os.Args[1:], os.Stdout); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func parseOptions(args []string, out io.Writer) (options, error) {
	var o options
	fs := flag.NewFlagSet("check", flag.ContinueOnError)
	fs.SetOutput(out)
	fs.StringVar(&o.tier, "tier", "fast", "fast or full")
	fs.StringVar(&o.scope, "scope", "all", "all, go, or plugin (CI job split)")
	fs.BoolVar(&o.ci, "ci", false, "fail when optional tools are missing")
	fs.BoolVar(&o.list, "list", false, "print the tiers and steps without running them")
	fs.Usage = func() { printMenu(out) }
	if err := fs.Parse(args); err != nil {
		return o, err
	}
	if fs.NArg() != 0 {
		return o, errors.New("unexpected positional arguments; use -list for the menu")
	}
	if o.tier != "fast" && o.tier != "full" {
		return o, fmt.Errorf("unknown tier %q: use fast or full", o.tier)
	}
	if o.scope != "all" && o.scope != "go" && o.scope != "plugin" {
		return o, fmt.Errorf("unknown scope %q: use all, go, or plugin", o.scope)
	}
	if o.tier == "fast" && o.scope == "plugin" && !o.list {
		return o, errors.New("plugin checks require -tier=full")
	}
	return o, nil
}

func printMenu(out io.Writer) {
	_, _ = fmt.Fprintln(out, `Usage (from repository root): go run ./internal/tools/check [flags]
  -tier=fast     iteration: gofmt check, go vet, go test ./... (default)
  -tier=full     CI parity: gofmt, vet, coverage, race, plus Linux-only steps
  -scope=all     all checks (default); go/plugin preserve the CI job split
  -ci           missing golangci-lint, govulncheck or npm fails the gate
  -list         print this menu; -h also prints help

Run all steps even after a failure; any failure makes the gate exit nonzero.
Missing optional tools print SKIPPED (nonfatal locally, fatal with -ci).
OS-excluded steps print NOT RUN; the gate fails if no steps ran.
Optional tool versions are printed before their checks.
Install plugin dependencies first: npm ci --no-audit --no-fund in paseo-plugin/
and plugins/opencode/ (Node 24+).
The gate does not install tools or dependencies. Coverage uses a temporary file
removed on exit. Command output streams; the final summary counts each result.
Full steps (Linux only: lint, vuln, windows, plugin-typecheck, plugin-test):`)
	for _, s := range selectSteps(options{tier: "full", scope: "all"}, "linux", "<temporary>/coverage.out") {
		_, _ = fmt.Fprintf(out, "  %-18s %s", s.name, strings.Join(s.command, " "))
		if len(s.env) > 0 {
			_, _ = fmt.Fprintf(out, " (env: %s)", strings.Join(s.env, " "))
		}
		if s.dir != "" {
			_, _ = fmt.Fprintf(out, " (in %s)", s.dir)
		}
		_, _ = fmt.Fprintln(out)
	}
}

func selectSteps(o options, goos, coverage string) []step {
	var steps []step
	if o.scope != "plugin" {
		steps = append(steps,
			step{name: "gofmt", command: []string{"gofmt", "-l", "."}, cleanOutput: true},
			step{name: "vet", command: []string{"go", "vet", "./..."}},
		)
		if o.tier == "fast" {
			steps = append(steps, step{name: "test", command: []string{"go", "test", "./..."}})
		} else {
			steps = append(steps,
				step{name: "coverage", command: []string{"go", "test", "-coverprofile=" + coverage, "-count=1", "./..."}},
				step{name: "race", command: []string{"go", "test", "-race", "-count=1", "./..."}},
			)
			steps = append(steps,
				step{name: "lint", command: []string{"golangci-lint", "run"}, versionArgs: []string{"version"}, optional: true, onlyOS: "linux"},
				step{name: "vuln", command: []string{"govulncheck", "./..."}, versionArgs: []string{"-version"}, optional: true, onlyOS: "linux"},
				step{name: "windows", command: []string{"go", "build", "./..."}, env: []string{"GOOS=windows", "GOARCH=amd64"}, onlyOS: "linux"},
			)
		}
	}
	if o.tier == "full" && o.scope != "go" {
		steps = append(steps,
			step{name: "plugin-typecheck", command: []string{"npm", "run", "typecheck"}, versionArgs: []string{"--version"}, dir: "paseo-plugin", optional: true, onlyOS: "linux"},
			step{name: "plugin-test", command: []string{"npm", "test"}, versionArgs: []string{"--version"}, dir: "paseo-plugin", optional: true, onlyOS: "linux"},
			step{name: "opencode-typecheck", command: []string{"npm", "run", "typecheck"}, versionArgs: []string{"--version"}, dir: "plugins/opencode", optional: true, onlyOS: "linux"},
			step{name: "opencode-test", command: []string{"npm", "test"}, versionArgs: []string{"--version"}, dir: "plugins/opencode", optional: true, onlyOS: "linux"},
		)
	}
	for i := range steps {
		if steps[i].onlyOS != "" && steps[i].onlyOS != goos {
			steps[i].notRun = steps[i].onlyOS + "-only"
		}
	}
	return steps
}

func run(args []string, out io.Writer) (err error) {
	o, err := parseOptions(args, out)
	if errors.Is(err, flag.ErrHelp) {
		return nil
	}
	if err != nil {
		return err
	}
	if o.list {
		printMenu(out)
		return nil
	}
	if _, err := os.Stat("go.mod"); err != nil {
		return errors.New("run the gate from the repository root (go.mod not found)")
	}
	dir, err := os.MkdirTemp("", "subswapper-check-")
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, os.RemoveAll(dir)) }()
	return runSteps(selectSteps(o, runtime.GOOS, filepath.Join(dir, "coverage.out")), o.ci, out)
}

func runSteps(steps []step, strict bool, out io.Writer) error {
	// exec may copy stdout and stderr concurrently (including gofmt's tee).
	out = &lockedWriter{out: out}
	var ran, passed, failed, skipped, notRun int
	versionsPrinted := map[string]bool{}
	for _, s := range steps {
		if s.notRun != "" {
			notRun++
			_, _ = fmt.Fprintf(out, "NOT RUN %s (%s)\n", s.name, s.notRun)
			continue
		}
		tool, err := exec.LookPath(s.command[0])
		if err != nil {
			if s.optional {
				skipped++
				_, _ = fmt.Fprintf(out, "SKIPPED %s: %s missing from PATH (required with -ci)\n", s.name, s.command[0])
			} else {
				failed++
				_, _ = fmt.Fprintf(out, "FAILED %s: %v\n", s.name, err)
			}
			continue
		}
		cmd := exec.Command(tool, s.command[1:]...)
		cmd.Dir = s.dir
		// Ambient routing, credentials and live-test flags must never reach checks.
		// Explicit step fixtures remain intentional, including version probes.
		cmd.Env = []string{}
		for _, entry := range os.Environ() {
			if !strings.HasPrefix(entry, "SUBSWAPPER_") {
				cmd.Env = append(cmd.Env, entry)
			}
		}
		cmd.Env = append(cmd.Env, s.env...)
		cmd.Stdout, cmd.Stderr = out, out
		var stdout bytes.Buffer
		if s.cleanOutput {
			cmd.Stdout = io.MultiWriter(out, &stdout)
		}
		ran++
		if len(s.versionArgs) > 0 && !versionsPrinted[tool] {
			_, _ = fmt.Fprintf(out, "VERSION %s %s\n", s.command[0], strings.Join(s.versionArgs, " "))
			version := exec.Command(tool, s.versionArgs...)
			version.Dir, version.Env = cmd.Dir, cmd.Env
			version.Stdout, version.Stderr = out, out
			if err = version.Run(); err != nil {
				err = fmt.Errorf("tool version: %w", err)
			} else {
				versionsPrinted[tool] = true
			}
		}
		if err == nil {
			_, _ = fmt.Fprintf(out, "RUN %s: %s\n", s.name, strings.Join(s.command, " "))
			err = cmd.Run()
		}
		if err == nil && s.cleanOutput && stdout.Len() > 0 {
			err = errors.New("gofmt listed unformatted files; format them before continuing")
		}
		if err != nil {
			failed++
			_, _ = fmt.Fprintf(out, "FAILED %s: %v\n", s.name, err)
		} else {
			passed++
			_, _ = fmt.Fprintf(out, "PASSED %s\n", s.name)
		}
	}
	_, _ = fmt.Fprintf(out, "SUMMARY ran=%d passed=%d failed=%d skipped=%d not-run=%d\n", ran, passed, failed, skipped, notRun)
	if ran == 0 {
		return errors.New("check gate failed: no steps ran")
	}
	if failed > 0 || (strict && skipped > 0) {
		return errors.New("check gate failed (with -ci, skipped tools are failures)")
	}
	return nil
}

type lockedWriter struct {
	mu  sync.Mutex
	out io.Writer
}

func (w *lockedWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.out.Write(p)
}
