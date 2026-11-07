package update

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/giantswarm/go-selfupdate"
)

const (
	scenarioAbsent   = "absent"
	scenarioTampered = "tampered"
)

type refusingEvidence struct{ acceptingValidator }

func (*refusingEvidence) Validate(string, []byte, []byte) error {
	return errors.New("tampered signature")
}

func TestEvidenceGate(t *testing.T) {
	for _, route := range []string{"latest", "pinned", "given"} {
		for _, scenario := range []string{CanaryStatusSuccess, scenarioAbsent, probationPending, CanaryStatusFailed, scenarioTampered, "schema", "version", "digest", "os", "arch", "merge", "run", "recorded"} {
			t.Run(route+"/"+scenario, func(t *testing.T) {
				t.Setenv("XDG_STATE_HOME", t.TempDir())
				rel := release(latestV)
				src := &fakeSource{assets: map[int64][]byte{binaryID: []byte(newBinary), bundleID: []byte("bundle")}}
				setup(t, running, src)
				checkEvidence = requireEvidence
				newValidator = func() selfupdate.Validator { return &acceptingValidator{} }
				previous := evidenceValidator
				evidenceValidator = func() selfupdate.Validator { return &acceptingValidator{} }
				t.Cleanup(func() { evidenceValidator = previous })
				digest := sha256.Sum256([]byte(newBinary))
				e := CanaryEvidence{Schema: 1, Version: latestV, Commit: "0123456789abcdef0123456789abcdef01234567", SHA256: hex.EncodeToString(digest[:]), OS: runtime.GOOS, Arch: runtime.GOARCH, Status: CanaryStatusSuccess, Run: "sandbox-run", MergeCommit: "0123456789012345678901234567890123456789"}
				switch scenario {
				case probationPending:
					e.Status = CanaryStatusPending
				case CanaryStatusFailed:
					e.Status = CanaryStatusFailed
					e.Reason = "sandbox regression"
				case scenarioTampered:
					evidenceValidator = func() selfupdate.Validator { return &refusingEvidence{} }
				case "schema":
					e.Schema = 0
				case "version":
					e.Version = "v0.4.0"
				case "digest":
					e.SHA256 = "other-candidate"
				case "os":
					e.OS = "other"
				case "arch":
					e.Arch = "other"
				case "merge":
					e.MergeCommit = ""
				case "run":
					e.Run = ""
				case "recorded":
					e.Run = RecordedRunPrefix + "sandbox-run"
				}
				if scenario != scenarioAbsent {
					rel.assets = append(rel.assets, fakeAsset{3, binaryName() + ".canary.json"}, fakeAsset{4, binaryName() + ".canary.json.bundle"})
					src.assets[3], _ = json.Marshal(e)
					src.assets[4] = []byte("evidence bundle")
				}
				src.releases = []selfupdate.SourceRelease{rel}
				exe, old := installed(t)
				install := func() (Result, error) {
					switch route {
					case "pinned":
						return InstallVersion(context.Background(), io.Discard, latestV)
					case "given":
						oldTransport := http.DefaultTransport
						defer func() { http.DefaultTransport = oldTransport }()
						http.DefaultTransport = releaseTransport(func(r *http.Request) (*http.Response, error) {
							for _, a := range rel.assets {
								if r.URL.String() == downloadURL(latestV, a.GetName()) {
									return &http.Response{
										StatusCode: 200,
										Header:     make(http.Header),
										Body:       io.NopCloser(strings.NewReader(string(src.assets[a.GetID()]))),
										Request:    r,
									}, nil
								}
							}
							return &http.Response{StatusCode: 404, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("")), Request: r}, nil
						})
						var assets []Asset
						for _, a := range rel.assets {
							assets = append(assets, Asset{ID: a.GetID(), Name: a.GetName(), Size: len(src.assets[a.GetID()]), URL: downloadURL(latestV, a.GetName())})
						}
						return InstallRelease(context.Background(), io.Discard, Release{Tag: latestV, ID: 1, Assets: assets})
					default:
						return Run(context.Background(), io.Discard, false)
					}
				}
				result, err := install()
				if scenario == CanaryStatusSuccess {
					if err != nil || !result.Updated {
						t.Fatalf("install = %+v, %v", result, err)
					}
					got, _ := os.ReadFile(filepath.Clean(exe)) //nolint:gosec // test executable path
					if string(got) != newBinary {
						t.Fatal("candidate not installed")
					}
					return
				}
				if err == nil || result.Updated {
					t.Fatalf("unsafe install = %+v, %v", result, err)
				}
				assertUnchanged(t, exe, old)
				// A terminal authenticated failure survives loss/replacement of evidence;
				// pending and unavailable evidence can subsequently succeed.
				if scenario == CanaryStatusFailed || scenario == probationPending || scenario == scenarioAbsent {
					e.Status = CanaryStatusSuccess
					src.assets[3], _ = json.Marshal(e)
					if scenario == scenarioAbsent {
						rel.assets = append(rel.assets, fakeAsset{3, binaryName() + ".canary.json"}, fakeAsset{4, binaryName() + ".canary.json.bundle"})
						src.releases = []selfupdate.SourceRelease{rel}
						src.assets[4] = []byte("bundle")
					}
					result, err = install()
					if scenario == CanaryStatusFailed {
						if err == nil || result.Updated {
							t.Fatal("quarantine lost")
						}
						assertUnchanged(t, exe, old)
					} else if err != nil || !result.Updated {
						t.Fatalf("retry = %+v, %v", result, err)
					}
				}
			})
		}
	}
}
