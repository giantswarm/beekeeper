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
	// Git and other tools read /dev/urandom for temporary file names.
	for _, dev := range []string{"null", "zero", "random", "urandom"} {
		target := filepath.Join(jail, "dev", dev)
		if err := os.WriteFile(target, nil, 0600); err != nil {
			return err
		}
		if err := syscall.Mount("/dev/"+dev, target, "", syscall.MS_BIND, ""); err != nil {
			return fmt.Errorf("mount /dev/%s: %w", dev, err)
		}
	}
	if err := syscall.Chroot(jail); err != nil {
		return err
	}
	return os.Chdir("/work/repo")
}
