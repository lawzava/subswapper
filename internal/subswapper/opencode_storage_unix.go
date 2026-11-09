//go:build !windows

package subswapper

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"
)

func openOpenCodePlaceholder(path string) (*os.File, error) {
	return openOpenCodePlaceholderWithGroupCheck(path, openCodePrivateGroup)
}

func openOpenCodePlaceholderWithGroupCheck(path string, groupCheck func(uint32, uint32) bool) (*os.File, error) {
	unsafe := errors.New("unsafe placeholder")
	privateGroups := make(map[[2]uint32]bool)
	for dir := filepath.Dir(path); ; dir = filepath.Dir(dir) {
		info, err := os.Lstat(dir)
		if err != nil {
			return nil, unsafe
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok || !info.IsDir() || (stat.Uid != uint32(os.Getuid()) && stat.Uid != 0) ||
			(dir == filepath.Dir(path) && openCodePermissionBits(info.Mode()) != 0o700) {
			return nil, unsafe
		}
		if stat.Uid != 0 || info.Mode()&os.ModeSticky == 0 {
			if info.Mode().Perm()&0o002 != 0 {
				return nil, unsafe
			}
			if info.Mode().Perm()&0o020 != 0 {
				key := [2]uint32{stat.Uid, stat.Gid}
				private, known := privateGroups[key]
				if !known {
					private = groupCheck(stat.Uid, stat.Gid)
					privateGroups[key] = private
				}
				if !private {
					return nil, unsafe
				}
			}
		}
		if filepath.Dir(dir) == dir {
			break
		}
	}
	before, err := os.Lstat(path)
	if err != nil {
		return nil, unsafe
	}
	stat, ok := before.Sys().(*syscall.Stat_t)
	if !ok || !before.Mode().IsRegular() || openCodePermissionBits(before.Mode()) != 0o600 || stat.Uid != uint32(os.Getuid()) {
		return nil, unsafe
	}
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, unsafe
	}
	after, err := file.Stat()
	if err != nil || !os.SameFile(before, after) || before.Mode() != after.Mode() {
		_ = file.Close()
		return nil, unsafe
	}
	afterStat, ok := after.Sys().(*syscall.Stat_t)
	if !ok || afterStat.Uid != stat.Uid || afterStat.Gid != stat.Gid {
		_ = file.Close()
		return nil, unsafe
	}
	return file, nil
}

func openCodePermissionBits(mode os.FileMode) os.FileMode {
	return mode & (os.ModePerm | os.ModeSetuid | os.ModeSetgid | os.ModeSticky)
}

func openCodePrivateGroupNSS(config string) bool {
	seen := make(map[string]bool)
	for line := range strings.SplitSeq(config, "\n") {
		fields := strings.Fields(strings.SplitN(line, "#", 2)[0])
		if len(fields) == 0 || (fields[0] != "passwd:" && fields[0] != "group:" && fields[0] != "initgroups:") {
			continue
		}
		if seen[fields[0]] || len(fields) < 2 {
			return false
		}
		files := false
		for _, source := range fields[1:] {
			if source != "files" && source != "systemd" {
				return false
			}
			files = files || source == "files"
		}
		if !files {
			return false
		}
		seen[fields[0]] = true
	}
	return seen["passwd:"] && seen["group:"]
}

func openCodeIdentityID(value string) (uint32, bool) {
	id, err := strconv.ParseUint(value, 10, 32)
	return uint32(id), err == nil && strconv.FormatUint(id, 10) == value
}

func openCodePrivateGroupRecords(uid, gid uint32, passwd, group string) bool {
	matchingGroups := 0
	for line := range strings.SplitSeq(strings.TrimRight(group, "\n"), "\n") {
		fields := strings.Split(line, ":")
		if len(fields) != 4 || fields[0] == "" {
			return false
		}
		groupID, ok := openCodeIdentityID(fields[2])
		if !ok {
			return false
		}
		if groupID == gid {
			if fields[3] != "" {
				return false
			}
			matchingGroups++
		}
	}
	if matchingGroups != 1 {
		return false
	}
	owners := 0
	for line := range strings.SplitSeq(strings.TrimRight(passwd, "\n"), "\n") {
		fields := strings.Split(line, ":")
		if len(fields) != 7 || fields[0] == "" {
			return false
		}
		accountUID, validUID := openCodeIdentityID(fields[2])
		accountGID, validGID := openCodeIdentityID(fields[3])
		if !validUID || !validGID {
			return false
		}
		if accountUID == uid {
			if accountGID != gid {
				return false
			}
			owners++
		} else if accountGID == gid {
			return false
		}
	}
	return owners == 1
}

// NSS enumeration must include every possible primary-group user. Unsupported
// remote/compat sources fail closed rather than treating an empty group as proof.
func openCodePrivateGroup(uid, gid uint32) bool {
	if runtime.GOOS != "linux" {
		return false
	}
	config, err := os.Open("/etc/nsswitch.conf")
	if err != nil {
		return false
	}
	data, err := io.ReadAll(io.LimitReader(config, 1024*1024+1))
	_ = config.Close()
	if err != nil || len(data) > 1024*1024 || !openCodePrivateGroupNSS(string(data)) {
		return false
	}
	getent := func(args ...string) (string, error) {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		return openCodeIdentityOutput(ctx, "/usr/bin/getent", args...)
	}
	passwd, err := getent("passwd")
	if err != nil {
		return false
	}
	group, err := getent("group")
	return err == nil && openCodePrivateGroupRecords(uid, gid, passwd, group)
}

func openCodeIdentityOutput(ctx context.Context, executable string, args ...string) (string, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	cmd := exec.CommandContext(ctx, executable, args...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return "", err
	}
	if err := cmd.Start(); err != nil {
		return "", err
	}
	data, readErr := io.ReadAll(io.LimitReader(stdout, 1024*1024+1))
	if readErr != nil || len(data) > 1024*1024 {
		cancel()
		_ = cmd.Wait()
		return "", errors.New("identity output unavailable")
	}
	if err := cmd.Wait(); err != nil {
		return "", err
	}
	return string(data), nil
}
