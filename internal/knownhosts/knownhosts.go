// Package knownhosts gives a run's commands a known_hosts file Go's SSH
// clients can read. golang.org/x/crypto/ssh/knownhosts, which go-git's SSH
// transport uses, refuses a whole file for one line whose key type it does
// not know (a post-quantum host key OpenSSH accepts, for one), so every
// go-git SSH test of a machine whose known_hosts holds such a line fails.
// Parseable writes the machine's known_hosts without those lines to a file
// of its own, for the command's SSH_KNOWN_HOSTS; the machine's files stay
// untouched.
package knownhosts

import (
	"bytes"
	"errors"
	"io/fs"
	"os"
	"path/filepath"

	"golang.org/x/crypto/ssh/knownhosts"
)

// Env is the variable go-git reads its known_hosts files from, a list
// separated like PATH; set, it replaces the default files.
const Env = "SSH_KNOWN_HOSTS"

// Defaults are the files go-git reads when Env is unset, home first:
// ~/.ssh/known_hosts and /etc/ssh/ssh_known_hosts.
func Defaults(home string) []string {
	return []string{filepath.Join(home, ".ssh", "known_hosts"), "/etc/ssh/ssh_known_hosts"}
}

// Parseable writes the lines of srcs that Go's knownhosts parses to dst and
// reports how many lines it dropped. With nothing dropped it leaves dst alone
// and returns 0: the sources serve as they are. A missing source counts as
// empty. dst is replaced atomically, so concurrent runs read a whole file.
func Parseable(dst string, srcs ...string) (dropped int, err error) {
	var out bytes.Buffer
	var check *os.File
	defer func() {
		if check != nil {
			_ = check.Close()
			_ = os.Remove(check.Name())
		}
	}()
	for _, src := range srcs {
		b, err := os.ReadFile(src) //nolint:gosec // the machine's own known_hosts files
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return 0, err
		}
		for line := range bytes.Lines(b) {
			// New only accepts filenames. Reuse a private scratch file to
			// apply the consumer's exact rules, including its scanner limit.
			if check == nil {
				check, err = os.CreateTemp("", ".known_hosts-check-*")
				if err != nil {
					return 0, err
				}
			}
			line = bytes.TrimSuffix(line, []byte{'\n'})
			ok, err := parses(check, line)
			if err != nil {
				return 0, err
			}
			if !ok {
				dropped++
				continue
			}
			out.Write(line)
			out.WriteByte('\n')
		}
	}
	if dropped == 0 {
		return 0, nil
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o750); err != nil {
		return 0, err
	}
	tmp, err := os.CreateTemp(filepath.Dir(dst), ".known_hosts-*")
	if err != nil {
		return 0, err
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if _, err := tmp.Write(out.Bytes()); err != nil {
		_ = tmp.Close()
		return 0, err
	}
	if err := tmp.Close(); err != nil {
		return 0, err
	}
	if _, err := knownhosts.New(tmp.Name()); err != nil {
		return 0, err
	}
	if err := os.Chmod(tmp.Name(), 0o644); err != nil { //nolint:gosec // public host keys, as ~/.ssh/known_hosts
		return 0, err
	}
	return dropped, os.Rename(tmp.Name(), dst)
}

// parses checks one output line with the same parser as the final consumer.
// File errors abort filtering rather than discarding potentially trusted keys.
func parses(check *os.File, line []byte) (bool, error) {
	if err := check.Truncate(0); err != nil {
		return false, err
	}
	if _, err := check.WriteAt(line, 0); err != nil {
		return false, err
	}
	if _, err := check.WriteAt([]byte{'\n'}, int64(len(line))); err != nil {
		return false, err
	}
	_, err := knownhosts.New(check.Name())
	var pathErr *fs.PathError
	if errors.As(err, &pathErr) {
		return false, err
	}
	return err == nil, nil
}
