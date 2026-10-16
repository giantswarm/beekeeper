package github

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
)

// readTries is how often a read is asked before its last error is returned;
// ReadBackoff is RunGH's wait before a read's first retry.
const (
	readTries   = 3
	ReadBackoff = 2 * time.Second
)

// serverError is a 5xx in gh's stderr: gh api's "HTTP 502: Bad Gateway",
// the GraphQL client's "non-200 OK status code: 504 Gateway Timeout".
var serverError = regexp.MustCompile(`(?:HTTP |status code: )(5\d\d)\b`)

// cutOff is how gh, or its decoder, says GitHub's body ended early or was
// empty.
var cutOff = []string{"unexpected end of JSON input", "unexpected EOF"}

// ghAPI and graphqlEndpoint are gh api's subcommand and GraphQL endpoint.
const (
	ghAPI           = "api"
	graphqlEndpoint = "graphql"
)

// shapers are the flags that print something else than GitHub's JSON: a
// filter or a template.
var shapers = map[string]bool{"-q": true, "--jq": true, "-t": true, "--template": true}

// errNoBody is the error of a read gh answered with exit 0 and an empty or
// cut-off body.
var errNoBody = errors.New("no complete JSON body")

// killed is how gh's exit says a signal killed it: exec.CommandContext's
// SIGKILL at its context's deadline, or the kernel's OOM killer.
const killed = "signal: killed"

// tryKey carries how long one try of a read may take.
type tryKey struct{}

// TryTimeout returns ctx with d as the time each try of a read under it
// may take (Retrying): a try past d is killed and asked again, where a
// deadline on ctx itself would kill the read and leave no time for the
// retry.
func TryTimeout(ctx context.Context, d time.Duration) context.Context {
	return context.WithValue(ctx, tryKey{}, d)
}

// timedOut is the error of a try killed at its TryTimeout.
type timedOut struct {
	d   time.Duration
	err error
}

func (t *timedOut) Error() string { return t.err.Error() }
func (t *timedOut) Unwrap() error { return t.err }

// tryRead is one try of a read, within ctx's TryTimeout when it has one.
func tryRead(ctx context.Context, gh GH, args []string) ([]byte, error) {
	d, _ := ctx.Value(tryKey{}).(time.Duration)
	if d <= 0 {
		return gh(ctx, args...)
	}
	tctx, cancel := context.WithTimeout(ctx, d)
	defer cancel()
	out, err := gh(tctx, args...)
	if err != nil && ctx.Err() == nil && errors.Is(tctx.Err(), context.DeadlineExceeded) {
		err = &timedOut{d: d, err: err}
	}
	return out, err
}

// Retrying is gh whose reads retry what GitHub answers transiently: a 5xx,
// an empty or a cut-off body, and a gh killed, at its TryTimeout or by the
// machine. Each read is asked at most readTries times,
// waiting backoff before the first retry and double that before the next;
// a read still failing returns an error naming the call, what went wrong
// and the tries. A write (a mutation, a
// REST call with a body or a non-GET method) is asked once: GitHub may have
// applied it before the answer broke.
func Retrying(gh GH, backoff time.Duration) GH {
	return func(ctx context.Context, args ...string) ([]byte, error) {
		if !isRead(args) {
			return gh(ctx, args...)
		}
		wait := backoff
		for try := 1; ; try++ {
			out, err := tryRead(ctx, gh, args)
			why := transient(args, out, err)
			if why == "" {
				return out, err
			}
			if try == readTries {
				return out, fmt.Errorf("%s: %s after %d tries: %w", callName(args), why, try, orNoBody(err))
			}
			t := time.NewTimer(wait)
			select {
			case <-ctx.Done():
				t.Stop()
				return out, fmt.Errorf("%s: %s, retry cancelled: %w", callName(args), why, orNoBody(err))
			case <-t.C:
			}
			wait *= 2
		}
	}
}

// orNoBody is err, errNoBody for none.
func orNoBody(err error) error {
	if err == nil {
		return errNoBody
	}
	return err
}

// transient says why a read's answer is worth asking again ("HTTP 502", "an
// empty body", "a cut-off body", "no answer within 10s", "a killed gh"), ""
// for an answer to return as it is.
func transient(args []string, out []byte, err error) string {
	if t := (*timedOut)(nil); errors.As(err, &t) {
		return "no answer within " + t.d.String()
	}
	if err != nil {
		msg := err.Error()
		if strings.Contains(msg, killed) {
			return "a killed gh"
		}
		if m := serverError.FindStringSubmatch(msg); m != nil {
			return "HTTP " + m[1]
		}
		for _, s := range cutOff {
			if strings.Contains(msg, s) {
				return "a cut-off body"
			}
		}
		return ""
	}
	if !answersJSON(args) {
		return ""
	}
	switch body := bytes.TrimSpace(out); {
	case len(body) == 0:
		return "an empty body"
	case !json.Valid(body):
		return "a cut-off body"
	}
	return ""
}

