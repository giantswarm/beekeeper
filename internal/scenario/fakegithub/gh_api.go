//go:build scenario

package fakegithub

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// gh api, as the CLI's pkg/cmd/api does it: fields into a query or a JSON
// body, GraphQL variables, placeholders, pagination by the Link header or
// endCursor, --include, --jq, and gh's words and exit code for an answer
// that is no success.

var (
	apiPlaceholderRE = regexp.MustCompile(`(:(owner|repo|branch)\b|\{[a-z]+\})`)
	apiLinkRE        = regexp.MustCompile(`<([^>]+)>;\s*rel="([^"]+)"`)
	apiJSONTypeRE    = regexp.MustCompile(`[/+]json(;|$)`)
)

// apiOpts are gh api's options.
type apiOpts struct {
	path         string
	method       string
	methodPassed bool
	headers      []string
	include      bool
	paginate     bool
	slurp        bool
	silent       bool
	filter       string
	input        string
}

func (g *ghRun) api(args []string) error {
	f, err := parseFlags("api", args,
		[]string{"include|i", "paginate", "silent", "slurp", "allow-escape-sequences"},
		[]string{"method|X", "header|H", "raw-field|f", "field|F", "input", "jq|q", "template|t", "hostname", "cache", "preview|p"})
	if err != nil {
		return err
	}
	if len(f.pos) != 1 {
		return fmt.Errorf("accepts 1 arg(s), received %d", len(f.pos))
	}
	if f.has("template") {
		return unhandled("the fake gh does not translate gh api --template")
	}
	if f.has("preview") {
		return unhandled("the fake gh does not translate gh api --preview")
	}
	if h := f.str("hostname"); h != "" && h != "github.com" {
		return unhandled("the fake gh serves github.com, not --hostname %s", h)
	}
	o := apiOpts{path: f.pos[0], method: cmpOr(f.str("method"), "GET"), methodPassed: f.has("method"), headers: f.all("header"),
		include: f.on("include"), paginate: f.on("paginate"), slurp: f.on("slurp"), silent: f.on("silent"), filter: f.str("jq"), input: f.str("input")}
	graphQL := o.path == "graphql"
	switch {
	case o.paginate && !strings.EqualFold(o.method, "GET") && !graphQL:
		return errors.New("the `--paginate` option is not supported for non-GET requests")
	case o.paginate && o.input != "":
		return errors.New("the `--paginate` option is not supported with `--input`")
	case o.slurp && !o.paginate:
		return errors.New("`--paginate` required when passing `--slurp`")
	case o.slurp && o.filter != "":
		return errors.New("the `--slurp` option is not supported with `--jq` or `--template`")
	case o.silent && o.filter != "":
		return errors.New("only one of `--template`, `--jq`, `--silent`, or `--verbose` may be used")
	}
	params, err := g.apiFields(f.all("raw-field"), f.all("field"))
	if err != nil {
		return err
	}
	path, err := g.apiPlaceholders(o.path)
	if err != nil {
		return fmt.Errorf("unable to expand placeholder in path: %w", err)
	}
	if !o.methodPassed && (len(params) > 0 || o.input != "") {
		o.method = "POST"
	}
	if o.paginate && !graphQL {
		path = apiAddPerPage(path, 100, params)
	}
	var body []byte
	raw := false
	if o.input != "" {
		if body, err = g.readFile(o.input); err != nil {
			return err
		}
		path = apiAddQuery(path, params)
		raw = true
	}
	if o.paginate && o.slurp {
		defer func() { g.out.WriteString("]") }()
	}
	first, more := true, true
	for more {
		resp, err := g.apiRequest(o, path, params, body, raw, graphQL)
		if err != nil {
			return err
		}
		if !graphQL {
			path, more = apiNextPage(resp)
			params, body, raw = nil, nil, false
		}
		if o.paginate && o.slurp {
			if first {
				g.out.WriteString("[")
			} else {
				g.out.WriteString(",")
			}
		}
		cursor, err := g.apiResponse(o, resp, graphQL, first, !more)
		if err != nil {
			return err
		}
		first = false
		if !o.paginate {
			break
		}
		if graphQL {
			more = cursor != ""
			if more {
				params["endCursor"] = cursor
			}
		}
		if more && o.include {
			g.out.WriteString("\n")
		}
	}
	return nil
}

