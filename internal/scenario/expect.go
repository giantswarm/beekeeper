//go:build scenario

package scenario

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/giantswarm/beekeeper/internal/scenario/fakegithub"
	"github.com/giantswarm/beekeeper/internal/scenario/harness"
)

// View is one step's response as every expectation reads it: one view for
// every step kind, so no expectation is honoured for one kind and silently
// ignored for another. A field a step kind does not observe is nil, and an
// expectation of it fails instead of passing.
type View struct {
	Kind     string
	Exit     *int
	Stdout   *string
	Stderr   *string
	Duration *time.Duration
	// Events reads the verbs of the desk's event log.
	Events func() ([]string, error)
	// Calls counts the world's calls of a program matching patterns.
	Calls func(program string, args []string) (int, error)
	// Process reads a background step's state.
	Process func(name string) (running bool, exit int, ok bool)
	// GitHub checks an expectation of the scenario's GitHub.
	GitHub func(e *fakegithub.Expect) error
	// Worker checks what a scripted worker did.
	Worker func(harness.WorkerExpect) error
	// Origin reads a branch of a repository's origin.
	Origin func(repository, branch string) (exists bool, ahead int, err error)
	// Log reads what a background step has written so far.
	Log func(name string) (string, error)
	// File reads a file under the scenario's work directory: its contents,
	// or false when it is not there.
	File func(path string) (data []byte, exists bool, err error)
}

// Check checks one expectation against a view.
func Check(e Expect, v View) error {
	missing := func(what string) error { return fmt.Errorf("%s is not observed for a %s step", what, v.Kind) }
	switch e.Kind() {
	case "exit":
		if v.Exit == nil {
			return missing("exit")
		}
		if *v.Exit != *e.Exit {
			return fmt.Errorf("exit %d, want %d", *v.Exit, *e.Exit)
		}
	case "output":
		if v.Stdout == nil {
			return missing("output")
		}
		return checkText("output", *e.Output, *v.Stdout)
	case "stderr":
		if v.Stderr == nil {
			return missing("stderr")
		}
		return checkText("stderr", *e.Stderr, *v.Stderr)
	case "json_path":
		if v.Stdout == nil {
			return missing("json_path")
		}
		return checkJSONPath(*e.JSONPath, *v.Stdout)
	case "duration":
		if v.Duration == nil {
			return missing("duration")
		}
		if *v.Duration > e.Duration.Max.Duration {
			return fmt.Errorf("took %s, at most %s", v.Duration.Round(time.Millisecond), e.Duration.Max)
		}
	case "events":
		if v.Events == nil {
			return missing("events")
		}
		verbs, err := v.Events()
		if err != nil {
			return fmt.Errorf("events: %w", err)
		}
		var errs []error
		for _, want := range e.Events.Contains {
			if !slices.Contains(verbs, want) {
				errs = append(errs, fmt.Errorf("events: no %s (log has %s)", want, summarize(verbs)))
			}
		}
		for _, not := range e.Events.NotContains {
			if slices.Contains(verbs, not) {
				errs = append(errs, fmt.Errorf("events: %s recorded", not))
			}
		}
		return errors.Join(errs...)
	case "calls":
		if v.Calls == nil {
			return missing("calls")
		}
		c := e.Calls
		n, err := v.Calls(c.Program, c.Args)
		if err != nil {
			return err
		}
		what := strings.TrimSpace(cmpOr(c.Program, "any program") + " " + strings.Join(c.Args, " "))
		if c.Min != nil && n < *c.Min {
			return fmt.Errorf("calls of %s: %d, at least %d", what, n, *c.Min)
		}
		if c.Max != nil && n > *c.Max {
			return fmt.Errorf("calls of %s: %d, at most %d", what, n, *c.Max)
		}
	case "github":
		if v.GitHub == nil {
			return missing("github")
		}
		return v.GitHub(e.GitHub)
	case "process":
		if v.Process == nil {
			return missing("process")
		}
		p := e.Process
		running, exit, ok := v.Process(p.Name)
		if !ok {
			return fmt.Errorf("process %s was never started", p.Name)
		}
		if p.Running != nil && running != *p.Running {
			return fmt.Errorf("process %s running %t, want %t", p.Name, running, *p.Running)
		}
		if p.Exit != nil {
			if running {
				return fmt.Errorf("process %s still runs, want exit %d", p.Name, *p.Exit)
			}
			if exit != *p.Exit {
				return fmt.Errorf("process %s exited %d, want %d", p.Name, exit, *p.Exit)
			}
		}
	case "worker":
		if v.Worker == nil {
			return missing("worker")
		}
		return v.Worker(*e.Worker)
	case "origin":
		if v.Origin == nil {
			return missing("origin")
		}
		o := e.Origin
		exists, ahead, err := v.Origin(o.Repository, o.Branch)
		if err != nil {
			return fmt.Errorf("origin %s %s: %w", o.Repository, o.Branch, err)
		}
		if want := o.Exists == nil || *o.Exists; exists != want {
			return fmt.Errorf("origin %s: branch %s there %t, want %t", o.Repository, o.Branch, exists, want)
		}
		if o.Ahead != nil && ahead != *o.Ahead {
			return fmt.Errorf("origin %s: branch %s is %d commit(s) ahead of main, want %d", o.Repository, o.Branch, ahead, *o.Ahead)
		}
	case "log":
		if v.Log == nil {
			return missing("log")
		}
		return e.Log.check(v.Log)
	case "file":
		if v.File == nil {
			return missing("file")
		}
		return e.File.check(v.File)
	default:
		return fmt.Errorf("expectation without exactly one kind: %v", e.kinds())
	}
	return nil
}

