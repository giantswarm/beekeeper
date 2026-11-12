//go:build scenario

package fakegithub

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/itchyny/gojq"
)

// The fake gh: a translator of the gh CLI's commands into requests to the
// server, which renders gh's output and exit codes for a pipe (no
// terminal, no color). It holds no state of its own, so what a command
// reads and spends is what the same request over HTTP reads and spends.
//
// A command, a flag or a --json field it does not translate is not
// answered: GHResult.Handled is false and the scenario fails on the call.

// GHCall is one call of gh.
type GHCall struct {
	Args  []string
	Stdin string
	// Dir is the directory gh was called in, Env its environment as far
	// as gh reads it (GH_REPO, the caller's name).
	Dir string
	Env map[string]string
}

// GHResult is what gh printed and exited with. Handled is false for a
// call the translator does not serve; Reason then says what it lacks.
type GHResult struct {
	Stdout  string
	Stderr  string
	Exit    int
	Handled bool
	Reason  string
}

// Token is what the fake gh auth token prints: a token of no account. It
// is written in two parts so that no secret scanner reads a GitHub token
// here.
const Token = "ghs_" + "scenarioFakeGitHubToken0000000000000000"

// ghRun is one call being translated.
type ghRun struct {
	s      *Server
	ctx    context.Context
	call   GHCall
	caller string
	out    bytes.Buffer
	err    bytes.Buffer
}

// errUnhandled is a call the translator does not serve.
type errUnhandled struct{ reason string }

func (e *errUnhandled) Error() string { return e.reason }

func unhandled(format string, args ...any) error {
	return &errUnhandled{reason: fmt.Sprintf(format, args...)}
}

// errExit ends a command with an exit code, its message already printed.
type errExit struct{ code int }

func (e *errExit) Error() string { return "exit " + strconv.Itoa(e.code) }

// errSilent is gh's SilentError: exit 1.
var errSilent = &errExit{code: 1}

// GH translates one call of gh.
func (s *Server) GH(ctx context.Context, call GHCall) GHResult {
	g := &ghRun{s: s, ctx: ctx, call: call, caller: call.Env[CallerEnv]}
	err := g.dispatch()
	res := GHResult{Stdout: g.out.String(), Stderr: g.err.String(), Handled: true}
	var un *errUnhandled
	var exit *errExit
	switch {
	case err == nil:
	case errors.As(err, &un):
		return GHResult{Handled: false, Reason: un.reason}
	case errors.As(err, &exit):
		res.Exit = exit.code
	default:
		// gh prints an error and exits 1.
		fmt.Fprintln(&g.err, err.Error())
		res.Stderr, res.Exit = g.err.String(), 1
	}
	return res
}

func (g *ghRun) dispatch() error {
	args := g.call.Args
	if len(args) == 0 {
		return unhandled("gh without a command")
	}
	group, rest := args[0], args[1:]
	if group == "api" {
		return g.api(rest)
	}
	if len(rest) == 0 {
		return unhandled("gh %s without a subcommand", group)
	}
	cmd, rest := rest[0], rest[1:]
	commands := map[string]func([]string) error{
		"auth token": g.authToken, "auth status": g.authStatus,
		"pr view": g.prView, "pr list": g.prList, "pr checks": g.prChecks, "pr create": g.prCreate, "pr merge": g.prMerge,
		"pr close": g.prClose, "pr reopen": g.prReopen, "pr ready": g.prReady, "pr comment": g.prComment, "pr review": g.prReview,
		"pr edit": g.prEdit, "pr diff": g.prDiff, "pr update-branch": g.prUpdateBranch,
		"issue view": g.issueView, "issue list": g.issueList, "issue create": g.issueCreate, "issue close": g.issueClose,
		"issue reopen": g.issueReopen, "issue comment": g.issueComment, "issue edit": g.issueEdit,
		"repo view": g.repoView, "label create": g.labelCreate, "label list": g.labelList, "search prs": g.searchPRs,
		"run list": g.runList, "run view": g.runView, "run rerun": g.runRerun,
		"release list": g.releaseList, "release view": g.releaseView, "release create": g.releaseCreate,
		"release delete": g.releaseDelete, "release download": g.releaseDownload, "release upload": g.releaseUpload,
	}
	run, ok := commands[group+" "+cmd]
	if !ok {
		return unhandled("the fake gh does not translate gh %s %s", group, cmd)
	}
	return run(rest)
}

// flags are a command's parsed flags and positional arguments.
type flags struct {
	vals map[string][]string
	pos  []string
}

func (f flags) has(name string) bool { return len(f.vals[name]) > 0 }

