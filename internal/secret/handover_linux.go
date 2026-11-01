//go:build linux

package secret

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

const handoverName = "beekeeper-vault"

type vaultHandover struct {
	Env          string
	Since, Until time.Time
}

// PrepareHandover gives systemd an anonymous memory descriptor. Neither the
// installer nor any socket reply receives the session. A later lock or unlock
// invalidates this snapshot, so a restart cannot resurrect a revoked session.
func (k *Keeper) PrepareHandover() error {
	k.mu.Lock()
	defer k.mu.Unlock()
	if !k.unlocked {
		return nil
	}
	k.invalidateHandover()
	fd, err := unix.MemfdCreate(handoverName, unix.MFD_CLOEXEC)
	if err != nil {
		return errors.New("cannot allocate vault handover memory")
	}
	f := os.NewFile(uintptr(fd), handoverName)
	keep := false
	defer func() {
		if !keep {
			_ = f.Truncate(0)
			_ = f.Close()
		}
	}()
	if err := json.NewEncoder(f).Encode(vaultHandover{k.env, k.since, k.until}); err != nil {
		return errors.New("cannot prepare vault handover")
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return errors.New("cannot rewind vault handover")
	}
	// Replacing a previously prepared snapshot must not consume a second slot.
	if err := notifyVault("FDSTOREREMOVE=1\nFDNAME="+handoverName, -1); err != nil {
		return err
	}
	if err := notifyVault("FDSTORE=1\nFDNAME="+handoverName, fd); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "systemctl", "--user", "show", BrokerUnit, "--property=NFileDescriptorStore", "--value").Output()
	if err != nil || strings.TrimSpace(string(out)) != "1" {
		_ = f.Truncate(0)
		return errors.New("service manager did not retain the vault handover; restart refused")
	}
	k.handover = f
	keep = true
	return nil
}

// notifyVault waits for systemd to process the descriptor message using its
// barrier protocol. Only fixed metadata travels in the datagram.
func notifyVault(message string, fd int) error {
	path := os.Getenv("NOTIFY_SOCKET")
	if path == "" {
		return errors.New("no service manager notification socket: vault handover unavailable")
	}
	socket, err := unix.Socket(unix.AF_UNIX, unix.SOCK_DGRAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		return errors.New("cannot reach service manager for vault handover")
	}
	defer func() { _ = unix.Close(socket) }()
	limit := unix.NsecToTimeval((3 * time.Second).Nanoseconds())
	if err := unix.SetsockoptTimeval(socket, unix.SOL_SOCKET, unix.SO_SNDTIMEO, &limit); err != nil {
		return err
	}
	address := &unix.SockaddrUnix{Name: path}
	var rights []byte
	if fd >= 0 {
		rights = unix.UnixRights(fd)
	}
	if err := unix.Sendmsg(socket, []byte(message), rights, address, 0); err != nil {
		return fmt.Errorf("cannot send vault handover to service manager: %w", err)
	}
	var pipe [2]int
	if err := unix.Pipe2(pipe[:], unix.O_CLOEXEC|unix.O_NONBLOCK); err != nil {
		return err
	}
	defer func() { _ = unix.Close(pipe[0]) }()
	err = unix.Sendmsg(socket, []byte("BARRIER=1"), unix.UnixRights(pipe[1]), address, 0)
	_ = unix.Close(pipe[1])
	if err != nil {
		return errors.New("cannot confirm vault handover")
	}
	readFD := pipe[0]
	if readFD < 0 || readFD > 2147483647 {
		return errors.New("invalid handover barrier descriptor")
	}
	p := []unix.PollFd{{Fd: int32(readFD), Events: unix.POLLIN | unix.POLLHUP}}
	if n, err := unix.Poll(p, 3000); err != nil || n == 0 || p[0].Revents&unix.POLLHUP == 0 {
		return errors.New("service manager did not confirm vault handover")
	}
	return nil
}

// RestoreHandover consumes only this service's inherited descriptor, before
// any sign-in or child process. The original expiry is never extended.
func (k *Keeper) RestoreHandover() (result error) {
	pid, count, names := os.Getenv("LISTEN_PID"), os.Getenv("LISTEN_FDS"), os.Getenv("LISTEN_FDNAMES")
	_ = os.Unsetenv("LISTEN_PID")
	_ = os.Unsetenv("LISTEN_FDS")
	_ = os.Unsetenv("LISTEN_FDNAMES")
	if pid != strconv.Itoa(os.Getpid()) || count == "" {
		return nil
	}
	n, err := strconv.Atoi(count)
	if err != nil || n < 0 || n > 64 {
		return errors.New("invalid inherited vault descriptor count")
	}
	labels := strings.Split(names, ":")
	var restored bool
	files := make([]*os.File, n)
	for i := range n {
		files[i] = os.NewFile(uintptr(3+i), handoverName)
		unix.CloseOnExec(3 + i)
	}
	defer func() {
		for _, f := range files {
			_ = f.Close()
		}
		if restored {
			if err := notifyVault("FDSTOREREMOVE=1\nFDNAME="+handoverName, -1); err != nil {
				result = err
			}
		}
		if result != nil {
			k.Lock()
		}
	}()
	for i, f := range files {
		if i >= len(labels) || labels[i] != handoverName {
			continue
		}
		if restored {
			return errors.New("duplicate vault handover")
		}
		restored = true
		err := k.restoreHandover(f, time.Now())
		if err != nil {
			return err
		}
	}
	return nil
}

func (k *Keeper) restoreHandover(r io.Reader, now time.Time) error {
	var h vaultHandover
	if err := json.NewDecoder(io.LimitReader(r, 16384)).Decode(&h); err != nil {
		return errors.New("invalid vault handover; vault stays locked")
	}
	name, token, ok := strings.Cut(h.Env, "=")
	if !ok || !sessionName.MatchString(name) || token == "" || strings.ContainsAny(token, "\x00\n") || h.Since.IsZero() || h.Since.After(now) || !h.Until.After(now) || !h.Until.After(h.Since) {
		return errors.New("invalid or expired vault handover; vault stays locked")
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.unlocked {
		return fmt.Errorf("vault handover found an unlocked keeper")
	}
	close(k.ready)
	k.unlocked, k.env, k.since, k.until = true, h.Env, h.Since, h.Until
	k.end = time.AfterFunc(time.Until(h.Until), func() { k.lockSession(h.Since) })
	return nil
}
