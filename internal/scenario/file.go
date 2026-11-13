//go:build scenario

package scenario

import (
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
)

// FileExpect checks a file under the scenario's work directory: one the
// candidate writes where a step names it (a CPU profile, a report), which no
// output, event or call shows. Path is relative to the work directory, so
// "cpu.pprof" is /work/cpu.pprof. Exists says whether the file is there (true
// by default); MinBytes is its least size; Contains, NotContains and Matches
// (a regular expression, multi-line) check its contents.
type FileExpect struct {
	Path        string   `yaml:"path"`
	Exists      *bool    `yaml:"exists"`
	MinBytes    *int     `yaml:"min_bytes"`
	Contains    []string `yaml:"contains"`
	NotContains []string `yaml:"not_contains"`
	Matches     string   `yaml:"matches"`
}

// validate is what the loader refuses of a file expectation.
func (f *FileExpect) validate() []string {
	var out []string
	switch {
	case f.Path == "":
		out = append(out, "file without a path")
	case !filepath.IsLocal(f.Path) || filepath.Clean(f.Path) == ".":
		out = append(out, fmt.Sprintf("file: path %q: a file under the scenario's work directory, relative to it (cpu.pprof is /work/cpu.pprof)", f.Path))
	}
	if f.MinBytes != nil && *f.MinBytes < 1 {
		out = append(out, fmt.Sprintf("file: min_bytes %d: at least 1; a file that is there passes exists", *f.MinBytes))
	}
	if f.Matches != "" {
		if _, err := regexp.Compile(f.Matches); err != nil {
			out = append(out, fmt.Sprintf("file: matches: %v", err))
		}
	}
	if f.Exists != nil && !*f.Exists && f.checksContents() {
		out = append(out, "file: exists false with checks of the contents of a file that is not there")
	}
	return out
}

// checksContents says whether the expectation reads the file's contents.
func (f *FileExpect) checksContents() bool {
	return f.MinBytes != nil || len(f.Contains)+len(f.NotContains) > 0 || f.Matches != ""
}

// check checks the expectation against what read returns for its path: the
// file's contents, or false when it is not there.
func (f *FileExpect) check(read func(path string) (data []byte, exists bool, err error)) error {
	data, exists, err := read(f.Path)
	if err != nil {
		return fmt.Errorf("file %s: %w", f.Path, err)
	}
	want := f.Exists == nil || *f.Exists
	switch {
	case exists && !want:
		return fmt.Errorf("file %s is there (%d bytes), want none", f.Path, len(data))
	case !exists && want:
		return fmt.Errorf("file %s is not there", f.Path)
	case !exists:
		return nil
	}
	var errs []error
	if f.MinBytes != nil && len(data) < *f.MinBytes {
		errs = append(errs, fmt.Errorf("file %s has %d bytes, at least %d", f.Path, len(data), *f.MinBytes))
	}
	if len(f.Contains)+len(f.NotContains) > 0 || f.Matches != "" {
		errs = append(errs, checkText("file "+f.Path, TextExpect{Contains: f.Contains, NotContains: f.NotContains, Matches: f.Matches}, string(data)))
	}
	return errors.Join(errs...)
}