func (f flags) str(name string) string {
	if v := f.vals[name]; len(v) > 0 {
		return v[len(v)-1]
	}
	return ""
}

func (f flags) all(name string) []string { return f.vals[name] }

// list is a flag's values, each split at its commas: gh's string slices.
func (f flags) list(name string) []string {
	var out []string
	for _, v := range f.vals[name] {
		for _, part := range strings.Split(v, ",") {
			if part = strings.TrimSpace(part); part != "" {
				out = append(out, part)
			}
		}
	}
	return out
}

func (f flags) on(name string) bool { return f.has(name) && f.str(name) != "false" }

// parseFlags parses a command's arguments as pflag does. bools and
// values are the flags it knows, each "name" or "name|x" with its
// shorthand; any other flag is one the translator does not serve.
func parseFlags(command string, args []string, bools, values []string) (flags, error) {
	kind := map[string]string{}  // "--name" or "-x" → "bool" or "value"
	canon := map[string]string{} // → name
	for k, specs := range map[string][]string{"bool": bools, "value": values} {
		for _, spec := range specs {
			name, short, _ := strings.Cut(spec, "|")
			kind["--"+name], canon["--"+name] = k, name
			if short != "" {
				kind["-"+short], canon["-"+short] = k, name
			}
		}
	}
	f := flags{vals: map[string][]string{}}
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--":
			f.pos = append(f.pos, args[i+1:]...)
			return f, nil
		case strings.HasPrefix(a, "--"):
			name, value, inline := strings.Cut(a, "=")
			switch kind[name] {
			case "bool":
				f.vals[canon[name]] = append(f.vals[canon[name]], cmpOr(value, "true"))
			case "value":
				if !inline {
					if i+1 >= len(args) {
						return f, fmt.Errorf("flag needs an argument: %s", name)
					}
					i++
					value = args[i]
				}
				f.vals[canon[name]] = append(f.vals[canon[name]], value)
			default:
				return f, unhandled("the fake gh does not translate the flag %s of gh %s", name, command)
			}
		case strings.HasPrefix(a, "-") && len(a) > 1:
			for j := 1; j < len(a); j++ {
				short := "-" + string(a[j])
				switch kind[short] {
				case "bool":
					f.vals[canon[short]] = append(f.vals[canon[short]], "true")
				case "value":
					value := strings.TrimPrefix(a[j+1:], "=")
					if value == "" {
						if i+1 >= len(args) {
							return f, fmt.Errorf("flag needs an argument: %s", short)
						}
						i++
						value = args[i]
					}
					f.vals[canon[short]] = append(f.vals[canon[short]], value)
					j = len(a)
				default:
					return f, unhandled("the fake gh does not translate the flag %s of gh %s", short, command)
				}
			}
		default:
			f.pos = append(f.pos, a)
		}
	}
	return f, nil
}

// do sends one request to the server as this call's caller.
func (g *ghRun) do(method, path string, query url.Values, header http.Header, body []byte) *Response {
	if header == nil {
		header = http.Header{}
	}
	if header.Get("Accept") == "" {
		header.Set("Accept", "*/*")
	}
	header.Set("Authorization", "token "+Token)
	header.Set("User-Agent", "GitHub CLI (scenario)")
	return g.s.Do(g.ctx, &Request{Method: method, Path: path, Query: query, Header: header, Body: body, Caller: g.caller, Via: viaGH, Base: publicAPI})
}

// connectError is gh's words for a connection that failed.
func connectError() error {
	return errors.New("error connecting to api.github.com\ncheck your internet connection or https://githubstatus.com")
}

