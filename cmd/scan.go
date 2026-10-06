package cmd

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/giantswarm/beekeeper/internal/claude"
	"github.com/giantswarm/beekeeper/internal/guard"
	"github.com/giantswarm/beekeeper/internal/state"
)

// scanDir holds the scanner's key and index under the state directory.
func (a *app) scanDir() string { return filepath.Join(a.cfg.StateDir, "scan") }

func (a *app) scanCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "scan",
		Short: "The transcript value scanner: the fingerprint index and the sweep over the transcripts",
		Long: `The scanner matches secret values, not command shapes. beekeeper keeps an
index of keyed fingerprints (HMAC-SHA256 under a key only beekeeper reads,
scan/key in the state directory) of every value it can reach, each naming
the reference to rotate; the index never holds a value. The PostToolUse
hook (beekeeper hook posttooluse) fingerprints the candidate strings of each
tool result against it before the model sees the result, and runs the token
patterns of the outbound guard (gitleaks' rules) beside it for values the
index does not hold.

Without a subcommand, prints what the index holds: its size, its
references and when it was built.`,
		Args: cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error { return a.scanStatus() },
	}
	c.AddCommand(&cobra.Command{
		Use:   "index",
		Short: "Rebuild the index from scan.sops and scan.vaults",
		Long: `index fingerprints every string value of the SOPS files scan.sops names
(decrypted with sops -d in beekeeper's own process) and every concealed
field of the 1Password vaults scan.vaults names (op item get --reveal), and
replaces what earlier runs indexed from them; values added with scan add
stay. Nothing it reads is printed or written: the counts are its output. A
source that cannot be read is named and the others are still indexed.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error { return a.scanIndex(cmd) },
	})
	c.AddCommand(&cobra.Command{
		Use:   "add <ref>",
		Short: "Index one value, read from stdin, under a reference",
		Long: `add reads one value from stdin and indexes it under ref, the name a hit
reports and the rotation note asks to rotate (op://vault/item/field,
sops://file#key, or any name). The value is never printed. A value shorter
than scan.minLength (12) is refused.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error { return a.scanAdd(cmd.InOrStdin(), args[0]) },
	})
	c.AddCommand(&cobra.Command{
		Use:   "sweep",
		Short: "Count the indexed references and token patterns in every transcript",
		Long: `sweep reads every transcript under claude.projectsDir: the sessions' and
subagents' .jsonl files and the spilled tool results (tool-results/*.txt).
It prints each reference and token rule found, base64-wrapped ones
included, with how often and in which files: never a value, never where in
a file. A reference listed was in a
transcript on disk and goes to its rotation.`,
		Args: cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error { return a.scanSweep() },
	})
	return c
}

