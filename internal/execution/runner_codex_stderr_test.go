package execution

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

const codexStartupDiagnostic = "config.toml: permission denied\n"

func TestCodexRunnerLogsExecFailure(t *testing.T) {
	if runtime.GOOS != messageTestLinux {
		t.Skip("exec failure diagnostic is asserted for Linux")
	}
	f, _ := codexFixture(t)
	h := f.s.Config.Harnesses[HarnessCodex]
	h.Executable = filepath.Join(t.TempDir(), "missing-codex")
	j := Journal{Request: f.r}
	j.Handle.Workspace = t.TempDir()
	var log bytes.Buffer
	_, accepted, err := f.s.codexTurn(context.Background(), f.r.Ref, h, nil, &log, j, f.r.Prompt, "", func() error {
		t.Fatal("failed exec accepted the task")
		return nil
	})
	if err == nil || accepted {
		t.Fatal("failed exec was accepted")
	}
	var event struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(log.Bytes(), &event); err != nil {
		t.Fatal(err)
	}
	if event.Type != codexStderrEvent || !strings.Contains(event.Text, "no such file or directory") {
		t.Fatalf("OS startup diagnostic missing: %+v", event)
	}
}

func TestCodexRunnerLogsStartupStderr(t *testing.T) {
	if runtime.GOOS != messageTestLinux {
		t.Skip("execution runner uses a Linux user service")
	}
	f, dir := codexFixture(t)
	ctx := startCodex(t, f)
	if err := os.WriteFile(filepath.Join(dir, "startup-error"), []byte(codexStartupDiagnostic), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := f.s.Run(ctx, f.r.Ref); err == nil {
		t.Fatal("startup failure was accepted")
	}
	page, err := f.s.Logs(ctx, f.r.Ref, LogRequest{})
	if err != nil {
		t.Fatal(err)
	}
	var event struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(page.Data, &event); err != nil {
		t.Fatal(err)
	}
	if event.Type != codexStderrEvent || event.Text != codexStartupDiagnostic {
		t.Fatalf("startup diagnostic missing: %+v", event)
	}
	j, err := f.s.Read(f.r.Ref)
	if err != nil || j.Ready || j.Phase != Failed {
		t.Fatal("stderr changed first-task acceptance")
	}
}

func TestCodexStderrRedactsAuthentication(t *testing.T) {
	patternValue := "ghp_" + strings.Repeat("A1b2", 9)
	for _, diagnostic := range []string{
		"Authorization:\nBearer opaque-fixture-value\n",
		"refresh_token: opaque-fixture-value\n",
		"-----BEGIN PRIVATE " + "KEY-----\nopaque-fixture-value\n",
		"gitleaks:allow " + patternValue,
		"unexpected value " + patternValue,
	} {
		w := &codexStderr{}
		// os/exec can split labels and values across writes.
		for i := range len(diagnostic) {
			if _, err := w.Write([]byte(diagnostic[i : i+1])); err != nil {
				t.Fatal(err)
			}
		}
		var log bytes.Buffer
		if err := w.writeLog(&log); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(log.String(), "opaque-fixture-value") || strings.Contains(log.String(), patternValue) {
			t.Fatal("authentication output reached the log")
		}
		if !strings.Contains(log.String(), "redacted") {
			t.Fatal("missing redaction marker")
		}
	}
}

func TestCodexStderrBoundsAndDropsPartialLine(t *testing.T) {
	w := &codexStderr{}
	input := codexStartupDiagnostic + strings.Repeat("x", codexStderrLimit*2)
	if n, err := w.Write([]byte(input)); n != len(input) || err != nil {
		t.Fatal("stderr copier was not drained")
	}
	if len(w.data) != codexStderrLimit {
		t.Fatal("stderr buffer is not bounded")
	}
	var log bytes.Buffer
	if err := w.writeLog(&log); err != nil {
		t.Fatal(err)
	}
	var event struct {
		Text      string `json:"text"`
		Truncated bool   `json:"truncated"`
	}
	if err := json.Unmarshal(log.Bytes(), &event); err != nil {
		t.Fatal(err)
	}
	if !event.Truncated || event.Text != codexStartupDiagnostic {
		t.Fatal("truncated diagnostic exposed a partial line")
	}
}
