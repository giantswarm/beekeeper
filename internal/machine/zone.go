package machine

import (
	"fmt"
	"os"
	"strings"
	"time"
)

// localtime is the link timedatectl set-timezone points at the zone.
var localtime = "/etc/localtime"

// Zone is the machine's time zone as timedatectl sets it, read now: a
// long-running process's time.Local stays the zone it started in.
func Zone() (*time.Location, error) {
	target, err := os.Readlink(localtime)
	if err != nil {
		return nil, fmt.Errorf("the machine's time zone: %w", err)
	}
	_, name, ok := strings.Cut(target, "zoneinfo/")
	if !ok {
		return nil, fmt.Errorf("the machine's time zone: %s links %s, no zoneinfo file", localtime, target)
	}
	return time.LoadLocation(name)
}