// apiRequest sends one request of gh api.
func (g *ghRun) apiRequest(o apiOpts, path string, params map[string]any, body []byte, raw, graphQL bool) (*Response, error) {
	for _, prefix := range []string{publicAPI + "/", "https://uploads.github.com/"} {
		if rest, ok := strings.CutPrefix(path, prefix); ok {
			path = rest
		}
	}
	if strings.Contains(path, "://") {
		return nil, unhandled("the fake gh reaches api.github.com only, not %s", path)
	}
	path = "/" + strings.TrimPrefix(path, "/")
	method := strings.ToUpper(o.method)
	header := http.Header{}
	jsonBody := false
	switch {
	case raw:
	case len(params) == 0:
	case method == "GET":
		path = apiAddQuery(path, params)
	default:
		send := params
		if graphQL {
			send = apiGraphQLVariables(params)
		}
		b, err := json.Marshal(send)
		if err != nil {
			return nil, fmt.Errorf("error serializing parameters: %w", err)
		}
		body, jsonBody = b, true
	}
	for _, h := range o.headers {
		name, value, ok := strings.Cut(h, ":")
		if !ok {
			return nil, fmt.Errorf("header %q requires a value separated by ':'", h)
		}
		if !strings.EqualFold(name, "Content-Length") {
			header.Add(name, strings.TrimSpace(value))
		}
	}
	if jsonBody && header.Get("Content-Type") == "" {
		header.Set("Content-Type", "application/json; charset=utf-8")
	}
	rawPath, rawQuery, _ := strings.Cut(path, "?")
	query, err := url.ParseQuery(rawQuery)
	if err != nil {
		return nil, fmt.Errorf("invalid query in %q: %w", path, err)
	}
	resp := g.do(method, rawPath, query, header, body)
	if resp.Outage {
		return nil, connectError()
	}
	return resp, nil
}

// apiResponse writes one answer of gh api and returns a GraphQL page's
// end cursor.
func (g *ghRun) apiResponse(o apiOpts, resp *Response, graphQL, first, last bool) (string, error) {
	if o.include {
		fmt.Fprintln(&g.out, "HTTP/2.0", strconv.Itoa(resp.Status)+" "+http.StatusText(resp.Status))
		names := make([]string, 0, len(resp.Header))
		for name := range resp.Header {
			if name != "Status" {
				names = append(names, name)
			}
		}
		sort.Strings(names)
		for _, name := range names {
			fmt.Fprintf(&g.out, "%s: %s\r\n", name, strings.Join(resp.Header[name], ", "))
		}
		g.out.WriteString("\r\n")
	}
	if resp.Status == http.StatusNoContent {
		return "", nil
	}
	body := resp.Body
	isJSON := apiJSONTypeRE.MatchString(resp.Header.Get("Content-Type"))
	serverError := ""
	if isJSON && (graphQL || resp.Status >= 400) && !strings.EqualFold(o.method, "HEAD") {
		var err error
		if serverError, err = apiErrorMessage(body, resp.Status); err != nil {
			return "", err
		}
	}
	graphQLPage := isJSON && resp.Status == http.StatusOK && o.paginate && graphQL
	switch {
	case o.silent:
	case o.filter != "" && serverError == "":
		if err := jq(&g.out, body, o.filter); err != nil {
			return "", err
		}
	default:
		if isJSON && o.paginate && !o.slurp && !graphQLPage && !o.include {
			body = apiMergePage(body, first, last)
		}
		g.out.Write(body)
	}
	if serverError == "" && resp.Status > 299 {
		serverError = fmt.Sprintf("HTTP %d", resp.Status)
	}
	if serverError != "" {
		fmt.Fprintf(&g.err, "gh: %s\n", serverError)
		return "", errSilent
	}
	if graphQLPage {
		return apiEndCursor(body), nil
	}
	return "", nil
}

// apiMergePage drops a page's brackets so the pages of an array read as
// one array.
func apiMergePage(body []byte, first, last bool) []byte {
	b := bytes.Clone(body)
	if !first && len(b) > 0 && b[0] == '[' {
		if len(b) > 1 && b[1] == ']' {
			b[0] = ' '
		} else {
			b[0] = ','
		}
	}
	if !last && len(b) > 0 && b[len(b)-1] == ']' {
		b = b[:len(b)-1]
	}
	return b
}

