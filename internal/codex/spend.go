package codex

import (
	"cmp"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/giantswarm/beekeeper/internal/claude"
)

// SpendTranscripts reads the Codex rollout files under stateDir's isolated
// CODEX_HOME and sessionsDir modified since `from`, returning the token usage
// requests within [from, to] for `beekeeper spend`.
func SpendTranscripts(stateDir, sessionsDir string, from, to time.Time) []claude.SpendTranscript {
	seen := map[string]bool{}
	var out []claude.SpendTranscript
	for _, dir := range sessionDirs(stateDir, sessionsDir) {
		var paths []string
		for _, pat := range []string{
			filepath.Join(dir, "*", "*", "*", "rollout-*.jsonl"),
			filepath.Join(dir, "*", "rollout-*.jsonl"),
			filepath.Join(dir, "rollout-*.jsonl"),
			filepath.Join(dir, "*.jsonl"),
		} {
			matches, _ := filepath.Glob(pat)
			paths = append(paths, matches...)
		}
		for _, p := range paths {
			if seen[p] {
				continue
			}
			seen[p] = true
			if fi, err := os.Stat(p); err != nil || (!from.IsZero() && fi.ModTime().Before(from)) {
				continue
			}
			if reqs := spendRequests(p, from, to); len(reqs) > 0 {
				out = append(out, claude.SpendTranscript{
					Requests: reqs,
				})
			}
		}
	}
	return out
}

func spendRequests(path string, from, to time.Time) []claude.SpendRequest {
	f, err := os.Open(filepath.Clean(path))
	if err != nil {
		return nil
	}
	defer func() { _ = f.Close() }()

	var id, currentModel string
	var prevTotal tokenUsage
	var out []claude.SpendRequest
	var step int

	sc := newScanner(f)
	for sc.Scan() {
		var l rolloutLine
		if json.Unmarshal(sc.Bytes(), &l) != nil {
			continue
		}
		switch l.Type {
		case evSessionMeta:
			var m sessionMetaPayload
			if json.Unmarshal(l.Payload, &m) == nil {
				id = cmp.Or(id, m.ID)
			}
		case evTurnContext:
			var tc turnContextPayload
			if json.Unmarshal(l.Payload, &tc) == nil && tc.Model != "" {
				currentModel = tc.Model
			}
		case evEventMsg:
			var em eventMsgPayload
			if json.Unmarshal(l.Payload, &em) != nil || em.Type != "token_count" || em.Info == nil {
				continue
			}
			tok, nextTotal, ok := stepTokens(em.Info.TotalTokenUsage, em.Info.LastTokenUsage, prevTotal)
			if !ok {
				continue
			}
			prevTotal = nextTotal
			step++
			reqID := ""
			if id != "" {
				reqID = id + "#" + strconv.Itoa(step)
			}
			out = append(out, claude.SpendRequest{
				ID:     reqID,
				At:     l.Timestamp,
				Model:  cmp.Or(currentModel, Harness),
				Tokens: tok,
			})
		case evThreadStarted:
			id = cmp.Or(id, l.ThreadID)
		case evTurnCompleted:
			if l.Usage != nil {
				tok := usageToClaudeTokens(*l.Usage)
				step++
				reqID := ""
				if id != "" {
					reqID = id + "#" + strconv.Itoa(step)
				}
				out = append(out, claude.SpendRequest{
					ID:     reqID,
					At:     l.Timestamp,
					Model:  cmp.Or(currentModel, Harness),
					Tokens: tok,
				})
			}
		}
	}
	return claude.InWindow(out, from, to)
}
