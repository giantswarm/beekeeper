package cmd

import (
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/giantswarm/beekeeper/internal/secret"
)

// The states a writing secret call ends in.
const (
	dryRunState  = "dry run"
	writtenState = "written"
)

func (a *app) secretRevealCmd() *cobra.Command {
	var to string
	var write bool
	c := &cobra.Command{
		Use:   "reveal <sops-file> <path>… | reveal <sops-file> <path> --to <plain.yaml#path> [--write]",
		Short: "Answer configuration a SOPS file encrypts with its secrets (ids, URLs, peer names), never a secret",
		Long: `A SOPS file encrypts every leaf, so configuration sits encrypted next to the
secrets: an OAuth client's id, name and redirect URIs, a peers list. reveal
answers the leaves the caller names, each path one scalar or every scalar
under it, in plaintext, once beekeeper's classifier agrees each one is
configuration: no key on its path named like a secret (secret, password,
token, key, credential, private, cookie, salt, hmac, cert), no value the
value scanner matches (a token pattern, an indexed secret), no URL carrying
a password, no run of 16 or more characters at a key's entropy. A client
id (a leaf named id, clientID or client_id, an item of a trustedPeers or
peers list) is public in every authorize URL and answered however random it
looks, unless it equals a value under a secret-named key of the same file.
One that looks secret refuses the whole call (exit 3), named by its path and the
reason, never its value; copy and compare move and check a secret.

--json answers [{"path": …, "value": …}], a tool's input:

  beekeeper --json secret reveal secret-values.yaml.patch \
    oidc.extraStaticClients.0.id oidc.extraStaticClients.0.redirectURIs

--to <plain.yaml#path> writes what one path names into a path of a
plaintext YAML file instead (created when absent, its other keys kept)
and prints no value: without --write it answers what it would write, with
it the file is written; a destination holding the same answers unchanged.`,
		Args: cobra.MinimumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			file, paths := args[0], args[1:]
			if err := checkSOPSFile(file); err != nil {
				return err
			}
			ops, err := a.secretOpsScanned()
			if err != nil {
				return err
			}
			if to == "" {
				if write {
					return usageErr("--write writes --to's file; reveal alone answers")
				}
				if err := a.sandboxFiles([]secret.Ref{{File: file}}, nil); err != nil {
					return err
				}
				fs, err := ops.Reveal(cmd.Context(), file, paths)
				if err := a.secretLog(classified(err), "reveal", "%s %s: %s", file, strings.Join(paths, " "), outcome(err, fmt.Sprintf("%d leaves", len(fs)))); err != nil {
					return err
				}
				var b strings.Builder
				for _, f := range fs {
					fmt.Fprintf(&b, "%-60s %s\n", f.Path, strconv.Quote(f.Value))
				}
				return a.secretPrint(fs, b.String())
			}
			if len(paths) != 1 {
				return usageErr("--to takes one path: reveal <sops-file> <path> --to <plain.yaml#path>")
			}
			dst, err := parseRefs(to)
			if err != nil {
				return err
			}
			if err := a.sandboxFiles([]secret.Ref{{File: file}}, dst); err != nil {
				return err
			}
			res, err := ops.RevealTo(cmd.Context(), file, paths[0], dst[0], write)
			state := dryRunState
			switch {
			case res.Unchanged:
				state = "unchanged"
			case res.Written:
				state = writtenState
			}
			if err := a.secretLog(classified(err), "reveal", "%s#%s to %s: %s", file, paths[0], dst[0], outcome(err, fmt.Sprintf("%d leaves, %s", len(res.Paths), state))); err != nil {
				return err
			}
			verb := map[string]string{dryRunState: "would write", "unchanged": "holds already", writtenState: "wrote"}[state]
			var b strings.Builder
			fmt.Fprintf(&b, "%s %s: %d leaves\n", verb, res.To, len(res.Paths))
			for _, p := range res.Paths {
				fmt.Fprintf(&b, "  %s\n", p)
			}
			return a.secretPrint(res, b.String())
		},
	}
	c.Flags().StringVar(&to, "to", "", "write the configuration into a path of a plaintext YAML file: <plain.yaml#path>")
	c.Flags().BoolVar(&write, "write", false, "write --to's file; without it reveal --to answers what it would write")
	return c
}

func (a *app) secretUnsetCmd() *cobra.Command {
	var write bool
	c := &cobra.Command{
		Use:   "unset <sops-file> <path>… [--write]",
		Short: "Remove keys from a SOPS file, its recipients kept",
		Long: `unset removes keys from a SOPS file, each path a dotted key path (a number
indexes a list) naming a value, a mapping or a list item. sops unset runs in
beekeeper's process with the file's age identity: the file keeps its
recipients, sops renews its MAC and lastmodified. Without --write it answers
what it would remove; with it the file is written. A path the file holds no
longer is answered absent, so a second run removes nothing. It answers the
paths removed, those absent and the key names the file keeps, never a value.

  beekeeper secret unset secret-values.yaml.patch oidc.extraStaticClients \
    oidc.staticClients.muster --write`,
		Args: cobra.MinimumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			file, paths := args[0], args[1:]
			if err := checkSOPSFile(file); err != nil {
				return err
			}
			if err := a.sandboxFiles(nil, []secret.Ref{{File: file}}); err != nil {
				return err
			}
			ops, err := a.secretOps()
			if err != nil {
				return err
			}
			res, err := ops.Unset(cmd.Context(), file, paths, write)
			state := dryRunState
			if res.Written {
				state = writtenState
			}
			if err := a.secretLog(err, "unset", "%s %s: %s", file, strings.Join(paths, " "), outcome(err, fmt.Sprintf("%d removed, %d absent, %d keys kept, %s", len(res.Removed), len(res.Absent), len(res.Kept), state))); err != nil {
				return err
			}
			verb := "would remove"
			if res.Written {
				verb = "removed"
			}
			var b strings.Builder
			for _, p := range res.Removed {
				fmt.Fprintf(&b, "%s %s\n", verb, p)
			}
			for _, p := range res.Absent {
				fmt.Fprintf(&b, "absent %s\n", p)
			}
			fmt.Fprintf(&b, "%s keeps %d keys\n", file, len(res.Kept))
			for _, k := range res.Kept {
				fmt.Fprintf(&b, "  %s\n", k)
			}
			return a.secretPrint(res, b.String())
		},
	}
	c.Flags().BoolVar(&write, "write", false, "write the file; without it unset answers what it would remove")
	return c
}

// checkSOPSFile refuses a reference where reveal and unset take a file
// and paths.
func checkSOPSFile(file string) error {
	r, err := secret.ParseRef(file)
	if err != nil {
		return usageErr("%v", err)
	}
	if r.Op != "" || r.IsKube() || r.Path != "" {
		return usageErr("%s: name the SOPS file, then its dotted paths as arguments", file)
	}
	return nil
}

// classified makes reveal's classifier refusal a refusal (exit 3).
func classified(err error) error {
	if err != nil && errors.Is(err, secret.ErrSecretLike) {
		return refused("%v", err)
	}
	return err
}

// secretOpsScanned are the operations with the value scanner's index,
// which reveal checks every value against.
func (a *app) secretOpsScanned() (*secret.Ops, error) {
	ops, ix, err := a.secretOpsIndexed()
	if err != nil {
		return nil, err
	}
	ops.Index = ix
	return ops, nil
}