// apiErrorMessage is what gh reads of an answer as its error.
func apiErrorMessage(body []byte, status int) (string, error) {
	var parsed struct {
		Message string
		Errors  json.RawMessage
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return "", err
	}
	if len(parsed.Errors) > 0 && parsed.Errors[0] == '"' {
		var s string
		if err := json.Unmarshal(parsed.Errors, &s); err != nil {
			return "", err
		}
		if s != "" {
			if parsed.Message != "" {
				return fmt.Sprintf("%s (%s)", s, parsed.Message), nil
			}
			return s, nil
		}
	}
	if parsed.Message != "" {
		return fmt.Sprintf("%s (HTTP %d)", parsed.Message, status), nil
	}
	if len(parsed.Errors) == 0 || parsed.Errors[0] != '[' {
		return "", nil
	}
	var raws []json.RawMessage
	if err := json.Unmarshal(parsed.Errors, &raws); err != nil {
		return "", err
	}
	var msgs []string
	for _, raw := range raws {
		if len(raw) == 0 {
			continue
		}
		switch raw[0] {
		case '{':
			var e struct{ Message string }
			if err := json.Unmarshal(raw, &e); err != nil {
				return "", err
			}
			msgs = append(msgs, e.Message)
		case '"':
			var s string
			if err := json.Unmarshal(raw, &s); err != nil {
				return "", err
			}
			msgs = append(msgs, s)
		}
	}
	return strings.Join(msgs, "\n"), nil
}

func apiNextPage(resp *Response) (string, bool) {
	for _, m := range apiLinkRE.FindAllStringSubmatch(resp.Header.Get("Link"), -1) {
		if len(m) > 2 && m[2] == "next" {
			return m[1], true
		}
	}
	return "", false
}

// apiEndCursor reads a GraphQL page's first pageInfo as gh does, token by
// token: its endCursor when it has a next page.
func apiEndCursor(body []byte) string {
	dec := json.NewDecoder(bytes.NewReader(body))
	var idx int
	var stack []json.Delim
	var lastKey, contextKey, endCursor string
	var hasNextPage, foundEndCursor, foundNextPage bool
loop:
	for {
		t, err := dec.Token()
		if err != nil {
			break
		}
		switch tt := t.(type) {
		case json.Delim:
			switch tt {
			case '{', '[':
				stack = append(stack, tt)
				contextKey = lastKey
				idx = 0
			case '}', ']':
				stack = stack[:len(stack)-1]
				contextKey = ""
				idx = 0
			}
		default:
			isKey := len(stack) > 0 && stack[len(stack)-1] == '{' && idx%2 == 0
			idx++
			switch tt := t.(type) {
			case string:
				if isKey {
					lastKey = tt
				} else if contextKey == "pageInfo" && lastKey == "endCursor" {
					endCursor, foundEndCursor = tt, true
					if foundNextPage {
						break loop
					}
				}
			case bool:
				if contextKey == "pageInfo" && lastKey == "hasNextPage" {
					hasNextPage, foundNextPage = tt, true
					if foundEndCursor {
						break loop
					}
				}
			}
		}
	}
	if hasNextPage {
		return endCursor
	}
	return ""
}

func apiAddPerPage(path string, perPage int, params map[string]any) string {
	if _, has := params["per_page"]; has {
		return path
	}
	sep := "?"
	if i := strings.IndexRune(path, '?'); i >= 0 {
		if q, err := url.ParseQuery(path[i+1:]); err == nil && q.Get("per_page") != "" {
			return path
		}
		sep = "&"
	}
	return fmt.Sprintf("%s%sper_page=%d", path, sep, perPage)
}

func apiAddQuery(path string, params map[string]any) string {
	if len(params) == 0 {
		return path
	}
	query := url.Values{}
	apiAddQueryParam(query, "", params)
	sep := "?"
	if strings.ContainsRune(path, '?') {
		sep = "&"
	}
	return path + sep + query.Encode()
}

func apiAddQueryParam(query url.Values, key string, value any) {
	switch v := value.(type) {
	case string:
		query.Add(key, v)
	case nil:
		query.Add(key, "")
	case int:
		query.Add(key, strconv.Itoa(v))
	case bool:
		query.Add(key, strconv.FormatBool(v))
	case map[string]any:
		for sub, val := range v {
			apiAddQueryParam(query, sub, val)
		}
	case []any:
		for _, entry := range v {
			apiAddQueryParam(query, key+"[]", entry)
		}
	}
}

// apiGraphQLVariables groups every field but query and operationName
// under variables.
func apiGraphQLVariables(params map[string]any) map[string]any {
	top, vars := map[string]any{}, map[string]any{}
	for k, v := range params {
		if k == "query" || k == "operationName" {
			top[k] = v
		} else {
			vars[k] = v
		}
	}
	if len(vars) > 0 {
		top["variables"] = vars
	}
	return top
}

// apiPlaceholders fills {owner}, {repo} and {branch}.
func (g *ghRun) apiPlaceholders(value string) (string, error) {
	var err error
	out := apiPlaceholderRE.ReplaceAllStringFunc(value, func(m string) string {
		name := strings.Trim(m, ":{}")
		switch name {
		case "owner", "repo":
			repo, e := g.repo(flags{})
			if e != nil {
				err = e
				return m
			}
			owner, repoName, _ := strings.Cut(repo, "/")
			if name == "owner" {
				return owner
			}
			return repoName
		case "branch":
			if g.call.Env["GH_REPO"] != "" {
				err = errors.New("unable to determine an appropriate value for the 'branch' placeholder")
				return m
			}
			branch, ok := g.currentBranch()
			if !ok {
				err = errors.New("git: not on any branch")
				return m
			}
			return branch
		}
		return m
	})
	return out, err
}

