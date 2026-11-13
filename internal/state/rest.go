package state

import (
	"bytes"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"sync"
	"unicode/utf8"
)

// rest are the members of one JSON object its Go type does not know: a newer
// beekeeper wrote them, and an older one still running (a start's reopen, a
// watch, a gated merge) writes them back unchanged instead of dropping the
// newer one's state. Every object of the document that carries per-entry
// data keeps its own, so an entry's fields travel with it when entries are
// added, removed or reordered.
type rest map[string]json.RawMessage

// decodeKeeping decodes raw into v, a type without JSON methods, and keeps
// the members its fields do not take in r.
func decodeKeeping[P any](raw []byte, v *P, r *rest) error {
	if err := json.Unmarshal(raw, v); err != nil {
		return err
	}
	all, err := unknownMembers(raw, jsonKeys(reflect.TypeFor[P]()))
	if err != nil {
		return err
	}
	*r = all
	return nil
}

var errMemberSyntax = errors.New("invalid JSON object")

// unknownMembers are the members of the object raw whose names known lacks,
// as decoding raw into a map of raw messages keeps them: the last of a
// repeated name, each value's bytes as written; nil for none, or for null.
// encoding/json matches names case-insensitively, and known holds lower-cased
// names. raw was decoded already, so it is valid JSON. Only the unknown
// members are copied: the known ones, nearly the whole document, are neither
// copied nor decoded a second time, at any depth of the state's objects.
func unknownMembers(raw []byte, known map[string]bool) (rest, error) {
	i := skipSpace(raw, 0)
	if bytes.HasPrefix(raw[i:], []byte("null")) {
		return nil, nil
	}
	if i >= len(raw) || raw[i] != '{' {
		return nil, errMemberSyntax
	}
	var r rest
	for i = skipSpace(raw, i+1); i < len(raw) && raw[i] != '}'; {
		end, err := valueEnd(raw, i)
		if err != nil || raw[i] != '"' {
			return nil, errMemberSyntax
		}
		name, err := memberName(raw[i:end])
		if err != nil {
			return nil, err
		}
		if i = skipSpace(raw, end); i >= len(raw) || raw[i] != ':' {
			return nil, errMemberSyntax
		}
		start := skipSpace(raw, i+1)
		if end, err = valueEnd(raw, start); err != nil {
			return nil, err
		}
		if !known[strings.ToLower(name)] {
			if r == nil {
				r = rest{}
			}
			r[name] = json.RawMessage(bytes.Clone(raw[start:end]))
		}
		if i = skipSpace(raw, end); i < len(raw) && raw[i] == ',' {
			i = skipSpace(raw, i+1)
		} else if i >= len(raw) || raw[i] != '}' {
			return nil, errMemberSyntax
		}
	}
	if i >= len(raw) {
		return nil, errMemberSyntax
	}
	return r, nil
}

// memberName is the quoted JSON string q decoded.
func memberName(q []byte) (string, error) {
	if s := q[1 : len(q)-1]; bytes.IndexByte(s, '\\') < 0 && utf8.Valid(s) {
		return string(s), nil
	}
	var name string
	err := json.Unmarshal(q, &name)
	return name, err
}

func skipSpace(raw []byte, i int) int {
	for i < len(raw) && (raw[i] == ' ' || raw[i] == '\t' || raw[i] == '\n' || raw[i] == '\r') {
		i++
	}
	return i
}

// valueEnd is the index just past the JSON value starting at raw[i].
func valueEnd(raw []byte, i int) (int, error) {
	if i >= len(raw) {
		return 0, errMemberSyntax
	}
	depth := 0
	for j := i; j < len(raw); j++ {
		switch raw[j] {
		case '"':
			for j++; j < len(raw) && raw[j] != '"'; j++ {
				if raw[j] == '\\' {
					j++
				}
			}
			if j >= len(raw) {
				return 0, errMemberSyntax
			}
			if depth == 0 {
				return j + 1, nil
			}
		case '{', '[':
			depth++
		case '}', ']':
			if depth == 0 {
				return j, nil
			}
			if depth--; depth == 0 {
				return j + 1, nil
			}
		case ',', ' ', '\t', '\n', '\r':
			if depth == 0 {
				return j, nil
			}
		}
	}
	if depth != 0 {
		return 0, errMemberSyntax
	}
	return len(raw), nil
}