func cmpOr(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

func summarize(xs []string) string {
	if len(xs) == 0 {
		return "none"
	}
	if len(xs) > 20 {
		return strings.Join(xs[len(xs)-20:], ", ") + fmt.Sprintf(" (last 20 of %d)", len(xs))
	}
	return strings.Join(xs, ", ")
}

func checkText(what string, t TextExpect, got string) error {
	var errs []error
	for _, want := range t.Contains {
		if !strings.Contains(got, want) {
			errs = append(errs, fmt.Errorf("%s lacks %q", what, want))
		}
	}
	for _, not := range t.NotContains {
		if strings.Contains(got, not) {
			errs = append(errs, fmt.Errorf("%s has %q", what, not))
		}
	}
	if t.Matches != "" {
		re, err := regexp.Compile(`(?m)` + t.Matches)
		if err != nil {
			return err
		}
		if !re.MatchString(got) {
			errs = append(errs, fmt.Errorf("%s does not match %q", what, t.Matches))
		}
	}
	if len(errs) > 0 {
		shown := clip(got, 2000)
		if !utf8.ValidString(got) {
			// A binary file, such as a profile, is quoted, not printed.
			shown = strconv.Quote(clip(got, 500))
		}
		errs = append(errs, fmt.Errorf("%s was: %s", what, shown))
	}
	return errors.Join(errs...)
}

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + fmt.Sprintf("… (%d bytes)", len(s))
}

// pathPart is one step of a JSON path: a key or a list index.
type pathPart struct {
	key   string
	index int
	isIdx bool
}

var pathPartRE = regexp.MustCompile(`^([^.\[\]]*)((?:\[[0-9]+\])*)$`)

// parsePath parses "a.b[0].c"; "." is the whole document.
func parsePath(p string) ([]pathPart, error) {
	if p == "" {
		return nil, errors.New("empty path")
	}
	if p == "." {
		return nil, nil
	}
	var out []pathPart
	for _, seg := range strings.Split(p, ".") {
		m := pathPartRE.FindStringSubmatch(seg)
		if m == nil || (m[1] == "" && m[2] == "") {
			return nil, fmt.Errorf("path %q: bad segment %q", p, seg)
		}
		if m[1] != "" {
			out = append(out, pathPart{key: m[1]})
		}
		for _, idx := range regexp.MustCompile(`[0-9]+`).FindAllString(m[2], -1) {
			n, err := strconv.Atoi(idx)
			if err != nil {
				return nil, err
			}
			out = append(out, pathPart{index: n, isIdx: true})
		}
	}
	return out, nil
}

// lookup resolves path in doc.
func lookup(doc any, path []pathPart) (any, bool) {
	cur := doc
	for _, p := range path {
		if p.isIdx {
			l, ok := cur.([]any)
			if !ok || p.index >= len(l) {
				return nil, false
			}
			cur = l[p.index]
			continue
		}
		m, ok := cur.(map[string]any)
		if !ok {
			return nil, false
		}
		if cur, ok = m[p.key]; !ok {
			return nil, false
		}
	}
	return cur, true
}

// normalize turns a YAML value into what encoding/json decodes the same
// JSON into, so 2 equals 2.0 and maps compare by content.
func normalize(v any) (any, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	var out any
	err = json.Unmarshal(b, &out)
	return out, err
}

func checkJSONPath(j JSONPathExpect, stdout string) error {
	var doc any
	if err := json.Unmarshal([]byte(stdout), &doc); err != nil {
		return fmt.Errorf("json_path %s: output is not JSON: %v: %s", j.Path, err, clip(stdout, 500))
	}
	path, err := parsePath(j.Path)
	if err != nil {
		return err
	}
	got, ok := lookup(doc, path)
	switch {
	case j.Exists != nil:
		if ok != *j.Exists {
			return fmt.Errorf("json_path %s: exists %t, want %t", j.Path, ok, *j.Exists)
		}
	case !ok:
		return fmt.Errorf("json_path %s: not found in %s", j.Path, clip(stdout, 1000))
	case j.Len != nil:
		n := -1
		switch x := got.(type) {
		case []any:
			n = len(x)
		case map[string]any:
			n = len(x)
		case string:
			n = len(x)
		}
		if n != *j.Len {
			return fmt.Errorf("json_path %s: length %d, want %d", j.Path, n, *j.Len)
		}
	default:
		want, err := normalize(j.Equals)
		if err != nil {
			return err
		}
		if !reflect.DeepEqual(got, want) {
			g, _ := json.Marshal(got)
			w, _ := json.Marshal(want)
			return fmt.Errorf("json_path %s: %s, want %s", j.Path, g, w)
		}
	}
	return nil
}