// httpError is gh's error for a REST answer that is no success.
func httpError(resp *Response, method, path string) error {
	var body struct {
		Message string `json:"message"`
		Errors  []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	_ = json.Unmarshal(resp.Body, &body)
	msg := fmt.Sprintf("HTTP %d: %s (%s%s)", resp.Status, cmpOr(body.Message, http.StatusText(resp.Status)), publicAPI, path)
	for _, e := range body.Errors {
		if e.Message != "" {
			msg += "\n" + e.Message
		}
	}
	_ = method
	return errors.New(msg)
}

// rest sends a REST request and decodes its JSON answer into v.
func (g *ghRun) rest(method, path string, query url.Values, in any, v any) error {
	var body []byte
	header := http.Header{}
	if in != nil {
		body, _ = json.Marshal(in)
		header.Set("Content-Type", "application/json; charset=utf-8")
	}
	header.Set("Accept", "application/vnd.github+json")
	resp := g.do(method, path, query, header, body)
	if resp.Outage {
		return connectError()
	}
	if resp.Status >= 300 {
		return httpError(resp, method, path)
	}
	if v == nil || len(resp.Body) == 0 {
		return nil
	}
	return json.Unmarshal(resp.Body, v)
}

// graphqlErr is gh's error for a GraphQL answer with errors:
// "GraphQL: <message> (<path>)".
func graphqlErr(errs []gqlResponseError) error {
	parts := make([]string, 0, len(errs))
	for _, e := range errs {
		msg := e.Message
		if len(e.Path) > 0 {
			path := make([]string, len(e.Path))
			for i, p := range e.Path {
				path[i] = fmt.Sprint(p)
			}
			msg += " (" + strings.Join(path, ".") + ")"
		}
		parts = append(parts, msg)
	}
	return errors.New("GraphQL: " + strings.Join(parts, ", "))
}

// graphql sends a GraphQL request and decodes its data into v.
func (g *ghRun) graphql(query string, vars map[string]any, v any) error {
	body, _ := json.Marshal(map[string]any{"query": query, "variables": vars})
	header := http.Header{}
	header.Set("Content-Type", "application/json; charset=utf-8")
	resp := g.do(http.MethodPost, "/graphql", nil, header, body)
	if resp.Outage {
		return connectError()
	}
	if resp.Status != http.StatusOK {
		return httpError(resp, http.MethodPost, "/graphql")
	}
	var doc struct {
		Data   json.RawMessage    `json:"data"`
		Errors []gqlResponseError `json:"errors"`
	}
	if err := json.Unmarshal(resp.Body, &doc); err != nil {
		return err
	}
	if len(doc.Errors) > 0 {
		return graphqlErr(doc.Errors)
	}
	if v == nil {
		return nil
	}
	return json.Unmarshal(doc.Data, v)
}

// repoArg parses [HOST/]OWNER/REPO or a repository's URL.
func repoArg(s string) (string, error) {
	s = strings.TrimSuffix(strings.TrimSuffix(s, "/"), ".git")
	for _, prefix := range []string{"https://github.com/", "http://github.com/", "git@github.com:", "github.com/"} {
		s = strings.TrimPrefix(s, prefix)
	}
	parts := strings.Split(s, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", fmt.Errorf(`expected the "[HOST/]OWNER/REPO" format, got %q`, s)
	}
	return parts[0] + "/" + parts[1], nil
}

// repo is the repository a command acts on: its --repo, GH_REPO, or the
// origin of the directory it was called in.
func (g *ghRun) repo(f flags) (string, error) {
	if r := f.str("repo"); r != "" {
		return repoArg(r)
	}
	if r := g.call.Env["GH_REPO"]; r != "" {
		return repoArg(r)
	}
	if r, ok := g.remoteRepo(); ok {
		return r, nil
	}
	return "", errors.New("failed to run git: fatal: not a git repository (or any of the parent directories): .git")
}

// remoteRepo reads the repository from the origin remote of the call's
// directory: a path that ends in <owner>/<name>.git.
func (g *ghRun) remoteRepo() (string, bool) {
	o := g.s.model.Origins
	if o == nil || g.call.Dir == "" {
		return "", false
	}
	c := exec.CommandContext(g.ctx, o.Git, "-C", g.call.Dir, "remote", "get-url", "origin") //nolint:gosec // real git on the caller's checkout
	c.Env = o.Env
	out, err := c.Output()
	if err != nil {
		return "", false
	}
	remote := strings.TrimSuffix(strings.TrimSpace(string(out)), ".git")
	remote = strings.TrimSuffix(remote, "/")
	name := filepath.Base(remote)
	owner := filepath.Base(filepath.Dir(remote))
	if name == "" || owner == "" || name == "." || owner == "." {
		return "", false
	}
	return owner + "/" + name, true
}

// currentBranch is the branch checked out in the call's directory.
func (g *ghRun) currentBranch() (string, bool) {
	o := g.s.model.Origins
	if o == nil || g.call.Dir == "" {
		return "", false
	}
	c := exec.CommandContext(g.ctx, o.Git, "-C", g.call.Dir, "symbolic-ref", "--quiet", "--short", "HEAD") //nolint:gosec // real git on the caller's checkout
	c.Env = o.Env
	out, err := c.Output()
	branch := strings.TrimSpace(string(out))
	return branch, err == nil && branch != ""
}

// readFile reads a file a flag names; "-" is standard input.
func (g *ghRun) readFile(name string) ([]byte, error) {
	if name == "-" {
		return []byte(g.call.Stdin), nil
	}
	if !filepath.IsAbs(name) {
		name = filepath.Join(g.call.Dir, name)
	}
	return os.ReadFile(name) //nolint:gosec // a file the scenario's own caller names
}

func unmarshal(b []byte, v any) error { return json.Unmarshal(b, v) }

// jqText is a scalar as gh's --jq prints it, and whether v is one.
func jqText(v any) (string, bool) {
	switch t := v.(type) {
	case nil:
		return "", true
	case string:
		return t, true
	case bool:
		return strconv.FormatBool(t), true
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64), true
	case int:
		return strconv.Itoa(t), true
	case json.Number:
		return t.String(), true
	}
	return "", false
}