// encodeKeeping encodes v, a type without JSON methods, with the members r
// kept.
func encodeKeeping[P any](v P, r rest) ([]byte, error) {
	raw, err := json.Marshal(v)
	if err != nil || len(r) == 0 {
		return raw, err
	}
	var all map[string]json.RawMessage
	if err := json.Unmarshal(raw, &all); err != nil {
		return nil, err
	}
	for k, m := range r {
		if _, ok := all[k]; !ok {
			all[k] = m
		}
	}
	return json.Marshal(all)
}

var keyCache sync.Map // reflect.Type → map[string]bool

// jsonKeys are the lower-cased JSON names of t's fields, an embedded
// struct's included.
func jsonKeys(t reflect.Type) map[string]bool {
	if k, ok := keyCache.Load(t); ok {
		return k.(map[string]bool)
	}
	out := map[string]bool{}
	for i := range t.NumField() {
		f := t.Field(i)
		name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
		switch {
		case name == "-":
		case f.Anonymous && name == "" && f.Type.Kind() == reflect.Struct:
			for k := range jsonKeys(f.Type) {
				out[k] = true
			}
		case !f.IsExported():
		case name != "":
			out[strings.ToLower(name)] = true
		default:
			out[strings.ToLower(f.Name)] = true
		}
	}
	keyCache.Store(t, out)
	return out
}

// The plain types are the document's types without their JSON methods.
// Party has no rest of its own: its fields are flattened into the types that
// embed it, whose rest keeps them.
type (
	plainState             State
	plainSupervisor        Supervisor
	plainRelay             Relay
	plainRelief            Relief
	plainCLI               CLI
	plainRelayDue          RelayDue
	plainGuideWork         GuideWork
	plainRole              Role
	plainGrant             Grant
	plainHold              Hold
	plainKeep              Keep
	plainPark              Park
	plainAgent             Agent
	plainImportWait        ImportWait
	plainNote              Note
	plainTimer             Timer
	plainOverride          Override
	plainOverrideKey       OverrideKey
	plainRecord            Record
	plainClaim             Claim
	plainAlertOwner        AlertOwner
	plainStart             Start
	plainApp               App
	plainAppRun            AppRun
	plainAppCreated        AppCreated
	plainArchive           Archive
	plainDecline           Decline
	plainReport            Report
	plainReportPause       ReportPause
	plainReportThread      ReportThread
	plainBudget            Budget
	plainBranchRelease     BranchRelease
	plainGraphQL           GraphQL
	plainMerge             Merge
	plainFailed            Failed
	plainOwed              Owed
	plainWriter            Writer
	plainWorkerReport      WorkerReport
	plainUrgent            Urgent
	plainRoleLine          RoleLine
	plainPair              Pair
	plainStop              Stop
	plainHostPlacement     HostPlacement
	plainHostPin           HostPin
	plainLocalTask         LocalTask
	plainTranscriptGrant   TranscriptGrant
	plainTranscriptRequest TranscriptRequest
)

func (v *State) UnmarshalJSON(b []byte) error {
	if err := decodeKeeping(b, (*plainState)(v), &v.rest); err != nil {
		return err
	}
	for _, k := range retiredStateKeys {
		delete(v.rest, k)
	}
	if len(v.rest) == 0 {
		v.rest = nil
	}
	return nil
}

// retiredStateKeys are the members of the supervisor session's role that
// older binaries wrote: the supervisor is the supervisor loop, so a state
// read drops them and the next write leaves them out.
var retiredStateKeys = []string{"supervisor", "relay", "relieved", "supervisorCLI", "supervisorRun", "relayDue"}