// readVerbs are the gh subcommands' second words that only read.
var readVerbs = map[string]bool{"view": true, "list": true, "status": true, "checks": true, "diff": true, "item-list": true, "field-list": true}

// isRead says whether gh args only read: gh search, a view or a list, a
// gh api GET, a GraphQL query.
func isRead(args []string) bool {
	if len(args) == 0 {
		return false
	}
	if args[0] != ghAPI {
		return args[0] == "search" || len(args) > 1 && readVerbs[args[1]]
	}
	a := parseAPI(args[1:])
	if a.endpoint == graphqlEndpoint {
		q, ok := a.fields["query"]
		q = strings.TrimSpace(q)
		return ok && (strings.HasPrefix(q, "query") || strings.HasPrefix(q, "{"))
	}
	if a.method != "" {
		return strings.EqualFold(a.method, "GET")
	}
	return !a.body
}

// answersJSON says whether gh args print GitHub's JSON as it is: gh api
// without a filter, a template, --silent, --include, --paginate or a
// non-JSON Accept, or a command's --json without a filter.
func answersJSON(args []string) bool {
	if len(args) == 0 {
		return false
	}
	if args[0] == ghAPI {
		a := parseAPI(args[1:])
		return !a.shaped
	}
	fields := false
	for _, x := range args {
		name, _, _ := strings.Cut(x, "=")
		if shapers[name] {
			return false
		}
		fields = fields || name == "--json"
	}
	return fields
}

// apiCall is what a gh api's arguments say.
type apiCall struct {
	endpoint string
	method   string
	// fields are the -f and -F values by name.
	fields map[string]string
	// body: the call sends fields or --input, which makes gh api a POST.
	body bool
	// shaped: the output is not GitHub's JSON body as it is.
	shaped bool
}

// apiValued are gh api's flags that take a value.
var apiValued = map[string]bool{
	"-X": true, "--method": true, "-H": true, "--header": true, "-f": true, "--raw-field": true, "-F": true, "--field": true,
	"-q": true, "--jq": true, "-t": true, "--template": true, "--input": true, "--hostname": true, "--cache": true, "-p": true, "--preview": true,
}

func parseAPI(args []string) apiCall {
	a := apiCall{fields: map[string]string{}}
	for i := 0; i < len(args); i++ {
		x := args[i]
		if !strings.HasPrefix(x, "-") {
			if a.endpoint == "" {
				a.endpoint = x
			}
			continue
		}
		name, value, inline := strings.Cut(x, "=")
		if !apiValued[name] {
			switch name {
			case "--silent", "-i", "--include", "--paginate":
				a.shaped = true
			}
			continue
		}
		if !inline {
			if i+1 == len(args) {
				break
			}
			i++
			value = args[i]
		}
		a.shaped = a.shaped || shapers[name]
		switch name {
		case "-X", "--method":
			a.method = value
		case "-f", "--raw-field", "-F", "--field":
			a.body = true
			k, v, _ := strings.Cut(value, "=")
			a.fields[k] = v
		case "--input":
			a.body = true
		case "-H", "--header":
			k, v, _ := strings.Cut(value, ":")
			if strings.EqualFold(strings.TrimSpace(k), "Accept") && !strings.Contains(v, "json") {
				a.shaped = true
			}
		}
	}
	return a
}

// queryHead is a GraphQL query's operation up to its first field: "query",
// its variables, "{", then the field's alias and name.
var queryHead = regexp.MustCompile(`^\s*(?:query\b[^{]*)?\{\s*([\w:]+)`)

// callName names a gh call for its error: "gh api graphql (repositoryOwner)"
// by the query's first field, else gh and its first three arguments.
func callName(args []string) string {
	if len(args) > 1 && args[0] == ghAPI {
		if a := parseAPI(args[1:]); a.endpoint == graphqlEndpoint {
			if m := queryHead.FindStringSubmatch(a.fields["query"]); m != nil {
				return "gh api graphql (" + m[1] + ")"
			}
		}
	}
	return "gh " + strings.Join(args[:min(len(args), 3)], " ")
}