// jq applies a jq expression to a JSON document and writes each result
// as gh does: a scalar as its text, anything else as compact JSON.
func jq(w *bytes.Buffer, input []byte, expr string) error {
	query, err := gojq.Parse(expr)
	if err != nil {
		return fmt.Errorf("failed to parse jq expression (line 1, column 1)\n    %s\n    ^  %w", expr, err)
	}
	code, err := gojq.Compile(query, gojq.WithEnvironLoader(func() []string { return nil }))
	if err != nil {
		return err
	}
	var doc any
	dec := json.NewDecoder(bytes.NewReader(input))
	dec.UseNumber()
	if err := dec.Decode(&doc); err != nil {
		return err
	}
	iter := code.Run(jqNumbers(doc))
	for {
		v, ok := iter.Next()
		if !ok {
			return nil
		}
		if err, isErr := v.(error); isErr {
			var halt *gojq.HaltError
			if errors.As(err, &halt) && halt.Value() == nil {
				return nil
			}
			return err
		}
		if text, scalar := jqText(v); scalar {
			fmt.Fprintln(w, text)
			continue
		}
		b, err := marshalNoEscape(v)
		if err != nil {
			return err
		}
		w.Write(b)
		w.WriteByte('\n')
	}
}

// jqNumbers turns a document's json.Numbers into the ints and floats
// gojq computes with.
func jqNumbers(v any) any {
	switch t := v.(type) {
	case json.Number:
		if n, err := t.Int64(); err == nil {
			return int(n)
		}
		f, _ := t.Float64()
		return f
	case []any:
		for i := range t {
			t[i] = jqNumbers(t[i])
		}
	case map[string]any:
		for k := range t {
			t[k] = jqNumbers(t[k])
		}
	}
	return v
}

// jsonFlags are the flags of a command with --json.
var jsonFlags = []string{"json", "jq|q", "template|t"}

// emit writes a command's --json answer: the data with exactly the asked
// fields, compact, or what --jq makes of it.
func (g *ghRun) emit(f flags, data any) error {
	if f.has("template") {
		return unhandled("the fake gh does not translate --template")
	}
	b, err := marshalNoEscape(data)
	if err != nil {
		return err
	}
	if expr := f.str("jq"); expr != "" {
		return jq(&g.out, b, expr)
	}
	g.out.Write(b)
	g.out.WriteByte('\n')
	return nil
}

// fields are a command's --json fields, checked against those it knows.
func (g *ghRun) fields(f flags, known []string) ([]string, error) {
	if f.has("jq") && !f.has("json") {
		return nil, errors.New("cannot use `--jq` without specifying `--json`")
	}
	asked := f.list("json")
	if f.has("json") && len(asked) == 0 {
		sorted := slices.Clone(known)
		slices.Sort(sorted)
		return nil, fmt.Errorf("Specify one or more comma-separated fields for `--json`:\n  %s", strings.Join(sorted, "\n  "))
	}
	for _, field := range asked {
		if !slices.Contains(known, field) {
			sorted := slices.Clone(known)
			slices.Sort(sorted)
			return nil, fmt.Errorf("Unknown JSON field: %q\nAvailable fields:\n  %s", field, strings.Join(sorted, "\n  "))
		}
	}
	return asked, nil
}

func (g *ghRun) authToken(args []string) error {
	if _, err := parseFlags("auth token", args, []string{"secure-storage"}, []string{"hostname|h", "user|u"}); err != nil {
		return err
	}
	fmt.Fprintln(&g.out, Token)
	return nil
}

func (g *ghRun) authStatus(args []string) error {
	if _, err := parseFlags("auth status", args, []string{"show-token|t", "active|a"}, []string{"hostname|h"}); err != nil {
		return err
	}
	fmt.Fprintf(&g.out, "github.com\n  ✓ Logged in to github.com account %s (GH_TOKEN)\n  - Active account: true\n  - Git operations protocol: https\n  - Token: ghs_************************************\n", g.s.model.Viewer)
	return nil
}
