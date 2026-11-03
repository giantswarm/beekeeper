//go:build (linux && !nosystemd) || darwin

package platform

import (
	"bytes"
	"encoding/json"
	"encoding/xml"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestSupervisorLoopServiceFindsToolsOutsideManagerPATH(t *testing.T) {
	const workerTool = "claude"
	root := t.TempDir()
	tools := filepath.Join(root, `tools & $percent% "quoted"`)
	if err := os.Mkdir(tools, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"gh", workerTool} {
		if err := os.WriteFile(filepath.Join(tools, name), []byte("#!/bin/sh\nprintf 'tool-found\\n'\n"), 0o700); err != nil { //nolint:gosec // the isolated fixture must be executable to test tool discovery
			t.Fatal(err)
		}
	}
	// Neither dependency is visible in the login service's environment.
	t.Setenv("PATH", root)
	if _, err := exec.LookPath("gh"); err == nil {
		t.Fatal("restricted manager PATH unexpectedly contains gh")
	}
	raw, err := json.Marshal(map[string]any{"Home": root, "ConfigDir": root, "Exe": "/opt/bin/beekeeper", "ToolPath": tools + ":.:relative::" + tools})
	if err != nil {
		t.Fatal(err)
	}
	var spec SetupSpec
	if err := json.Unmarshal(raw, &spec); err != nil {
		t.Fatal(err)
	}
	files, _ := Current(Options{}).Setup.Files(spec)
	for _, f := range files {
		if !strings.Contains(filepath.Base(f.Path), "supervisor-loop") {
			continue
		}
		path := renderedLoopPath(t, f)
		dirs := filepath.SplitList(path)
		if len(dirs) == 0 || dirs[0] != tools {
			t.Fatalf("service lost installing shell's tool directory: %q", path)
		}
		for _, dir := range dirs {
			if !filepath.IsAbs(dir) {
				t.Fatalf("service retained relative PATH entry %q", dir)
			}
		}
		t.Setenv("PATH", path)
		for _, name := range []string{"gh", workerTool} {
			found, err := exec.LookPath(name)
			if err != nil || found != filepath.Join(tools, name) {
				t.Fatalf("service cannot discover %s: %q, %v", name, found, err)
			}
			out, err := exec.Command(name, "--version").CombinedOutput() //nolint:gosec // isolated fixture tools discovered through the rendered PATH
			if err != nil || string(out) != "tool-found\n" {
				t.Fatalf("service cannot run %s: %q, %v", name, out, err)
			}
		}
		return
	}
	t.Fatal("supervisor loop unit missing")
}

func renderedLoopPath(t *testing.T, f File) string {
	t.Helper()
	if strings.HasSuffix(f.Path, ".service") {
		for line := range strings.Lines(string(f.Content)) {
			if raw, ok := strings.CutPrefix(strings.TrimSpace(line), "Environment="); ok {
				value, err := strconv.Unquote(raw)
				if err != nil {
					t.Fatal(err)
				}
				if path, ok := strings.CutPrefix(value, "PATH="); ok {
					return strings.ReplaceAll(path, "%%", "%")
				}
			}
		}
	} else {
		decoder := xml.NewDecoder(bytes.NewReader(f.Content))
		wantPath := false
		for {
			token, err := decoder.Token()
			if err == io.EOF {
				break
			}
			if err != nil {
				t.Fatal(err)
			}
			start, ok := token.(xml.StartElement)
			if !ok {
				continue
			}
			if start.Name.Local == "key" {
				var key string
				if err := decoder.DecodeElement(&key, &start); err != nil {
					t.Fatal(err)
				}
				wantPath = key == "PATH"
			}
			if start.Name.Local == "string" && wantPath {
				var path string
				if err := decoder.DecodeElement(&path, &start); err != nil {
					t.Fatal(err)
				}
				return path
			}
		}
	}
	t.Fatal("loop service has no explicit tool PATH")
	return ""
}