func (a *app) scanStatus() error {
	ix, err := guard.LoadIndex(a.scanDir())
	if err != nil {
		return err
	}
	refs := ix.Refs()
	if a.json {
		return a.printJSON(struct {
			Fingerprints int      `json:"fingerprints"`
			Refs         []string `json:"refs"`
			Built        string   `json:"built,omitempty"`
		}{ix.Len(), refs, builtAt(ix.Built())})
	}
	if ix.Len() == 0 {
		_, err := fmt.Fprintln(a.out, "the index is empty: the hook matches the token patterns only (beekeeper scan index, beekeeper scan add)")
		return err
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%d fingerprints of %d references, built %s\n", ix.Len(), len(refs), builtAt(ix.Built()))
	for _, r := range refs {
		b.WriteString("  " + r + "\n")
	}
	_, err = io.WriteString(a.out, b.String())
	return err
}

// builtAt is t in RFC 3339, empty for the zero time.
func builtAt(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.Local().Format(time.RFC3339)
}

// scanFiles are the SOPS files scan.sops names, sorted; a glob's match of
// sops' own configuration, .sops.yaml, is none.
func (a *app) scanFiles() ([]string, error) {
	var files []string
	for _, g := range a.cfg.Scan.SOPS {
		m, err := filepath.Glob(g)
		if err != nil {
			return nil, usageErr("scan.sops: %q: %v", g, err)
		}
		files = append(files, slices.DeleteFunc(m, func(f string) bool { return filepath.Base(f) == ".sops.yaml" })...)
	}
	slices.Sort(files)
	return slices.Compact(files), nil
}

func (a *app) scanIndex(cmd *cobra.Command) error {
	ix, err := guard.OpenIndex(a.scanDir())
	if err != nil {
		return err
	}
	ix.MinLen = a.cfg.Scan.MinLength
	files, err := a.scanFiles()
	if err != nil {
		return err
	}
	ix.Drop(guard.SOPSRef, guard.OpRef)
	var failed []string
	var b strings.Builder
	n, err := guard.IndexSOPS(cmd.Context(), ix, files)
	if err != nil {
		failed = append(failed, err.Error())
	}
	fmt.Fprintf(&b, "sops: %d files, %d fingerprints\n", len(files), n)
	ops, err := a.secretOps()
	if err != nil {
		return err
	}
	for _, v := range a.cfg.Scan.Vaults {
		var env []string
		if v == ops.Vault && ops.Token != "" {
			env = []string{"OP_SERVICE_ACCOUNT_TOKEN=" + ops.Token}
		}
		n, err := guard.IndexVault(cmd.Context(), ix, v, env)
		if err != nil {
			failed = append(failed, err.Error())
		}
		fmt.Fprintf(&b, "op %s: %d fingerprints\n", v, n)
	}
	if err := ix.Save(a.now); err != nil {
		return err
	}
	fmt.Fprintf(&b, "index: %d fingerprints of %d references\n", ix.Len(), len(ix.Refs()))
	if _, err := io.WriteString(a.out, b.String()); err != nil {
		return err
	}
	if len(failed) > 0 {
		return fmt.Errorf("not indexed: %s", strings.Join(failed, "; "))
	}
	return nil
}

func (a *app) scanAdd(in io.Reader, ref string) error {
	raw, err := io.ReadAll(io.LimitReader(in, 1<<20))
	if err != nil {
		return err
	}
	ix, err := guard.OpenIndex(a.scanDir())
	if err != nil {
		return err
	}
	ix.MinLen = a.cfg.Scan.MinLength
	n := ix.Add(ref, string(raw))
	if n == 0 && !slices.Contains(ix.Refs(), ref) {
		return usageErr("nothing indexed: the value on stdin is shorter than scan.minLength (%d)", ix.MinLen)
	}
	if err := ix.Save(a.now); err != nil {
		return err
	}
	_, err = fmt.Fprintf(a.out, "indexed %s: %d fingerprints\n", ref, n)
	return err
}

// sweepPathsShown bounds the files the sweep prints under one finding.
const sweepPathsShown = 10

func (a *app) scanSweep() error {
	ix, err := guard.LoadIndex(a.scanDir())
	if err != nil {
		return err
	}
	rep, err := guard.SweepTranscripts(a.cfg.Claude.ProjectsDir, ix)
	if err != nil {
		return err
	}
	if a.json {
		return a.printJSON(rep)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%d transcript files, %d MiB, against %d fingerprints and the token patterns\n", rep.Files, rep.Bytes>>20, ix.Len())
	if len(rep.Leaks) == 0 {
		b.WriteString("nothing found\n")
	}
	for _, l := range rep.Leaks {
		fmt.Fprintf(&b, "  %-60s %6d× in %d files\n", l.Name(), l.Count, l.Files)
		for i, p := range l.Paths {
			if i == sweepPathsShown {
				fmt.Fprintf(&b, "      … %d more (--json lists them all)\n", len(l.Paths)-i)
				break
			}
			fmt.Fprintf(&b, "      %s\n", p)
		}
	}
	_, err = io.WriteString(a.out, b.String())
	return err
}

// postToolUse decides one PostToolUse event: a result carrying an indexed
// value or a token pattern is replaced by its redacted copy, a scan.redact
// event is logged, and each indexed reference gets a rotation note for the
// guide's person unless an open one names it. Any error: no answer.
func (a *app) postToolUse(raw []byte) []byte {
	if a.loadConfig() != nil {
		return nil
	}
	a.now = time.Now()
	return a.redactResult(raw)
}

// redactResult is postToolUse with the configuration loaded.
func (a *app) redactResult(raw []byte) []byte {
	ix, err := guard.LoadIndex(a.scanDir())
	if err != nil {
		ix = &guard.Index{} // the token patterns still run
	}
	out, r, found := guard.PostToolUse(raw, ix)
	if out == nil {
		return nil
	}
	if err := a.recordRedaction(r, found); err != nil {
		fmt.Fprintln(os.Stderr, guard.LogPrefix+"scan: the redaction is not recorded: "+err.Error())
	}
	return out
}

// sessionLabel names the session a result came from: its name and id, or
// its id alone when no running CLI names it.
func sessionLabel(p state.Party) string {
	if p.Name == noSession || p.Name == "" {
		return "session " + p.Session
	}
	return fmt.Sprintf("session %q (%s)", p.Name, p.Session)
}

// rotateNote opens the text of a rotation note; one per reference is open.
const rotateNote = "Rotate "

func (a *app) recordRedaction(r guard.ToolResult, found []guard.Finding) error {
	store, err := state.Open(a.cfg.StateDir)
	if err != nil {
		return err
	}
	who := state.Party{Session: r.Session, Name: noSession}
	if t, err := plat.Machine.Processes(); err == nil {
		if n := claude.RecordName(a.cfg, t, r.Session); n != "" {
			who.Name = n
		}
	}
	names := make([]string, len(found))
	for i, f := range found {
		names[i] = fmt.Sprintf("%s (%d)", f.Name(), f.Count)
	}
	return store.Update(func(st *state.State) ([]state.Event, error) {
		evs := []state.Event{event(who, "scan.redact", "%s result: %s", r.Tool, strings.Join(names, ", "))}
		for _, f := range found {
			if f.Ref == "" {
				continue
			}
			text := fmt.Sprintf("%s%s: its value was in a %s result of %s. The PostToolUse hook replaced it before the model and the transcript got it, but the command that printed it had it in reach. Rotate the value at its source, then mark this note done.",
				rotateNote, f.Ref, r.Tool, sessionLabel(who))
			if slices.ContainsFunc(st.Notes, func(n state.Note) bool { return strings.HasPrefix(n.Text, rotateNote+f.Ref+":") }) {
				continue
			}
			st.NextNote++
			n := state.Note{ID: st.NextNote, For: a.cfg.Guide.Person, By: who, At: a.now.UTC(), Text: text,
				Default: "the value stays valid"}
			st.Notes = append(st.Notes, n)
			evs = append(evs, event(who, "note.add", "#%d %s", n.ID, n.Text))
		}
		return evs, nil
	})
}