func (v State) MarshalJSON() ([]byte, error) { return encodeKeeping(plainState(v), v.rest) }
func (v *Supervisor) UnmarshalJSON(b []byte) error {
	return decodeKeeping(b, (*plainSupervisor)(v), &v.rest)
}
func (v Supervisor) MarshalJSON() ([]byte, error) { return encodeKeeping(plainSupervisor(v), v.rest) }
func (v *Relay) UnmarshalJSON(b []byte) error     { return decodeKeeping(b, (*plainRelay)(v), &v.rest) }
func (v Relay) MarshalJSON() ([]byte, error)      { return encodeKeeping(plainRelay(v), v.rest) }
func (v *Relief) UnmarshalJSON(b []byte) error    { return decodeKeeping(b, (*plainRelief)(v), &v.rest) }
func (v Relief) MarshalJSON() ([]byte, error)     { return encodeKeeping(plainRelief(v), v.rest) }
func (v *CLI) UnmarshalJSON(b []byte) error       { return decodeKeeping(b, (*plainCLI)(v), &v.rest) }
func (v CLI) MarshalJSON() ([]byte, error)        { return encodeKeeping(plainCLI(v), v.rest) }
func (v *RelayDue) UnmarshalJSON(b []byte) error {
	return decodeKeeping(b, (*plainRelayDue)(v), &v.rest)
}
func (v RelayDue) MarshalJSON() ([]byte, error) { return encodeKeeping(plainRelayDue(v), v.rest) }
func (v *Role) UnmarshalJSON(b []byte) error    { return decodeKeeping(b, (*plainRole)(v), &v.rest) }
func (v Role) MarshalJSON() ([]byte, error)     { return encodeKeeping(plainRole(v), v.rest) }
func (v *Grant) UnmarshalJSON(b []byte) error   { return decodeKeeping(b, (*plainGrant)(v), &v.rest) }
func (v Grant) MarshalJSON() ([]byte, error)    { return encodeKeeping(plainGrant(v), v.rest) }
func (v *Hold) UnmarshalJSON(b []byte) error    { return decodeKeeping(b, (*plainHold)(v), &v.rest) }
func (v Hold) MarshalJSON() ([]byte, error)     { return encodeKeeping(plainHold(v), v.rest) }
func (v *Keep) UnmarshalJSON(b []byte) error    { return decodeKeeping(b, (*plainKeep)(v), &v.rest) }
func (v Keep) MarshalJSON() ([]byte, error)     { return encodeKeeping(plainKeep(v), v.rest) }
func (v *Park) UnmarshalJSON(b []byte) error    { return decodeKeeping(b, (*plainPark)(v), &v.rest) }
func (v Park) MarshalJSON() ([]byte, error)     { return encodeKeeping(plainPark(v), v.rest) }
func (v *Agent) UnmarshalJSON(b []byte) error   { return decodeKeeping(b, (*plainAgent)(v), &v.rest) }
func (v Agent) MarshalJSON() ([]byte, error)    { return encodeKeeping(plainAgent(v), v.rest) }
func (v *ImportWait) UnmarshalJSON(b []byte) error {
	return decodeKeeping(b, (*plainImportWait)(v), &v.rest)
}
func (v ImportWait) MarshalJSON() ([]byte, error) { return encodeKeeping(plainImportWait(v), v.rest) }
func (v *Note) UnmarshalJSON(b []byte) error      { return decodeKeeping(b, (*plainNote)(v), &v.rest) }
func (v Note) MarshalJSON() ([]byte, error)       { return encodeKeeping(plainNote(v), v.rest) }
func (v *Timer) UnmarshalJSON(b []byte) error     { return decodeKeeping(b, (*plainTimer)(v), &v.rest) }
func (v Timer) MarshalJSON() ([]byte, error)      { return encodeKeeping(plainTimer(v), v.rest) }
func (v *Override) UnmarshalJSON(b []byte) error {
	return decodeKeeping(b, (*plainOverride)(v), &v.rest)
}
func (v Override) MarshalJSON() ([]byte, error) { return encodeKeeping(plainOverride(v), v.rest) }
func (v *OverrideKey) UnmarshalJSON(b []byte) error {
	return decodeKeeping(b, (*plainOverrideKey)(v), &v.rest)
}
func (v OverrideKey) MarshalJSON() ([]byte, error) { return encodeKeeping(plainOverrideKey(v), v.rest) }
func (v *Record) UnmarshalJSON(b []byte) error     { return decodeKeeping(b, (*plainRecord)(v), &v.rest) }
func (v Record) MarshalJSON() ([]byte, error)      { return encodeKeeping(plainRecord(v), v.rest) }
func (v *Claim) UnmarshalJSON(b []byte) error      { return decodeKeeping(b, (*plainClaim)(v), &v.rest) }
func (v Claim) MarshalJSON() ([]byte, error)       { return encodeKeeping(plainClaim(v), v.rest) }
func (v *AlertOwner) UnmarshalJSON(b []byte) error {
	return decodeKeeping(b, (*plainAlertOwner)(v), &v.rest)
}
func (v AlertOwner) MarshalJSON() ([]byte, error) { return encodeKeeping(plainAlertOwner(v), v.rest) }
func (v *Start) UnmarshalJSON(b []byte) error     { return decodeKeeping(b, (*plainStart)(v), &v.rest) }
func (v Start) MarshalJSON() ([]byte, error)      { return encodeKeeping(plainStart(v), v.rest) }

