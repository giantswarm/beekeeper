package update

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"

	"github.com/giantswarm/go-selfupdate"
	selfupdatecosign "github.com/giantswarm/selfupdate-cosign"
)

// CanaryEvidence is signed separately from the binary, only after the staging
// workflow observes the sandbox merge. Schema changes require a new gate.
type CanaryEvidence struct {
	Schema      int    `json:"schema"`
	Version     string `json:"version"`
	Commit      string `json:"commit,omitempty"`
	SHA256      string `json:"sha256"`
	OS          string `json:"os"`
	Arch        string `json:"arch"`
	Status      string `json:"status"`
	Run         string `json:"run"`
	MergeCommit string `json:"merge_commit"`
	Reason      string `json:"reason,omitempty"`
}

// VerifyCandidateBundle verifies a staged candidate binary against its cosign
// bundle using the same release identity policy as self-update.
func VerifyCandidateBundle(assetName string, binary, bundle []byte) error {
	return newValidator().Validate(assetName, binary, bundle)
}

// ValidateEvidence validates raw canary evidence JSON for the current platform
// against the expected release version tag and candidate binary SHA-256 digest.
func ValidateEvidence(data []byte, version, digest string) (CanaryEvidence, error) {
	return ValidateEvidenceFor(data, version, digest, runtime.GOOS, runtime.GOARCH)
}

// ValidateEvidenceFor validates raw canary evidence JSON for a target OS/Arch
// against the expected release version tag and candidate binary SHA-256 digest.
func ValidateEvidenceFor(data []byte, version, digest, goos, goarch string) (CanaryEvidence, error) {
	return validateEvidenceTarget(data, version, digest, goos, goarch)
}

// CanaryEvidence status values.
const (
	CanaryStatusSuccess = "success"
	CanaryStatusFailed  = "failed"
	CanaryStatusPending = "pending"
)

var evidenceValidator = func() selfupdate.Validator { return selfupdatecosign.New(Repository) }
var checkEvidence = requireEvidence

func requireEvidence(ctx context.Context, source selfupdate.Source, rel *selfupdate.Release, path string) error {
	binary, err := os.ReadFile(filepath.Clean(path)) //nolint:gosec // verified staged binary path
	if err != nil {
		return err
	}
	sum := sha256.Sum256(binary)
	digest := hex.EncodeToString(sum[:])
	stateDir := os.Getenv("XDG_STATE_HOME")
	if stateDir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return fmt.Errorf("locating canary quarantine: %w", err)
		}
		stateDir = filepath.Join(home, ".local", "state")
	}
	quarantine := filepath.Join(stateDir, "beekeeper", "canary-quarantine", digest+".json")
	if reason, err := os.ReadFile(filepath.Clean(quarantine)); err == nil { //nolint:gosec // local state quarantine path
		return fmt.Errorf("candidate quarantined: %s", reason)
	} else if !os.IsNotExist(err) {
		return err
	}
	releases, err := source.ListReleases(ctx, selfupdate.ParseSlug(Repository))
	if err != nil {
		return fmt.Errorf("canary evidence lookup (retryable): %w", err)
	}
	name := rel.AssetName + ".canary.json"
	var evidenceID, bundleID int64
	for _, release := range releases {
		if release.GetTagName() != "v"+rel.Version() {
			continue
		}
		for _, asset := range release.GetAssets() {
			switch asset.GetName() {
			case name:
				evidenceID = asset.GetID()
			case name + ".bundle":
				bundleID = asset.GetID()
			}
		}
	}
	if evidenceID == 0 || bundleID == 0 {
		return fmt.Errorf("authenticated canary evidence unavailable (retryable)")
	}
	download := func(id int64) ([]byte, error) {
		r, err := source.DownloadReleaseAsset(ctx, rel, id)
		if err != nil {
			return nil, err
		}
		defer func() { _ = r.Close() }()
		data, err := io.ReadAll(io.LimitReader(r, 1024*1024+1))
		if len(data) > 1024*1024 {
			return nil, fmt.Errorf("canary asset exceeds size limit")
		}
		return data, err
	}
	data, err := download(evidenceID)
	if err != nil {
		return err
	}
	bundle, err := download(bundleID)
	if err != nil {
		return err
	}
	if err := evidenceValidator().Validate(name, data, bundle); err != nil {
		return fmt.Errorf("authenticating canary evidence: %w", err)
	}
	evidence, err := validateEvidence(data, "v"+rel.Version(), digest)
	if err != nil {
		return err
	}
	if evidence.Status == CanaryStatusFailed {
		// Only authenticated, matching terminal failure is durable. Service errors,
		// pending runs and malformed evidence must never poison the retry queue.
		if err := os.MkdirAll(filepath.Dir(quarantine), 0o700); err != nil { //nolint:gosec // local state quarantine directory
			return err
		}
		if err := os.WriteFile(quarantine, data, 0o600); err != nil { //nolint:gosec // local state quarantine file
			return err
		}
		return fmt.Errorf("candidate quarantined: %s", evidence.Reason)
	}
	if evidence.Status != CanaryStatusSuccess {
		return fmt.Errorf("canary pending (retryable)")
	}
	return nil
}

func validateEvidence(data []byte, version, digest string) (CanaryEvidence, error) {
	return validateEvidenceTarget(data, version, digest, runtime.GOOS, runtime.GOARCH)
}

func validateEvidenceTarget(data []byte, version, digest, goos, goarch string) (CanaryEvidence, error) {
	var e CanaryEvidence
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&e); err != nil {
		return e, err
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return e, fmt.Errorf("trailing canary evidence")
	}
	if e.Schema != 1 || e.Version != version || e.SHA256 != digest || e.OS != goos || e.Arch != goarch {
		return e, fmt.Errorf("canary evidence schema or candidate identity mismatch")
	}
	if e.Run == "" || (e.Status == CanaryStatusSuccess && e.MergeCommit == "") || (e.Status == CanaryStatusFailed && e.Reason == "") {
		return e, fmt.Errorf("incomplete canary evidence")
	}
	if e.Status != CanaryStatusSuccess && e.Status != CanaryStatusFailed && e.Status != CanaryStatusPending {
		return e, fmt.Errorf("unknown canary status")
	}
	return e, nil
}
