package omp

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

// A provider's key reaches an omp agent beekeeper starts through its
// environment and nothing else. omp resolves a provider's apiKey as the
// name of an environment variable first and as the key itself otherwise,
// so the models file names the variable, beekeeper reads the vault and
// writes NAME=value into the agent's inbox before omp runs, and the unit's
// shell exports it and execs omp: no file, no command line and no unit
// property carries the value. omp hands its whole environment to the
// shells that run the agent's tool commands; the shell the unit names in
// SHELL (ToolShell) drops the variables listed in EnvCredentials first.

// EnvCredentials lists, in an omp agent's environment, the variables that
// carry a provider's key, separated by spaces: what ToolShell drops.
const EnvCredentials = "BEEKEEPER_OMP_CREDENTIALS" //nolint:gosec // the variables' names, no value

// VarName is the form of a variable's name as a provider's apiKey names
// one: upper case, as omp's own variables are named. A key is no name.
var VarName = regexp.MustCompile(`^[A-Z][A-Z0-9_]*$`)

// ErrKeyValue is a provider whose apiKey in the models file is a value,
// which the file then carries.
var ErrKeyValue = errors.New("the models file carries the key itself")

// ProviderKey reads how provider gets its key in the models file at
// path: the name of the variable it reads it from (VarName), "" for no key
// (no apiKey, a keyless provider, or none the file has; a missing file has
// none), or ErrKeyValue when the file holds a value instead of a name.
func ProviderKey(path, provider string) (string, error) {
	raw, err := os.ReadFile(filepath.Clean(path))
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	var file struct {
		Providers map[string]struct {
			APIKey string `yaml:"apiKey"`
		} `yaml:"providers"`
	}
	if err := yaml.Unmarshal(raw, &file); err != nil {
		return "", fmt.Errorf("%s: %w", path, err)
	}
	key := file.Providers[provider].APIKey
	switch {
	case key == "":
		return "", nil
	case VarName.MatchString(key):
		return key, nil
	}
	return "", fmt.Errorf("%s: providers.%s.apiKey: %w", path, provider, ErrKeyValue)
}

// ToolShell is the shell omp runs the agent's tool commands in (SHELL):
// bash without the provider keys of EnvCredentials, which omp passes on
// with the rest of its environment.
const ToolShell = `#!/bin/sh
# beekeeper's bash for an omp agent's tool commands: the provider keys omp
# runs on stay omp's.
for n in $` + EnvCredentials + `; do unset "$n"; done
exec /bin/bash "$@"
`

// ToolShellPath is where beekeeper keeps ToolShell under its state folder,
// named bash so that omp takes it for the bash it is.
func ToolShellPath(stateDir string) string {
	return filepath.Join(stateDir, "omp", "bin", "bash")
}

// WriteToolShell puts ToolShell at ToolShellPath, executable, and answers
// the path; one there with the same content stays.
func WriteToolShell(stateDir string) (string, error) {
	path := ToolShellPath(stateDir)
	if cur, err := os.ReadFile(path); err == nil && string(cur) == ToolShell { //nolint:gosec // beekeeper's own file under its state folder
		return path, nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".bash.*")
	if err != nil {
		return "", err
	}
	defer os.Remove(tmp.Name()) //nolint:errcheck // gone after the rename
	if _, err := tmp.WriteString(ToolShell); err != nil {
		_ = tmp.Close()
		return "", err
	}
	if err := tmp.Chmod(0o700); err != nil {
		_ = tmp.Close()
		return "", err
	}
	if err := tmp.Close(); err != nil {
		return "", err
	}
	return path, os.Rename(tmp.Name(), path)
}

// ShellArgv runs argv, omp's command line, with its stdin on the inbox at
// path: opened for reading and writing, so the inbox never reaches its end
// while the agent runs, whoever writes to it. Before omp runs, the shell
// reads one NAME=value line from the inbox for each variable named in
// credentials and exports it: the provider keys beekeeper hands over, which
// then live in omp's environment alone. A line that is not the named
// variable's ends the unit (exit 78), as a key that does not arrive would.
// omp's protocol output goes nowhere; its log is omp's own (~/.omp/logs)
// and the unit's journal keeps its errors.
func ShellArgv(path string, credentials []string, argv ...string) []string {
	const script = `inbox=$1; names=$2; shift 2
exec 3<>"$inbox"
for n in $names; do
  IFS= read -r line <&3 || exit 78
  case $line in "$n="*) ;; *) exit 78 ;; esac
  export "$line"
done
exec "$@" <&3 3<&- >/dev/null`
	return append([]string{"/bin/sh", "-c", script, "omp-inbox", path, strings.Join(credentials, " ")}, argv...)
}
