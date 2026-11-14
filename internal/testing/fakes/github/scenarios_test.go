package fakegithub

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/giantswarm/beekeeper/internal/clock"
)

// TestScenariosSeedAndDrive loads every approved scenario's pre_configuration.github (merged onto
// the base fixture) and runs its github_* steps against the fake, without an engine: every seed
// decodes strictly and every step's arguments are accepted.
func TestScenariosSeedAndDrive(t *testing.T) {
	t.Parallel()
	root := filepath.Join("..", "..", "scenarios")
	base := readYAML(t, filepath.Join(root, "fixtures", "base.yaml"))
	var files []string
	_ = filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() && strings.HasSuffix(p, ".yaml") && !strings.Contains(p, "fixtures") {
			files = append(files, p)
		}
		return nil
	})
	if len(files) == 0 {
		t.Skip("no scenarios")
	}
	for _, file := range files {
		t.Run(strings.TrimSuffix(filepath.Base(file), ".yaml"), func(t *testing.T) {
			t.Parallel()
			doc := readYAML(t, file)
			pre, _ := doc["pre_configuration"].(map[string]any)
			gh := merge(base["github"], pre["github"]).(map[string]any)
			at := start
			if c, ok := pre["clock"].(string); ok {
				var err error
				if at, err = time.Parse(time.RFC3339, c); err != nil {
					t.Fatal(err)
				}
			}
			c := clock.NewFake(at)
			s, err := New(Options{Clock: c})
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			if err := s.Seed(gh); err != nil {
				t.Fatalf("seed: %v", err)
			}
			steps, _ := doc["steps"].([]any)
			for _, st := range steps {
				step := st.(map[string]any)
				id, _ := step["id"].(string)
				tool, _ := step["tool"].(string)
				args, _ := step["args"].(map[string]any)
				s.MarkStep(id)
				switch {
				case tool == "clock_advance":
					d, err := time.ParseDuration(args["by"].(string))
					if err != nil {
						t.Fatal(err)
					}
					c.Advance(d)
				case strings.HasPrefix(tool, "github_"):
					wantErr := false
					if exp, ok := step["expected"].(map[string]any); ok {
						if ok, isBool := exp["success"].(bool); isBool && !ok {
							wantErr = true
						}
					}
					_, err := s.Do(context.Background(), tool, args)
					if (err != nil) != wantErr {
						t.Fatalf("step %s (%s): %v", id, tool, err)
					}
				}
			}
		})
	}
}

func readYAML(t *testing.T, path string) map[string]any {
	t.Helper()
	b, err := os.ReadFile(path) //nolint:gosec // G304: a fixture path from the test itself
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := yaml.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

// merge overlays b on a: maps merge by key, lists of named objects by name or ref, anything else
// is replaced.
func merge(a, b any) any {
	if b == nil {
		return a
	}
	switch av := a.(type) {
	case map[string]any:
		bv, ok := b.(map[string]any)
		if !ok {
			return b
		}
		out := map[string]any{}
		for k, v := range av {
			out[k] = v
		}
		for k, v := range bv {
			out[k] = merge(av[k], v)
		}
		return out
	case []any:
		bv, ok := b.([]any)
		if !ok {
			return b
		}
		key := func(x any) string {
			m, ok := x.(map[string]any)
			if !ok {
				return ""
			}
			for _, k := range []string{"name", "ref"} {
				if s, ok := m[k].(string); ok {
					return s
				}
			}
			return ""
		}
		if len(av) == 0 || key(av[0]) == "" {
			return b
		}
		out := append([]any(nil), av...)
		for _, x := range bv {
			k := key(x)
			replaced := false
			for i, y := range out {
				if k != "" && key(y) == k {
					out[i] = merge(y, x)
					replaced = true
				}
			}
			if !replaced {
				out = append(out, x)
			}
		}
		return out
	}
	return b
}
