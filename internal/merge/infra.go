package merge

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/giantswarm/beekeeper/internal/github"
)

// InfraSignature is a transient infrastructure failure a failed job's log
// shows: a line matching Pattern is no change's fault, the job run again
// may pass.
type InfraSignature struct {
	Name    string
	Pattern *regexp.Regexp
}

// InfraSignatures are the transient infrastructure failures the gate knows:
// module proxies, registries, checkouts, the network and the runner.
var InfraSignatures = []InfraSignature{
	{"Go module proxy", regexp.MustCompile(`(?:proxy|sum)\.golang\.org.*(?:stream error|INTERNAL_ERROR|connection reset|i/o timeout|TLS handshake timeout|unexpected EOF|: EOF\b|\b50[234]\b|Bad Gateway|Service Unavailable|Gateway Timeout)`)},
	{"container registry", regexp.MustCompile(`(?i)toomanyrequests|(?:registry|docker\.io|ghcr\.io|azurecr\.io|quay\.io|gcr\.io|pkg\.dev).*(?:\b50[234]\b|Bad Gateway|Service Unavailable|Gateway Timeout|i/o timeout|connection reset|TLS handshake timeout|unexpected EOF)`)},
	{"git checkout", regexp.MustCompile(`fatal: unable to access 'https://github\.com/[^']*': (?:The requested URL returned error: 5\d\d|Could not resolve host|Failed to connect)|error: RPC failed|fetch-pack: unexpected disconnect|remote: Internal Server Error`)},
	{"package registry", regexp.MustCompile(`npm (?:ERR!|error) (?:code|errno) (?:ECONNRESET|ETIMEDOUT|EAI_AGAIN|E50[234])`)},
	{"network", regexp.MustCompile(`Could not resolve host|Temporary failure in name resolution|TLS handshake timeout|dial tcp: lookup \S+ on \S+: (?:server misbehaving|i/o timeout)`)},
	{"runner", regexp.MustCompile(`The runner has received a shutdown signal|lost communication with the server|The hosted runner encountered an error`)},
}

// infraTail is how many of a job log's last lines are searched: the failure
// and the job's cleanup after it.
const infraTail = 200

var (
	// logStamp is the timestamp an Actions log line starts with, and the
	// error annotation after it.
	logStamp = regexp.MustCompile(`^(?:\d{4}-\d\d-\d\dT[\d:.]+Z\s+)?(?:##\[error\])?`)
	// escape is a terminal escape sequence of a log line.
	escape = regexp.MustCompile(`\x1b\[[0-9;]*[A-Za-z]`)
	// actionsJob is the URL of a check that is a GitHub Actions job:
	// repository, run and job.
	actionsJob = regexp.MustCompile(`^https://github\.com/([^/]+/[^/]+)/actions/runs/(\d+)/job/(\d+)`)
)

// InfraLine is the first line of log's tail that matches a signature, and
// the signature's name; ok false when none does.
func InfraLine(log string) (signature, line string, ok bool) {
	lines := strings.Split(log, "\n")
	for _, l := range lines[max(0, len(lines)-infraTail):] {
		l = strings.TrimSpace(logStamp.ReplaceAllString(escape.ReplaceAllString(l, ""), ""))
		for _, s := range InfraSignatures {
			if s.Pattern.MatchString(l) {
				return s.Name, l, true
			}
		}
	}
	return "", "", false
}

// Infra is a red check whose job died on a transient infrastructure
// failure: the matched signature and log line, and the job to rerun.
type Infra struct {
	Check     string `json:"check"`
	Repo      string `json:"repo"`
	Run       int64  `json:"run"`
	Job       int64  `json:"job"`
	Signature string `json:"signature"`
	Line      string `json:"line"`
}

// Rerun is the command that runs the failed job again.
func (i Infra) Rerun() string {
	return fmt.Sprintf("gh run rerun %d --job %d --repo %s", i.Run, i.Job, i.Repo)
}

// Why is the attribution: the signature, its log line, and the rerun. A
// long line keeps its end, where a tool's error names the failure.
func (i Infra) Why() string {
	line := i.Line
	if r := []rune(line); len(r) > 200 {
		line = "…" + string(r[len(r)-199:])
	}
	return fmt.Sprintf("a transient infrastructure failure, not this pull request's breakage (%s: %q): rerun the job (`%s`), then the same merge again",
		i.Signature, line, i.Rerun())
}

// String is the check and its attribution.
func (i Infra) String() string { return i.Check + ": " + i.Why() }

// InfraNote is the attribution of every infra-red check, "" for none.
func InfraNote(infra []Infra) string {
	parts := make([]string, 0, len(infra))
	for _, i := range infra {
		parts = append(parts, i.String())
	}
	return strings.Join(parts, "; ")
}

// maxInfraLogs bounds the job logs one red document reads.
const maxInfraLogs = 5

// InfraReds are the red checks of devctl's document that are GitHub Actions
// jobs whose log tail shows a transient infrastructure failure
// (InfraSignatures). A log GitHub does not hand over, a CircleCI job and a
// workflow are attributed as before.
func InfraReds(ctx context.Context, run github.GH, doc []byte) []Infra {
	reds, _ := redChecks(doc)
	var infra []Infra
	read := 0
	for _, r := range reds {
		m := actionsJob.FindStringSubmatch(r.url)
		if r.workflow || m == nil || read == maxInfraLogs {
			continue
		}
		read++
		runID, _ := strconv.ParseInt(m[2], 10, 64)
		job, _ := strconv.ParseInt(m[3], 10, 64)
		log, err := github.JobLog(ctx, run, m[1], job)
		if err != nil {
			continue
		}
		if sig, line, ok := InfraLine(log); ok {
			infra = append(infra, Infra{Check: r.name, Repo: m[1], Run: runID, Job: job, Signature: sig, Line: line})
		}
	}
	return infra
}

// infraOf is the named check's infra attribution, nil when it has none.
func infraOf(infra []Infra, name string) *Infra {
	for i := range infra {
		if infra[i].Check == name {
			return &infra[i]
		}
	}
	return nil
}
