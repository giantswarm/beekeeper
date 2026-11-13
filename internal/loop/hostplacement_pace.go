package loop

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/giantswarm/beekeeper/internal/capacity"
)

// PaceYield is why a host with room does not take a start under
// supervisor.hostsFirst: the coordinator's own pick (local, its capacity
// --json) is a subscription behind its weekly pace and the host's pick (host,
// capacity --host … --json) spends another provider, such as a subscription
// whose harness no host runs. Placed there, the start would leave the
// subscription furthest behind its pace unspent while the coordinator has
// room for it. "" when the host spends the coordinator's provider, or the
// coordinator's pick is not behind its pace.
func PaceYield(local, host string) string {
	pick, hostPick := capacityChoice(local), capacityChoice(host)
	behind, ok := pick.BehindPace()
	if !ok || hostPick != nil && strings.EqualFold(hostPick.Provider, pick.Provider) {
		return ""
	}
	return fmt.Sprintf("the coordinator's pick %s is %.1f points behind its weekly pace; this host's pick is %s", pick.Summary(), behind, hostPick.Summary())
}

// capacityChoice is the choice of a capacity --json answer, nil when it has
// none. The answer is stdout then stderr: the document is its first value.
func capacityChoice(out string) *capacity.ChoiceView {
	var v struct {
		Choice *capacity.ChoiceView `json:"choice"`
	}
	if json.NewDecoder(strings.NewReader(out)).Decode(&v) != nil {
		return nil
	}
	return v.Choice
}