type plainExecutionRecord ExecutionRecord

type plainExecutionMessage ExecutionMessage

func (v *ExecutionMessage) UnmarshalJSON(b []byte) error {
	return decodeKeeping(b, (*plainExecutionMessage)(v), &v.rest)
}
func (v ExecutionMessage) MarshalJSON() ([]byte, error) {
	return encodeKeeping(plainExecutionMessage(v), v.rest)
}

func (v *ExecutionRecord) UnmarshalJSON(b []byte) error {
	return decodeKeeping(b, (*plainExecutionRecord)(v), &v.rest)
}
func (v ExecutionRecord) MarshalJSON() ([]byte, error) {
	return encodeKeeping(plainExecutionRecord(v), v.rest)
}
func (v *App) UnmarshalJSON(b []byte) error    { return decodeKeeping(b, (*plainApp)(v), &v.rest) }
func (v App) MarshalJSON() ([]byte, error)     { return encodeKeeping(plainApp(v), v.rest) }
func (v *AppRun) UnmarshalJSON(b []byte) error { return decodeKeeping(b, (*plainAppRun)(v), &v.rest) }
func (v AppRun) MarshalJSON() ([]byte, error)  { return encodeKeeping(plainAppRun(v), v.rest) }
func (v *AppCreated) UnmarshalJSON(b []byte) error {
	return decodeKeeping(b, (*plainAppCreated)(v), &v.rest)
}
func (v AppCreated) MarshalJSON() ([]byte, error) { return encodeKeeping(plainAppCreated(v), v.rest) }
func (v *Archive) UnmarshalJSON(b []byte) error   { return decodeKeeping(b, (*plainArchive)(v), &v.rest) }
func (v Archive) MarshalJSON() ([]byte, error)    { return encodeKeeping(plainArchive(v), v.rest) }
func (v *Decline) UnmarshalJSON(b []byte) error   { return decodeKeeping(b, (*plainDecline)(v), &v.rest) }
func (v Decline) MarshalJSON() ([]byte, error)    { return encodeKeeping(plainDecline(v), v.rest) }
func (v *Report) UnmarshalJSON(b []byte) error    { return decodeKeeping(b, (*plainReport)(v), &v.rest) }
func (v Report) MarshalJSON() ([]byte, error)     { return encodeKeeping(plainReport(v), v.rest) }
func (v *ReportThread) UnmarshalJSON(b []byte) error {
	return decodeKeeping(b, (*plainReportThread)(v), &v.rest)
}
func (v ReportThread) MarshalJSON() ([]byte, error) {
	return encodeKeeping(plainReportThread(v), v.rest)
}
func (v *ReportPause) UnmarshalJSON(b []byte) error {
	return decodeKeeping(b, (*plainReportPause)(v), &v.rest)
}
func (v ReportPause) MarshalJSON() ([]byte, error) { return encodeKeeping(plainReportPause(v), v.rest) }
func (v *Budget) UnmarshalJSON(b []byte) error     { return decodeKeeping(b, (*plainBudget)(v), &v.rest) }
func (v Budget) MarshalJSON() ([]byte, error)      { return encodeKeeping(plainBudget(v), v.rest) }
func (v *BranchRelease) UnmarshalJSON(b []byte) error {
	return decodeKeeping(b, (*plainBranchRelease)(v), &v.rest)
}
func (v BranchRelease) MarshalJSON() ([]byte, error) {
	return encodeKeeping(plainBranchRelease(v), v.rest)
}
func (v *GraphQL) UnmarshalJSON(b []byte) error { return decodeKeeping(b, (*plainGraphQL)(v), &v.rest) }
func (v GraphQL) MarshalJSON() ([]byte, error)  { return encodeKeeping(plainGraphQL(v), v.rest) }
func (v *Merge) UnmarshalJSON(b []byte) error   { return decodeKeeping(b, (*plainMerge)(v), &v.rest) }
func (v Merge) MarshalJSON() ([]byte, error)    { return encodeKeeping(plainMerge(v), v.rest) }
func (v *Writer) UnmarshalJSON(b []byte) error  { return decodeKeeping(b, (*plainWriter)(v), &v.rest) }
func (v Writer) MarshalJSON() ([]byte, error)   { return encodeKeeping(plainWriter(v), v.rest) }
func (v *WorkerReport) UnmarshalJSON(b []byte) error {
	return decodeKeeping(b, (*plainWorkerReport)(v), &v.rest)
}
func (v WorkerReport) MarshalJSON() ([]byte, error) {
	return encodeKeeping(plainWorkerReport(v), v.rest)
}
func (v *Failed) UnmarshalJSON(b []byte) error { return decodeKeeping(b, (*plainFailed)(v), &v.rest) }
func (v Failed) MarshalJSON() ([]byte, error)  { return encodeKeeping(plainFailed(v), v.rest) }
func (v *Owed) UnmarshalJSON(b []byte) error   { return decodeKeeping(b, (*plainOwed)(v), &v.rest) }
func (v Owed) MarshalJSON() ([]byte, error)    { return encodeKeeping(plainOwed(v), v.rest) }
func (v *Urgent) UnmarshalJSON(b []byte) error { return decodeKeeping(b, (*plainUrgent)(v), &v.rest) }
func (v Urgent) MarshalJSON() ([]byte, error)  { return encodeKeeping(plainUrgent(v), v.rest) }
func (v *RoleLine) UnmarshalJSON(b []byte) error {
	return decodeKeeping(b, (*plainRoleLine)(v), &v.rest)
}
func (v RoleLine) MarshalJSON() ([]byte, error) { return encodeKeeping(plainRoleLine(v), v.rest) }
func (v *Pair) UnmarshalJSON(b []byte) error    { return decodeKeeping(b, (*plainPair)(v), &v.rest) }
func (v Pair) MarshalJSON() ([]byte, error)     { return encodeKeeping(plainPair(v), v.rest) }
func (v *Stop) UnmarshalJSON(b []byte) error    { return decodeKeeping(b, (*plainStop)(v), &v.rest) }
func (v Stop) MarshalJSON() ([]byte, error)     { return encodeKeeping(plainStop(v), v.rest) }
func (v *HostPin) UnmarshalJSON(b []byte) error { return decodeKeeping(b, (*plainHostPin)(v), &v.rest) }
func (v HostPin) MarshalJSON() ([]byte, error)  { return encodeKeeping(plainHostPin(v), v.rest) }
func (v *LocalTask) UnmarshalJSON(b []byte) error {
	return decodeKeeping(b, (*plainLocalTask)(v), &v.rest)
}
func (v LocalTask) MarshalJSON() ([]byte, error) { return encodeKeeping(plainLocalTask(v), v.rest) }
func (v *TranscriptGrant) UnmarshalJSON(b []byte) error {
	return decodeKeeping(b, (*plainTranscriptGrant)(v), &v.rest)
}
func (v TranscriptGrant) MarshalJSON() ([]byte, error) {
	return encodeKeeping(plainTranscriptGrant(v), v.rest)
}
func (v *TranscriptRequest) UnmarshalJSON(b []byte) error {
	return decodeKeeping(b, (*plainTranscriptRequest)(v), &v.rest)
}
func (v TranscriptRequest) MarshalJSON() ([]byte, error) {
	return encodeKeeping(plainTranscriptRequest(v), v.rest)
}

func (v *GuideWork) UnmarshalJSON(b []byte) error {
	return decodeKeeping(b, (*plainGuideWork)(v), &v.rest)
}
func (v GuideWork) MarshalJSON() ([]byte, error) { return encodeKeeping(plainGuideWork(v), v.rest) }

func (v *HostPlacement) UnmarshalJSON(b []byte) error {
	return decodeKeeping(b, (*plainHostPlacement)(v), &v.rest)
}
func (v HostPlacement) MarshalJSON() ([]byte, error) {
	return encodeKeeping(plainHostPlacement(v), v.rest)
}
