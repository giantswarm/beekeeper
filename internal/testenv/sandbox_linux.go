//go:build linux

package testenv

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// EnterSandbox prepares a private mount namespace, then enters a filesystem
// containing only system executables and this run's workspace. The caller must
// already be in fresh user, mount, PID and network namespaces (unshare -Urmpnf).
// No host home, runtime sockets, credentials, or other test runs are mounted.
// Namespace teardown, including on interruption, never unmounts the host's mounts.
func EnterSandbox(root string) error {
	if os.Getpid() != 1 || os.Geteuid() != 0 {
		return fmt.Errorf("sandbox must be namespace init under unshare -Urmpnf")
	}
	if err := syscall.Mount("", "/", "", syscall.MS_REC|syscall.MS_PRIVATE, ""); err != nil {
		return err
	}
	jail := filepath.Join(root, "jail")
	bind := func(source, target string, readonly bool) error {
		target = filepath.Join(jail, target)
		if err := os.MkdirAll(target, 0700); err != nil {
			return err
		}
		if err := syscall.Mount(source, target, "", syscall.MS_BIND|syscall.MS_REC, ""); err != nil {
			return err
		}
		if readonly {
			return syscall.Mount("", target, "", syscall.MS_BIND|syscall.MS_REMOUNT|syscall.MS_RDONLY, "")
		}
		return nil
	}
	for _, dir := range []string{"usr", "bin", "lib", "lib64"} {
		source := "/" + dir
		if _, err := os.Stat(source); os.IsNotExist(err) {
			continue
		}
		if err := bind(source, dir, true); err != nil {
			return fmt.Errorf("mount %s: %w", dir, err)
		}
	}
	if err := bind(filepath.Join(root, "work"), "work", false); err != nil {
		return err
	}
	for _, dir := range []string{"proc", "dev", "tmp"} {
		if err := os.MkdirAll(filepath.Join(jail, dir), 0700); err != nil {
			return err
		}
	}
	if err := syscall.Mount("proc", filepath.Join(jail, "proc"), "proc", syscall.MS_NOSUID|syscall.MS_NODEV|syscall.MS_NOEXEC, ""); err != nil {
		return err
	}
	null := filepath.Join(jail, "dev/null")
	if err := os.WriteFile(null, nil, 0600); err != nil {
		return err
	}
	if err := syscall.Mount("/dev/null", null, "", syscall.MS_BIND, ""); err != nil {
		return err
	}
	if err := syscall.Chroot(jail); err != nil {
		return err
	}
	return os.Chdir("/work/repo")
}

// WorkflowEnv is an allowlist, deliberately independent of os.Environ.
// GitHub Git transport uses the fixture remote helper; Git has no system or
// user config. The private network namespace also blocks direct socket calls.
func WorkflowEnv() []string {
	return []string{
		"PATH=/work/bin:/usr/bin:/bin",
		"HOME=/work/home",
		"TMPDIR=/tmp",
		"LANG=C",
		"XDG_CONFIG_HOME=/work/home/config",
		"XDG_STATE_HOME=/work/home/state",
		"XDG_CACHE_HOME=/work/home/cache",
		"BEEKEEPER_CONFIG=/work/config.yaml",
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_ALLOW_PROTOCOL=file:https",
		"GIT_EXEC_PATH=/work/bin",
		"GIT_TERMINAL_PROMPT=0",
		"GIT_AUTHOR_NAME=Test",
		"GIT_AUTHOR_EMAIL=test@example.invalid",
		"GIT_COMMITTER_NAME=Test",
		"GIT_COMMITTER_EMAIL=test@example.invalid",
		"GOPROXY=off",
		"GOSUMDB=off",
	}
}
