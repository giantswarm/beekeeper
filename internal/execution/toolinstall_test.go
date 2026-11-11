package execution

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestUnpackPinnedTool(t *testing.T) {
	for _, tc := range []struct {
		name    string
		headers []*tar.Header
		fail    bool
	}{
		{"complete Go root", []*tar.Header{{Name: "go/", Typeflag: tar.TypeDir}, {Name: "go/bin/go", Mode: 0755, Size: 3}, {Name: "go/src/runtime/runtime.go", Mode: 0644, Size: 3}}, false},
		{"traversal", []*tar.Header{{Name: "go/../outside", Size: 3}}, true},
		{"absolute", []*tar.Header{{Name: "go//outside", Size: 3}}, true},
		{"link", []*tar.Header{{Name: "go/bin/go", Typeflag: tar.TypeSymlink, Linkname: "/elsewhere"}}, true},
		{"root link", []*tar.Header{{Name: "go/", Typeflag: tar.TypeSymlink, Linkname: "/elsewhere"}}, true},
		{"missing executable", []*tar.Header{{Name: "go/README", Size: 3}}, true},
		{"unexpected prefix", []*tar.Header{{Name: "other/bin/go", Size: 3}}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var data bytes.Buffer
			gz := gzip.NewWriter(&data)
			tw := tar.NewWriter(gz)
			for _, h := range tc.headers {
				if err := tw.WriteHeader(h); err != nil {
					t.Fatal(err)
				}
				if h.Size > 0 {
					if _, err := tw.Write([]byte("exe")); err != nil {
						t.Fatal(err)
					}
				}
			}
			if err := tw.Close(); err != nil {
				t.Fatal(err)
			}
			if err := gz.Close(); err != nil {
				t.Fatal(err)
			}
			dest := t.TempDir()
			err := unpackTool(data.Bytes(), goArchivePrefix, dest, false)
			if (err != nil) != tc.fail {
				t.Fatalf("unpack = %v", err)
			}
			if !tc.fail {
				info, err := os.Stat(filepath.Join(dest, toolBinDirectory, ToolGo))
				if err != nil || info.Mode()&0111 == 0 {
					t.Fatalf("executable: %v %v", info, err)
				}
			}
		})
	}
}

func TestProvisionToolsPreservesExistingMismatchedInstallation(t *testing.T) {
	f := testFixture(t)
	exe := filepath.Join(t.TempDir(), "gh")
	if err := os.WriteFile(exe, []byte("original"), 0600); err != nil {
		t.Fatal(err)
	}
	f.s.Config.Tools[ToolGitHub] = ToolConfig{Executable: exe, Version: fixtureGitHubVersion}
	f.s.Command = func(context.Context, string, []string, string, ...string) ([]byte, error) {
		return []byte("wrong version"), nil
	}
	if err := f.s.ProvisionTools(context.Background()); err == nil || !strings.Contains(err.Error(), "installation exists") {
		t.Fatalf("provision: %v", err)
	}
	got, err := readRegular(exe, MaxWire)
	if err != nil || string(got) != "original" {
		t.Fatal("existing installation changed")
	}
}

type toolRoundTrip func(*http.Request) (*http.Response, error)

