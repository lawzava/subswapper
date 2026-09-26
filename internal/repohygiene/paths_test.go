package repohygiene

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// userHomePath matches a literal Linux or macOS home directory. Tests and
// docs derive such paths from os.UserHomeDir or t.TempDir instead, so a match
// is a developer's machine path leaking into the repository.
var userHomePath = regexp.MustCompile(`/(home|Users)/[A-Za-z0-9_][A-Za-z0-9._-]*`)

func TestNoUserHomePathLiterals(t *testing.T) {
	root, err := exec.Command("git", "rev-parse", "--show-toplevel").Output()
	if err != nil {
		t.Skipf("not a git checkout: %v", err)
	}
	dir := strings.TrimSpace(string(root))
	// Untracked files are included so a leak fails before it is committed.
	cmd := exec.Command("git", "ls-files", "-z", "--cached", "--others", "--exclude-standard")
	cmd.Dir = dir
	list, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	for name := range strings.SplitSeq(strings.TrimRight(string(list), "\x00"), "\x00") {
		path := filepath.Join(dir, name)
		info, err := os.Lstat(path)
		if err != nil || !info.Mode().IsRegular() {
			// Deleted-but-staged paths and symlinks carry no content to check.
			continue
		}
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		number := 0
		for line := range bytes.SplitSeq(data, []byte("\n")) {
			number++
			if match := userHomePath.Find(line); match != nil {
				t.Errorf("%s:%d: home directory literal %q; derive it from os.UserHomeDir or t.TempDir", name, number, match)
			}
		}
	}
}
