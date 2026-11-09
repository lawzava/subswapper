//go:build !windows

package subswapper

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func openCodeCanonicalTempDir(t *testing.T) string {
	t.Helper()
	// macOS temp roots can contain /var -> /private/var. Keep synthetic
	// fixtures canonical without weakening production's symlink rejection.
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestOpenCodePlaceholderPrivateGroupWritableAncestor(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("private-group resolution requires Linux NSS")
	}
	if !openCodePrivateGroup(uint32(os.Getuid()), uint32(os.Getgid())) {
		t.Skip("current primary group is not verifiably private through supported Linux NSS")
	}
	dir := openCodeCanonicalTempDir(t)
	ancestor := filepath.Join(dir, "shared-mode")
	parent := filepath.Join(ancestor, "private")
	if err := os.MkdirAll(parent, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(ancestor, 0o775); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(parent, "placeholder.json")
	if err := os.WriteFile(path, []byte(`{"token":"synthetic.fixture.signature","account_id":"subswapper-proxy"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	file, err := openOpenCodePlaceholder(path)
	if err != nil {
		t.Fatalf("owner-private group writable ancestor rejected: %v", err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	for _, mode := range []os.FileMode{0o777, 0o702} {
		if err := os.Chmod(ancestor, mode); err != nil {
			t.Fatal(err)
		}
		if file, err := openOpenCodePlaceholder(path); err == nil {
			_ = file.Close()
			t.Fatalf("world writable ancestor %o accepted", mode)
		}
	}
	if err := os.Chmod(ancestor, 0o775); err != nil {
		t.Fatal(err)
	}
	for _, mode := range []os.FileMode{0o600, 0o755, 0o770} {
		if err := os.Chmod(parent, mode); err != nil {
			t.Fatal(err)
		}
		if file, err := openOpenCodePlaceholder(path); err == nil {
			_ = file.Close()
			t.Fatalf("non-private immediate parent %o accepted", mode)
		}
	}
	if err := os.Chmod(parent, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o640); err != nil {
		t.Fatal(err)
	}
	if file, err := openOpenCodePlaceholder(path); err == nil {
		_ = file.Close()
		t.Fatal("non-private placeholder accepted")
	}
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(ancestor, link); err != nil {
		t.Fatal(err)
	}
	if file, err := openOpenCodePlaceholder(filepath.Join(link, "private", "placeholder.json")); err == nil {
		_ = file.Close()
		t.Fatal("symlink ancestor accepted")
	}
}

func TestOpenCodePrivateGroupRecords(t *testing.T) {
	const owner = "owner:x:1000:1000::/synthetic/owner:/bin/sh\n"
	const group = "owner:x:1000:\n"
	for _, tt := range []struct {
		name, passwd, group string
		gid                 uint32
		want                bool
	}{
		{"private", owner, group, 1000, true},
		{"other unrelated user", owner + "other:x:1001:1001::/synthetic/other:/bin/sh\n", group, 1000, true},
		{"other unrelated group", owner, group + "unrelated:x:1001:someone\n", 1000, true},
		{"wrong primary gid", "owner:x:1000:1001::/synthetic/owner:/bin/sh\n", group, 1000, false},
		{"different directory gid", owner, group, 1001, false},
		{"explicit member", owner, "owner:x:1000:someone\n", 1000, false},
		{"explicit owner member", owner, "owner:x:1000:owner\n", 1000, false},
		{"other primary user", owner + "other:x:1001:1000::/synthetic/other:/bin/sh\n", group, 1000, false},
		{"duplicate owner", owner + owner, group, 1000, false},
		{"missing owner", "other:x:1001:1001::/synthetic/other:/bin/sh\n", group, 1000, false},
		{"duplicate group", owner, group + group, 1000, false},
		{"gid alias with members", owner, group + "alias:x:1000:someone\n", 1000, false},
		{"malformed passwd", owner + "malformed\n", group, 1000, false},
		{"malformed gid", owner, "owner:x:not-a-gid:\n", 1000, false},
		{"whitespace member", owner, "owner:x:1000: \n", 1000, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := openCodePrivateGroupRecords(1000, tt.gid, tt.passwd, tt.group); got != tt.want {
				t.Fatalf("private group = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestOpenCodePrivateGroupNSS(t *testing.T) {
	for _, config := range []string{"passwd: files systemd\ngroup: files systemd\n", "passwd: files\ngroup: files # comment\n", "passwd: files\ngroup: files\ninitgroups: files systemd\n"} {
		if !openCodePrivateGroupNSS(config) {
			t.Fatalf("local NSS rejected: %q", config)
		}
	}
	for _, config := range []string{"", "passwd: files\n", "passwd: ldap files\ngroup: files\n", "passwd: files\ngroup: compat\n", "passwd: files [SUCCESS=return]\ngroup: files\n", "passwd: systemd\ngroup: files\n", "passwd: files\npasswd: files\ngroup: files\n", "passwd: files\ngroup: files\ninitgroups: ldap\n"} {
		if openCodePrivateGroupNSS(config) {
			t.Fatalf("unsupported NSS accepted: %q", config)
		}
	}
}

func TestOpenCodePlaceholderGroupPolicyDeterministic(t *testing.T) {
	dir := openCodeCanonicalTempDir(t)
	ancestor := filepath.Join(dir, "ancestor")
	parent := filepath.Join(ancestor, "private")
	if err := os.MkdirAll(parent, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(ancestor, 0o775); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(parent, "placeholder.json")
	if err := os.WriteFile(path, []byte("synthetic"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, scenario := range []string{"private", "explicit member", "other primary user", "wrong primary gid", "unavailable identity"} {
		t.Run(scenario, func(t *testing.T) {
			calls := 0
			file, err := openOpenCodePlaceholderWithGroupCheck(path, func(uid, gid uint32) bool {
				calls++
				passwd := fmt.Sprintf("owner:x:%d:%d::/synthetic:/bin/sh\n", uid, gid)
				group := fmt.Sprintf("owner:x:%d:\n", gid)
				switch scenario {
				case "explicit member":
					group = fmt.Sprintf("owner:x:%d:other\n", gid)
				case "other primary user":
					passwd += fmt.Sprintf("other:x:%d:%d::/synthetic:/bin/sh\n", uid+1, gid)
				case "wrong primary gid":
					passwd = fmt.Sprintf("owner:x:%d:%d::/synthetic:/bin/sh\n", uid, gid+1)
				case "unavailable identity":
					return false
				}
				return openCodePrivateGroupRecords(uid, gid, passwd, group)
			})
			if file != nil {
				_ = file.Close()
			}
			if (err == nil) != (scenario == "private") || calls == 0 {
				t.Fatalf("policy scenario=%s error=%v identity calls=%d", scenario, err, calls)
			}
		})
	}
}

func TestOpenCodeIdentityOutputFailures(t *testing.T) {
	for _, scenario := range []string{"missing", "failure", "oversize", "timeout"} {
		t.Run(scenario, func(t *testing.T) {
			timeout := 5 * time.Second
			if scenario == "timeout" {
				timeout = 100 * time.Millisecond
			}
			ctx, cancel := context.WithTimeout(context.Background(), timeout)
			defer cancel()
			executable := os.Args[0]
			if scenario == "missing" {
				executable = filepath.Join(t.TempDir(), "missing-getent")
			}
			data, err := openCodeIdentityOutput(ctx, executable, "-test.run=^TestOpenCodeIdentityOutputHelper$", "--", scenario)
			if err == nil || data != "" {
				t.Fatalf("identity %s did not fail closed: bytes=%d err=%v", scenario, len(data), err)
			}
		})
	}
}

func TestOpenCodePlaceholderRejectsSpecialPermissionBits(t *testing.T) {
	parent := openCodeCanonicalTempDir(t)
	path := filepath.Join(parent, "placeholder.json")
	if err := os.WriteFile(path, []byte("synthetic"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct {
		path string
		mode os.FileMode
	}{
		{parent, 0o700 | os.ModeSticky},
		{path, 0o600 | os.ModeSetgid},
	} {
		if err := os.Chmod(tt.path, tt.mode); err != nil {
			t.Fatal(err)
		}
		file, err := openOpenCodePlaceholder(path)
		if file != nil {
			_ = file.Close()
		}
		if err == nil {
			t.Errorf("special permission bits accepted: %v", tt.mode)
		}
		if err := os.Chmod(tt.path, tt.mode.Perm()); err != nil {
			t.Fatal(err)
		}
	}
}

func TestOpenCodeIdentityOutputHelper(t *testing.T) {
	if len(os.Args) < 3 || os.Args[len(os.Args)-2] != "--" {
		return
	}
	switch os.Args[len(os.Args)-1] {
	case "failure":
		os.Exit(1)
	case "oversize":
		_, _ = os.Stdout.WriteString(strings.Repeat("x", 2*1024*1024))
		os.Exit(0)
	case "timeout":
		<-time.After(time.Hour)
		os.Exit(0)
	}
}
