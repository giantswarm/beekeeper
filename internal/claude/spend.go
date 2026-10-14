package claude

import (
	"bufio"
	"bytes"
	"cmp"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/giantswarm/beekeeper/internal/config"
)

// Spend is what the API requests of a window cost, read from every
// transcript of the harnesses: the sessions' and their subagents' files,
// whether the session runs, is paused or was archived. A request counts
// once, by its id, however many transcripts carry it (a forked or resumed
// session copies its history).
type Spend struct {
	From time.Time `json:"from"`
	To   time.Time `json:"to"`
	// Transcripts are the files with a request in the window, Sessions the
	// sessions' own (not their subagents').
	Transcripts int    `json:"transcripts"`
	Sessions    int    `json:"sessions"`
	Requests    int    `json:"requests"`
	Tokens      Tokens `json:"tokens"`
	// USD is the cost of the priced requests; Unpriced names the models
	// whose requests have no price, and those requests are in Models only.
	USD      float64      `json:"usd"`
	Unpriced []string     `json:"unpriced,omitempty"`
	Models   []ModelSpend `json:"models"`
}

// ModelSpend is one model's share of a Spend; a fast request's model is
// marked (fast).
type ModelSpend struct {
	Model    string  `json:"model"`
	Priced   bool    `json:"priced"`
	Requests int     `json:"requests"`
	Tokens   Tokens  `json:"tokens"`
	USD      float64 `json:"usd"`
}

// SpendRequest is an API request a transcript records.
type SpendRequest struct {
	// ID is what the request counts once by; empty, it always counts.
	ID string
	At time.Time
	// Model is the model it is priced by and listed under.
	Model  string
	Fast   bool
	Tokens Tokens
}

// SpendTranscript is the requests of one transcript in a window; a subagent's
// transcript is no session.
type SpendTranscript struct {
	Subagent bool
	Requests []SpendRequest
}

// ReadSpendTranscripts reads the requests from from up to to of every Claude
// Code transcript under dir. A transcript last written before from has
// none and is not read.
func ReadSpendTranscripts(dir string, from, to time.Time) ([]SpendTranscript, error) {
	var files []string
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if p == dir {
				return err
			}
			return nil //nolint:nilerr // an unreadable entry is passed over
		}
		if !d.Type().IsRegular() || filepath.Ext(p) != ".jsonl" {
			return nil
		}
		if fi, err := d.Info(); err == nil && !fi.ModTime().Before(from) {
			files = append(files, p)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	ts := make([]SpendTranscript, len(files))
	var wg sync.WaitGroup
	sem := make(chan struct{}, runtime.GOMAXPROCS(0))
	for i, p := range files {
		sem <- struct{}{}
		wg.Go(func() {
			defer func() { <-sem }()
			ts[i] = SpendTranscript{Subagent: filepath.Base(filepath.Dir(p)) == "subagents", Requests: transcriptRequests(p, from, to)}
		})
	}
	wg.Wait()
	return ts, nil
}

// PriceSpend counts each request of ts once, in their order, and prices
// it with m.
func PriceSpend(ts []SpendTranscript, from, to time.Time, m config.Metrics) Spend {
	s := Spend{From: from, To: to}
	byID := map[string]int{}
	var all []message
	for _, t := range ts {
		if len(t.Requests) == 0 {
			continue
		}
		s.Transcripts++
		if !t.Subagent {
			s.Sessions++
		}
		for _, r := range t.Requests {
			msg := message{at: r.At, model: r.Model, fast: r.Fast, tokens: r.Tokens}
			j, seen := byID[r.ID]
			switch {
			case r.ID == "":
				all = append(all, msg)
			case !seen:
				byID[r.ID] = len(all)
				all = append(all, msg)
			case r.Tokens.Sum() > all[j].tokens.Sum():
				all[j] = msg
			}
		}
	}
	byModel := map[string]*ModelSpend{}
	for _, msg := range all {
		name := msg.priceName()
		ms := byModel[name]
		if ms == nil {
			ms = &ModelSpend{Model: name, Priced: true}
			byModel[name] = ms
		}
		c, ok := msg.price(m)
		if !ok {
			ms.Priced = false
		}
		ms.Requests++
		ms.Tokens.add(msg.tokens)
		ms.USD += c
		s.Requests++
		s.Tokens.add(msg.tokens)
		s.USD += c
	}
	for _, ms := range byModel {
		s.Models = append(s.Models, *ms)
		if !ms.Priced {
			s.Unpriced = append(s.Unpriced, ms.Model)
		}
	}
	slices.SortFunc(s.Models, func(a, b ModelSpend) int {
		return cmp.Or(cmp.Compare(b.USD, a.USD), cmp.Compare(b.Tokens.Sum(), a.Tokens.Sum()), strings.Compare(a.Model, b.Model))
	})
	slices.Sort(s.Unpriced)
	return s
}

// spendEntry is the part of a transcript line a request's price needs.
type spendEntry struct {
	Type      string    `json:"type"`
	Timestamp time.Time `json:"timestamp"`
	Message   struct {
		ID    string    `json:"id"`
		Model string    `json:"model"`
		Usage *apiUsage `json:"usage"`
	} `json:"message"`
}

// transcriptRequests reads the whole transcript at p for its requests
// from from up to to: a streamed response repeats its id and usage on one
// line per content block, the last carrying the final usage, and is at
// its first line's time. A file that cannot be opened has none.
func transcriptRequests(p string, from, to time.Time) []SpendRequest {
	f, err := os.Open(filepath.Clean(p))
	if err != nil {
		return nil
	}
	defer func() { _ = f.Close() }()
	r := bufio.NewReaderSize(f, 1<<20)
	var out []SpendRequest
	byID := map[string]int{}
	for {
		line, err := r.ReadBytes('\n')
		if bytes.Contains(line, []byte(`"usage"`)) && bytes.Contains(line, []byte(`"type":"assistant"`)) {
			var e spendEntry
			if json.Unmarshal(line, &e) == nil && e.Type == roleAssistant && e.Message.Usage != nil {
				u := e.Message.Usage
				r := SpendRequest{ID: e.Message.ID, At: e.Timestamp, Model: e.Message.Model, Fast: u.Speed == "fast", Tokens: u.tokens()}
				if i, ok := byID[r.ID]; ok && r.ID != "" {
					r.At = out[i].At
					out[i] = r
				} else if r.Tokens.Sum() > 0 {
					byID[r.ID] = len(out)
					out = append(out, r)
				}
			}
		}
		if err != nil {
			break // io.EOF, or a file cut short: what was read counts
		}
	}
	return InWindow(out, from, to)
}

// InWindow drops the requests of rs before from, at or after to, or
// without tokens.
func InWindow(rs []SpendRequest, from, to time.Time) []SpendRequest {
	return slices.DeleteFunc(rs, func(r SpendRequest) bool {
		return r.At.Before(from) || !r.At.Before(to) || r.Tokens.Sum() == 0
	})
}
