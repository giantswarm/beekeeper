package update

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"runtime"
	"testing"

	"github.com/giantswarm/go-selfupdate"
)

type refusingEvidence struct{ acceptingValidator }

func (*refusingEvidence) Validate(string, []byte, []byte) error {
	return errors.New("tampered signature")
}

func TestEvidenceGate(t *testing.T) {
	for _, route := range []string{"latest", "pinned"} {
		for _, scenario := range []string{"success", "absent", "pending", "failed", "tampered", "schema", "version", "digest", "os", "arch", "merge", "run"} {
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
				e := CanaryEvidence{Schema: 1, Version: latestV, SHA256: hex.EncodeToString(digest[:]), OS: runtime.GOOS, Arch: runtime.GOARCH, Status: "success", Run: "sandbox-run", MergeCommit: "0123456789012345678901234567890123456789"}
				switch scenario {
				case "pending":
					e.Status = "pending"
				case "failed":
					e.Status = "failed"
					e.Reason = "sandbox regression"
				case "tampered":
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
				}
				if scenario != "absent" {
					rel.assets = append(rel.assets, fakeAsset{3, binaryName() + ".canary.json"}, fakeAsset{4, binaryName() + ".canary.json.bundle"})
					src.assets[3], _ = json.Marshal(e)
					src.assets[4] = []byte("evidence bundle")
				}
				src.releases = []selfupdate.SourceRelease{rel}
				exe, old := installed(t)
				install := func() (Result, error) {
					if route == "pinned" {
						return InstallVersion(context.Background(), io.Discard, latestV)
					}
					return Run(context.Background(), io.Discard, false)
				}
				result, err := install()
				if scenario == "success" {
					if err != nil || !result.Updated {
						t.Fatalf("install = %+v, %v", result, err)
					}
					got, _ := os.ReadFile(exe)
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
				if scenario == "failed" || scenario == "pending" || scenario == "absent" {
					e.Status = "success"
					src.assets[3], _ = json.Marshal(e)
					if scenario == "absent" {
						rel.assets = append(rel.assets, fakeAsset{3, binaryName() + ".canary.json"}, fakeAsset{4, binaryName() + ".canary.json.bundle"})
						src.releases = []selfupdate.SourceRelease{rel}
						src.assets[4] = []byte("bundle")
					}
					result, err = install()
					if scenario == "failed" {
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
