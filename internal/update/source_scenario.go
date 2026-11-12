//go:build scenario

package update

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/giantswarm/go-selfupdate"
)

// A build with the scenario tag (docs/scenarios.md) runs in a sandbox that
// has no network, no Sigstore and no CircleCI identity. Its releases are
// those of the scenario's GitHub, which BEEKEEPER_GITHUB_API names as for
// every other GitHub call, and a download is verified against a scenario
// bundle, the binary's SHA-256, where a release build verifies a cosign
// bundle. Everything after the verification is the release build's own:
// the journal, the canary, the replacement, probation and recovery.
//
// Without the variable a scenario build looks releases up and verifies
// them as a release build does.

// ScenarioBundlePrefix starts a scenario bundle: the SHA-256 of the binary
// it is published next to follows, in hexadecimal.
const ScenarioBundlePrefix = "beekeeper-scenario-bundle sha256:"

// ScenarioBundle is the bundle the scenario's GitHub publishes next to
// binary.
func ScenarioBundle(binary []byte) []byte {
	sum := sha256.Sum256(binary)
	return []byte(ScenarioBundlePrefix + hex.EncodeToString(sum[:]) + "\n")
}

// apiBase is the scenario's GitHub, "" outside a scenario.
func apiBase() string {
	return strings.TrimSuffix(os.Getenv("BEEKEEPER_GITHUB_API"), "/")
}

// verifyScenarioBundle refuses a download that is not the binary its
// bundle names, as a cosign bundle of another binary is refused.
func verifyScenarioBundle(name string, data, bundle []byte) error {
	if !bytes.HasPrefix(bundle, []byte(ScenarioBundlePrefix)) {
		return errors.New(name + ": its bundle is no scenario bundle")
	}
	if !bytes.Equal(bytes.TrimSpace(bundle), bytes.TrimSpace(ScenarioBundle(data))) {
		return fmt.Errorf("%s: the download does not match its scenario bundle", name)
	}
	return nil
}

func init() {
	if apiBase() == "" {
		return
	}
	validator := func() selfupdate.Validator { return funcValidator{fn: verifyScenarioBundle} }
	newValidator, evidenceValidator = validator, validator
}
