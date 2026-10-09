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
// transcript under Claude Code's projects directory: the sessions' and
// their subagents' .jsonl files, whether the session runs, is paused or
// was archived. A request counts once, by its message id, however many
// transcripts carry it (a forked or resumed session copies its history).
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

// spendRequest is a request of one transcript and the message id it
// counts once by.
type spendRequest struct {
	id string
	message
}

// ReadSpend prices the requests from from up to to of every transcript
// under dir with m. A transcript last written before from has none and
// is not read.
func ReadSpend(dir string, from, to time.Time, m config.Metrics) (Spend, error) {
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
		return Spend{}, err
	}
	reqs := make([][]spendRequest, len(files))
	var wg sync.WaitGroup
	sem := make(chan struct{}, runtime.GOMAXPROCS(0))
	for i, p := range files {
		sem <- struct{}{}
		wg.Go(func() {
			defer func() { <-sem }()
			reqs[i] = transcriptRequests(p, from, to)
		})
	}
	wg.Wait()
	return priceSpend(files, reqs, from, to, m), nil
}

// priceSpend counts each request once, in the order of files, and prices
// it.
func priceSpend(files []string, reqs [][]spendRequest, from, to time.Time, m config.Metrics) Spend {
	s := Spend{From: from, To: to}
	byID := map[string]int{}
	var all []message
	for i, rs := range reqs {
		if len(rs) == 0 {
			continue
		}
		s.Transcripts++
		if filepath.Base(filepath.Dir(files[i])) != "subagents" {
			s.Sessions++
		}
		for _, r := range rs {
			j, seen := byID[r.id]
			switch {
			case r.id == "":
				all = append(all, r.message)
			case !seen:
				byID[r.id] = len(all)
				all = append(all, r.message)
			case r.tokens.Sum() > all[j].tokens.Sum():
				all[j] = r.message
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
func transcriptRequests(p string, from, to time.Time) []spendRequest {
	f, err := os.Open(filepath.Clean(p))
	if err != nil {
		return nil
	}
	defer func() { _ = f.Close() }()
	r := bufio.NewReaderSize(f, 1<<20)
	var out []spendRequest
	byID := map[string]int{}
	for {
		line, err := r.ReadBytes('\n')
		if bytes.Contains(line, []byte(`"usage"`)) && bytes.Contains(line, []byte(`"type":"assistant"`)) {
			var e spendEntry
			if json.Unmarshal(line, &e) == nil && e.Type == roleAssistant && e.Message.Usage != nil {
				u := e.Message.Usage
				msg := message{at: e.Timestamp, model: e.Message.Model, fast: u.Speed == "fast", tokens: u.tokens()}
				if i, ok := byID[e.Message.ID]; ok && e.Message.ID != "" {
					msg.at = out[i].at
					out[i].message = msg
				} else if msg.tokens.Sum() > 0 {
					byID[e.Message.ID] = len(out)
					out = append(out, spendRequest{id: e.Message.ID, message: msg})
				}
			}
		}
		if err != nil {
			break // io.EOF, or a file cut short: what was read counts
		}
	}
	return slices.DeleteFunc(out, func(r spendRequest) bool {
		return r.at.Before(from) || !r.at.Before(to) || r.tokens.Sum() == 0
	})
}