// apiFields builds gh api's parameters from its -f and -F fields: raw
// strings, and magic values (numbers, true, false, null, @file), with
// a[b]= and a[]= for nested objects and arrays.
func (g *ghRun) apiFields(rawFields, magicFields []string) (map[string]any, error) {
	params := map[string]any{}
	parse := func(field string, magic bool) error {
		valueIndex, keyStartAt := 0, 0
		var keystack []string
	loop:
		for i, r := range field {
			switch r {
			case '[':
				if keyStartAt == 0 {
					keystack = append(keystack, field[0:i])
				}
				keyStartAt = i + 1
			case ']':
				keystack = append(keystack, field[keyStartAt:i])
			case '=':
				if keyStartAt == 0 {
					keystack = append(keystack, field[0:i])
				}
				valueIndex = i + 1
				break loop
			}
		}
		if len(keystack) == 0 {
			return fmt.Errorf("invalid key: %q", field)
		}
		fieldKey := field
		var value any
		if valueIndex == 0 {
			if keystack[len(keystack)-1] != "" {
				return fmt.Errorf("field %q requires a value separated by an '=' sign", fieldKey)
			}
		} else {
			fieldKey = field[0 : valueIndex-1]
			value = field[valueIndex:]
		}
		if magic && value != nil {
			v, err := g.apiMagicValue(value.(string))
			if err != nil {
				return fmt.Errorf("error parsing %q value: %w", fieldKey, err)
			}
			value = v
		}
		dest := params
		isArray := false
		var subkey string
		for _, k := range keystack {
			if k == "" {
				isArray = true
				continue
			}
			if subkey != "" {
				var err error
				if isArray {
					dest, err = apiParamsSlice(dest, subkey, k)
					isArray = false
				} else {
					dest, err = apiParamsMap(dest, subkey)
				}
				if err != nil {
					return err
				}
			}
			subkey = k
		}
		if isArray {
			if value == nil {
				dest[subkey] = []any{}
				return nil
			}
			if old, exists := dest[subkey]; exists {
				list, ok := old.([]any)
				if !ok {
					return fmt.Errorf("expected array type under %q, got %T", subkey, old)
				}
				dest[subkey] = append(list, value)
				return nil
			}
			dest[subkey] = []any{value}
			return nil
		}
		if _, exists := dest[subkey]; exists {
			return fmt.Errorf("unexpected override existing field under %q", subkey)
		}
		dest[subkey] = value
		return nil
	}
	for _, f := range rawFields {
		if err := parse(f, false); err != nil {
			return params, err
		}
	}
	for _, f := range magicFields {
		if err := parse(f, true); err != nil {
			return params, err
		}
	}
	return params, nil
}

func apiParamsMap(m map[string]any, key string) (map[string]any, error) {
	if v, exists := m[key]; exists {
		if existing, ok := v.(map[string]any); ok {
			return existing, nil
		}
		return nil, fmt.Errorf("expected map type under %q, got %T", key, v)
	}
	created := map[string]any{}
	m[key] = created
	return created, nil
}

func apiParamsSlice(m map[string]any, prevKey, newKey string) (map[string]any, error) {
	if v, exists := m[prevKey]; exists {
		list, ok := v.([]any)
		if !ok {
			return nil, fmt.Errorf("expected array type under %q, got %T", prevKey, v)
		}
		if len(list) > 0 {
			if last, ok := list[len(list)-1].(map[string]any); ok {
				if _, has := last[newKey]; !has {
					return last, nil
				} else if reflect.TypeOf(last[newKey]).Kind() == reflect.Slice {
					return last, nil
				}
			}
		}
		created := map[string]any{}
		m[prevKey] = append(list, created)
		return created, nil
	}
	created := map[string]any{}
	m[prevKey] = []any{created}
	return created, nil
}

func (g *ghRun) apiMagicValue(v string) (any, error) {
	if strings.HasPrefix(v, "@") {
		b, err := g.readFile(v[1:])
		if err != nil {
			return "", err
		}
		return string(b), nil
	}
	if n, err := strconv.Atoi(v); err == nil {
		return n, nil
	}
	switch v {
	case "true":
		return true, nil
	case "false":
		return false, nil
	case "null":
		return nil, nil
	}
	return g.apiPlaceholders(v)
}
