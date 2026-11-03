package claude

import (
	"bufio"
	"bytes"
	"cmp"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/giantswarm/beekeeper/internal/config"
)

// SDKEntrypoint is the entrypoint a transcript of a session the Claude Agent
// SDK for Python ran records: the security-guidance plugin's review runs as
// one on a session's Stop, commit and push, in that session's folder.
const SDKEntrypoint = "sdk-py"

// transcriptExt ends the name of a Claude Code transcript.
const transcriptExt = ".jsonl"

// sdkHeadLines bounds the lines read for a transcript's entrypoint: its
// first user entry carries it.
const sdkHeadLines = 32

// SDKChild is one sdk-py session run in a folder: when its first request
// went out, what its requests cost, and on which models.
type SDKChild struct {
	Session  string    `json:"session"`
	Started  time.Time `json:"started"`
	Models   []string  `json:"models"`
	Requests int       `json:"requests"`
	USD      float64   `json:"usd"`
	// Unpriced: a model of its requests has no price, so USD is short.
	Unpriced bool `json:"unpriced,omitempty"`
}

// SDKChildren reads the sdk-py sessions of the project folder dir with a
// request at or after since, oldest first, priced with m. Every other
// transcript, and one last written before since, is passed over; an
// unreadable folder has none.
func SDKChildren(dir string, since time.Time, m config.Metrics) []SDKChild {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []SDKChild
	until := time.Now().Add(time.Hour) // a request's clock may run ahead
	for _, e := range entries {
		if !e.Type().IsRegular() || filepath.Ext(e.Name()) != transcriptExt {
			continue
		}
		if fi, err := e.Info(); err != nil || fi.ModTime().Before(since) {
			continue
		}
		p := filepath.Join(dir, e.Name())
		if transcriptEntrypoint(p) != SDKEntrypoint {
			continue
		}
		rs := transcriptRequests(p, since, until)
		if len(rs) == 0 {
			continue
		}
		s := PriceSpend([]SpendTranscript{{Requests: rs}}, since, until, m)
		c := SDKChild{Session: strings.TrimSuffix(e.Name(), transcriptExt), Started: rs[0].At, Requests: s.Requests, USD: s.USD, Unpriced: len(s.Unpriced) > 0}
		for _, r := range rs {
			c.Started = minTime(c.Started, r.At)
		}
		for _, ms := range s.Models {
			c.Models = append(c.Models, ms.Model)
		}
		out = append(out, c)
	}
	slices.SortFunc(out, func(a, b SDKChild) int {
		return cmp.Or(a.Started.Compare(b.Started), strings.Compare(a.Session, b.Session))
	})
	return out
}

func minTime(a, b time.Time) time.Time {
	if b.Before(a) {
		return b
	}
	return a
}

// transcriptEntrypoint is the entrypoint the first entries of the transcript
// at p record, "" for none.
func transcriptEntrypoint(p string) string {
	f, err := os.Open(filepath.Clean(p))
	if err != nil {
		return ""
	}
	defer func() { _ = f.Close() }()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64<<10), 16<<20)
	for n := 0; n < sdkHeadLines && sc.Scan(); n++ {
		line := sc.Bytes()
		if !bytes.Contains(line, []byte(`"entrypoint"`)) {
			continue
		}
		var e struct {
			Entrypoint string `json:"entrypoint"`
		}
		if json.Unmarshal(line, &e) == nil && e.Entrypoint != "" {
			return e.Entrypoint
		}
	}
	return ""
}
