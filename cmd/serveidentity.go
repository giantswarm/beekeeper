package cmd

import (
	"cmp"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/giantswarm/beekeeper/internal/identity"
)

func (a *app) serveIdentityCmd() *cobra.Command {
	var tool, args string
	c := &cobra.Command{
		Use:   "identity",
		Short: "Exchange serve's ServiceAccount token at Dex and show the identity muster sees",
		Long: `identity exchanges serve's projected ServiceAccount token
(serve.identity.tokenFile) at serve.issuer's Dex through the connector
serve.identity.connectorID (RFC 8693) and prints the Dex identity it gets:
its email, subject, audience, groups and expiry, never the token.

With --tool it calls that muster tool (through serve.muster's call_tool)
as the identity and prints the tool's text: the proof of what the identity
may read, and that it is refused a write.`,
		Example: `  beekeeper serve identity
  beekeeper serve identity --tool x_kubernetes_list --args '{"resourceType":"agenttemplates","allNamespaces":true}'`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ex, err := identity.NewExchanger(a.cfg.Serve.Issuer, a.cfg.Serve.Identity)
			if err != nil {
				return usageErr("%v", err)
			}
			var toolArgs map[string]any
			if tool != "" {
				if a.cfg.Serve.Muster == "" {
					return usageErr("serve.muster is not set: the muster --tool is called through")
				}
				if err := json.Unmarshal([]byte(cmp.Or(args, "{}")), &toolArgs); err != nil {
					return usageErr("--args: a JSON object: %v", err)
				}
			}
			token, err := ex.Token(cmd.Context())
			if err != nil {
				return err
			}
			if _, err := fmt.Fprintln(cmd.OutOrStdout(), describeToken(token)); err != nil || tool == "" {
				return err
			}
			out, err := callMuster(cmd.Context(), a.cfg.Serve.Muster, token, tool, toolArgs)
			if err != nil {
				return err
			}
			_, err = fmt.Fprintln(cmd.OutOrStdout(), out)
			return err
		},
	}
	c.Flags().StringVar(&tool, "tool", "", "the muster tool to call as the identity")
	c.Flags().StringVar(&args, "args", "", "the tool's arguments, a JSON object")
	return c
}

// describeToken words a Dex ID token's identity from its claims, unverified:
// the token comes straight from Dex, and the words never carry the token.
func describeToken(token string) string {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return "identity: the token is no JWT"
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return "identity: the token's claims do not decode: " + err.Error()
	}
	var c struct {
		Issuer   string   `json:"iss"`
		Subject  string   `json:"sub"`
		Audience audience `json:"aud"`
		Email    string   `json:"email"`
		Groups   []string `json:"groups"`
		Expiry   int64    `json:"exp"`
	}
	if err := json.Unmarshal(payload, &c); err != nil {
		return "identity: the token's claims do not decode: " + err.Error()
	}
	groups := "no groups"
	if len(c.Groups) > 0 {
		groups = "groups " + strings.Join(c.Groups, ", ")
	}
	return fmt.Sprintf("identity: %s (subject %s) from %s, for %s, %s, expires %s",
		cmp.Or(c.Email, "no email"), c.Subject, c.Issuer, strings.Join(c.Audience, ", "), groups,
		time.Unix(c.Expiry, 0).UTC().Format(time.RFC3339))
}

// audience is a JWT's aud: one string or a list.
type audience []string

func (a *audience) UnmarshalJSON(b []byte) error {
	var one string
	if err := json.Unmarshal(b, &one); err == nil {
		*a = audience{one}
		return nil
	}
	var many []string
	if err := json.Unmarshal(b, &many); err != nil {
		return err
	}
	*a = many
	return nil
}
