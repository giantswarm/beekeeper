// Package feedback reads the person's replies to the scheduled status
// reports in Slack: where a report was posted (its slack_send_message
// result), the replies in its thread (the Slack MCP server's
// slack_read_thread text) and whom a reply is for.
package feedback

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

// Post is where a message was posted: its channel and ts.
type Post struct {
	Channel string
	TS      string
}

// ParsePost reads a slack_send_message result: its message_context.
func ParsePost(result string) (Post, error) {
	var v struct {
		Context struct {
			Channel string `json:"channel_id"`
			TS      string `json:"message_ts"`
		} `json:"message_context"`
	}
	if err := json.Unmarshal([]byte(result), &v); err != nil {
		return Post{}, fmt.Errorf("the post's result: %w", err)
	}
	if v.Context.Channel == "" || v.Context.TS == "" {
		return Post{}, fmt.Errorf("the post's result names no channel and ts")
	}
	return Post{Channel: v.Context.Channel, TS: v.Context.TS}, nil
}

// Message is one message of a thread.
type Message struct {
	// User is the author's Slack user id.
	User string
	TS   string
	Text string
}

// Thread is a thread's parent and its replies, oldest first.
type Thread struct {
	Parent  Message
	Replies []Message
}

var (
	// header opens the parent ("=== THREAD PARENT MESSAGE ===") or a reply
	// ("--- Reply 1 of 2 ---").
	header = regexp.MustCompile(`(?m)^(?:=== THREAD PARENT MESSAGE ===|--- Reply \d+ of \d+ ---)\n`)
	from   = regexp.MustCompile(`(?m)^From: .*\((\w+)\)$`)
	ts     = regexp.MustCompile(`(?m)^Message TS: (\d+\.\d+)$`)
	// replies closes the parent: the header of the replies' list.
	replies = regexp.MustCompile(`(?m)^=== THREAD REPLIES \(\d+ total\) ===\n?`)
)

// ParseThread reads slack_read_thread's result: a JSON object whose
// messages are the thread as text, the parent first, each message a header,
// From, Time and Message TS lines, then its text.
func ParseThread(result string) (Thread, error) {
	var v struct {
		Messages string `json:"messages"`
	}
	if err := json.Unmarshal([]byte(result), &v); err != nil {
		return Thread{}, fmt.Errorf("the thread: %w", err)
	}
	text := replies.ReplaceAllString(v.Messages, "")
	idx := header.FindAllStringIndex(text, -1)
	if len(idx) == 0 {
		return Thread{}, fmt.Errorf("the thread names no parent message")
	}
	var t Thread
	for i, at := range idx {
		end := len(text)
		if i+1 < len(idx) {
			end = idx[i+1][0]
		}
		m, err := parseMessage(text[at[1]:end])
		if err != nil {
			return Thread{}, err
		}
		if i == 0 {
			t.Parent = m
			continue
		}
		t.Replies = append(t.Replies, m)
	}
	return t, nil
}

// parseMessage reads one message: its From, Time and Message TS lines, then
// its text.
func parseMessage(s string) (Message, error) {
	f, n := from.FindStringSubmatch(s), ts.FindStringSubmatchIndex(s)
	if f == nil || n == nil {
		return Message{}, fmt.Errorf("a message of the thread names no author or ts: %q", firstLine(s))
	}
	return Message{User: f[1], TS: s[n[2]:n[3]], Text: strings.TrimSpace(s[n[1]:])}, nil
}

// Later reports whether Slack ts a is after b; an empty b is before all.
func Later(a, b string) bool {
	if b == "" {
		return true
	}
	as, af, _ := strings.Cut(a, ".")
	bs, bf, _ := strings.Cut(b, ".")
	if len(as) != len(bs) {
		return len(as) > len(bs)
	}
	if as != bs {
		return as > bs
	}
	return af > bf
}

// Route is whom a reply is for: the agent among names its text opens with,
// "<name>: <message>" (the longest, without regard to case), with the rest;
// none, "" and the whole text.
func Route(text string, names []string) (to, msg string) {
	for _, n := range names {
		if len(n) <= len(to) || len(text) <= len(n) || !strings.EqualFold(text[:len(n)], n) {
			continue
		}
		if rest, ok := strings.CutPrefix(text[len(n):], ":"); ok && strings.TrimSpace(rest) != "" {
			to, msg = n, strings.TrimSpace(rest)
		}
	}
	if to == "" {
		return "", text
	}
	return to, msg
}

func firstLine(s string) string {
	line, _, _ := strings.Cut(strings.TrimSpace(s), "\n")
	return line
}