func (f toolRoundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestInstallPinnedToolVerifiesBeforeInstalling(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("execution hosts require Linux")
	}
	for _, name := range []string{ToolGo, ToolGitHub} {
		for _, corrupt := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/corrupt=%v", name, corrupt), func(t *testing.T) {
				version := fixtureGoVersion
				archiveName := "go" + version + ".linux-" + runtime.GOARCH + ".tar.gz"
				prefix := goArchivePrefix
				if name == ToolGitHub {
					version = fixtureGitHubVersion
					archiveName = "gh_" + version + "_linux_" + runtime.GOARCH + ".tar.gz"
					prefix = strings.TrimSuffix(archiveName, ".tar.gz") + "/"
				}
				var buf bytes.Buffer
				gz := gzip.NewWriter(&buf)
				tw := tar.NewWriter(gz)
				executable := []byte("fixture executable")
				if err := tw.WriteHeader(&tar.Header{Name: prefix + "bin/" + name, Mode: 0755, Size: int64(len(executable))}); err != nil {
					t.Fatal(err)
				}
				if _, err := tw.Write(executable); err != nil {
					t.Fatal(err)
				}
				if err := tw.Close(); err != nil {
					t.Fatal(err)
				}
				if err := gz.Close(); err != nil {
					t.Fatal(err)
				}
				data := buf.Bytes()
				sum := fmt.Sprintf("%x", sha256.Sum256(data))
				if corrupt {
					data = append(append([]byte(nil), data...), byte(1))
				}
				client := &http.Client{Transport: toolRoundTrip(func(r *http.Request) (*http.Response, error) {
					body := data
					switch {
					case r.URL.Host == "go.dev" && r.URL.Path == "/dl/":
						body = []byte(fmt.Sprintf(`[{"files":[{"filename":%q,"sha256":%q}]}]`, archiveName, sum))
					case strings.HasSuffix(r.URL.Path, "_checksums.txt"):
						body = []byte(sum + "  " + archiveName + "\n")
					}
					return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(bytes.NewReader(body))}, nil
				})}
				exe := filepath.Join(t.TempDir(), "install", toolBinDirectory, name)
				err := installToolWithClient(context.Background(), name, ToolConfig{Executable: exe, Version: version}, client)
				if corrupt {
					if err == nil || !strings.Contains(err.Error(), "checksum mismatch") {
						t.Fatalf("corrupt archive: %v", err)
					}
					if _, err := os.Lstat(exe); !os.IsNotExist(err) {
						t.Fatal("corrupt tool installed")
					}
					return
				}
				if err != nil {
					t.Fatal(err)
				}
				got, err := readRegular(exe, MaxWire)
				if err != nil || !bytes.Equal(got, executable) {
					t.Fatalf("installed executable: %q %v", got, err)
				}
			})
		}
	}
}

func TestProvisionToolsRaisesGoPinForServedRepositories(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("execution hosts require Linux")
	}
	const newGoVersion = "1.27.4"
	archiveName := "go" + newGoVersion + ".linux-" + runtime.GOARCH + ".tar.gz"
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	executable := []byte("#!/bin/sh\necho go version go" + newGoVersion + " linux/" + runtime.GOARCH + "\n")
	if err := tw.WriteHeader(&tar.Header{Name: goArchivePrefix + "bin/go", Mode: 0o755, Size: int64(len(executable))}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(executable); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	data := buf.Bytes()
	sum := fmt.Sprintf("%x", sha256.Sum256(data))
	prevClient := toolHTTPClient
	t.Cleanup(func() { toolHTTPClient = prevClient })
	toolHTTPClient = &http.Client{Transport: toolRoundTrip(func(r *http.Request) (*http.Response, error) {
		body := data
		if r.URL.Host == "go.dev" && r.URL.Path == "/dl/" {
			body = []byte(fmt.Sprintf(`[{"files":[{"filename":%q,"sha256":%q}]}]`, archiveName, sum))
		}
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(bytes.NewReader(body))}, nil
	})}

	f := testFixture(t)
	toolsParent := filepath.Join(t.TempDir(), "tools")
	oldExe := filepath.Join(toolsParent, "go"+fixtureGoVersion, toolBinDirectory, ToolGo)
	newExe := filepath.Join(toolsParent, "go"+newGoVersion, toolBinDirectory, ToolGo)
	if err := os.MkdirAll(filepath.Dir(oldExe), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(oldExe, []byte("old-go"), 0o700); err != nil {
		t.Fatal(err)
	}
	f.s.Config.Tools[ToolGo] = ToolConfig{Executable: oldExe, Version: fixtureGoVersion}
	next := f.s.Command
	f.s.Command = func(ctx context.Context, dir string, env []string, bin string, args ...string) ([]byte, error) {
		switch bin {
		case oldExe:
			return []byte("go version go" + fixtureGoVersion + " linux/amd64\n"), nil
		case newExe:
			if _, err := os.Stat(newExe); err != nil {
				return nil, err
			}
			return []byte("go version go" + newGoVersion + " linux/amd64\n"), nil
		}
		return next(ctx, dir, env, bin, args...)
	}
	if err := f.s.ProvisionToolsFor(context.Background(), map[string]string{ToolGo: newGoVersion}); err != nil {
		t.Fatal(err)
	}
	if got := f.s.Config.Tools[ToolGo]; got != (ToolConfig{Executable: newExe, Version: newGoVersion}) {
		t.Fatalf("provisioned go tool = %+v, want %s at %s", got, newGoVersion, newExe)
	}
	if _, err := f.s.Prepare(f.s.Policy()); err != nil {
		t.Fatal(err)
	}
	caps, err := f.s.Check(context.Background())
	if err != nil || caps.Tools[ToolGo] != newGoVersion {
		t.Fatalf("Check after raising Go pin = %+v, %v", caps.Tools, err)
	}
}
