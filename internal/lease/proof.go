package lease

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
)

// Proof is the destructive purpose left by the preceding holder.
type Proof struct {
	By      string `json:"by"`
	Purpose string `json:"purpose"`
}

// IsDestructiveProof recognizes purposes that may leave a lab disrupted.
func IsDestructiveProof(purpose string) bool {
	p := strings.ToLower(purpose)
	p = strings.NewReplacer("-", " ", "/", " ", "_", " ").Replace(p)
	words := strings.Fields(p)
	for i, word := range words {
		if word == "down" {
			return true
		}
		if word == "node" && i+1 < len(words) && (words[i+1] == "restart" || words[i+1] == "restarts") {
			return true
		}
	}
	return false
}

func (d Dir) proofPath(res string) string {
	return filepath.Join(string(d), ".proof-"+hex.EncodeToString([]byte(res))+".json")
}

// PreviousProof reads the last released holder's destructive purpose.
// The notice remains available for retries until the next release.
func (d Dir) PreviousProof(res string) (*Proof, error) {
	raw, err := os.ReadFile(d.proofPath(res))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var p *Proof
	err = json.Unmarshal(raw, &p)
	return p, err
}
