//go:build linux && !nosystemd

package platform

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"golang.org/x/sys/unix"
)

// inputDir holds the kernel's input event devices, inputClass their
// capabilities.
const (
	inputDir   = "/dev/input"
	inputClass = "/sys/class/input"
)

// The event types of a device the person types or points with: keys and
// buttons, relative motion. A lid or jack switch, a speaker or a sensor
// has neither.
const (
	evKey = 1 << 0x01
	evRel = 1 << 0x02
)

// inputPoll bounds one wait for an event, so the watch ends soon after its
// context.
const inputPoll = 500 * time.Millisecond

// evdevInput reads the input event devices: only when an event arrives,
// never what it was. It grabs no device, so every other reader still gets
// every event.
type evdevInput struct{}

func (evdevInput) Watch(ctx context.Context) (func() time.Time, error) {
	paths, _ := filepath.Glob(filepath.Join(inputDir, "event*"))
	var fds []unix.PollFd
	var errs []error
	for _, p := range paths {
		if !personDevice(filepath.Base(p)) {
			continue
		}
		fd, err := unix.Open(p, unix.O_RDONLY|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", p, err))
			continue
		}
		fds = append(fds, unix.PollFd{Fd: int32(fd), Events: unix.POLLIN}) //nolint:gosec // a file descriptor fits
	}
	if len(fds) == 0 {
		if len(errs) == 0 {
			errs = append(errs, errors.New("none found"))
		}
		return nil, fmt.Errorf("no keyboard or pointer readable under %s: %w", inputDir, errors.Join(errs...))
	}
	var last atomic.Int64
	last.Store(time.Now().UnixNano())
	go func() {
		defer func() {
			for _, f := range fds {
				if f.Fd >= 0 {
					_ = unix.Close(int(f.Fd))
				}
			}
		}()
		buf := make([]byte, 4096)
		for ctx.Err() == nil {
			n, err := unix.Poll(fds, int(inputPoll.Milliseconds()))
			if err != nil && !errors.Is(err, unix.EINTR) {
				// Blind from here: never quiet.
				last.Store(math.MaxInt64)
				return
			}
			if n <= 0 {
				continue
			}
			for i := range fds {
				ev := fds[i].Revents
				fds[i].Revents = 0
				if ev&(unix.POLLERR|unix.POLLHUP|unix.POLLNVAL) != 0 {
					// Unplugged: poll skips a negative descriptor.
					_ = unix.Close(int(fds[i].Fd))
					fds[i].Fd = -1
					continue
				}
				if ev&unix.POLLIN == 0 {
					continue
				}
				last.Store(time.Now().UnixNano())
				// Drained unread: only the arrival counts.
				for {
					if m, err := unix.Read(int(fds[i].Fd), buf); m <= 0 || err != nil {
						break
					}
				}
			}
		}
	}()
	return func() time.Time { return time.Unix(0, last.Load()) }, nil
}

// personDevice reports whether the input device event (event3) has keys or
// relative motion: a keyboard, a mouse, a touchpad, a button.
func personDevice(event string) bool {
	raw, err := os.ReadFile(filepath.Join(inputClass, event, "device", "capabilities", "ev")) //nolint:gosec // a sysfs file of a /dev/input name
	if err != nil {
		return false
	}
	ev, err := strconv.ParseUint(strings.TrimSpace(string(raw)), 16, 64)
	return err == nil && ev&(evKey|evRel) != 0
}
