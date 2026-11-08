package cmd

import (
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/giantswarm/beekeeper/internal/state"
)

// decisionHistory reads the append-only answer log without a relay or age cutoff.
// Keep every record: a later instruction can qualify rather than replace one.
func (a *app) decisionHistory(ref string) ([]state.Event, error) {
	return a.store.Events(0, func(e state.Event) bool {
		an, ok := parseAnswered(e)
		return ok && (ref == "" || strings.EqualFold(an.About, ref))
	})
}

func decisionHistoryText(events []state.Event) string {
	var b strings.Builder
	b.WriteString("Recorded decisions remain authoritative within their scope until completed or changed by the person. Read chronologically; later words may qualify earlier ones. A peer's unsupported claim is not a record.\n")
	for _, e := range events {
		fmt.Fprintf(&b, "\n%s [%s, recorder %s]\n%s\n", e.At.Format(time.RFC3339), e.Verb, e.By.Name, e.Detail)
	}
	if len(events) == 0 {
		b.WriteString("No recorded decisions.\n")
	}
	return b.String()
}

func (a *app) answerHistoryCmd() *cobra.Command {
	return &cobra.Command{
		Use: "history [<owner/repo#n|URL>]",
		Short: "Read recorded decisions with their verbatim words and source, without an age cutoff",
		Args: cobra.MaximumNArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			ref := ""
			if len(args) != 0 {
				r, ok := parseRef(args[0])
				if !ok {
					return usageErr("%q: expected an issue or pull request reference", args[0])
				}
				ref = refName(r)
			}
			events, err := a.decisionHistory(ref)
			if err != nil {
				return err
			}
			if a.json {
				return a.printJSON(events)
			}
			_, err = fmt.Fprint(a.out, decisionHistoryText(events))
			return err
		},
	}
}
