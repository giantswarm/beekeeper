// Package feedback reads the person's replies to the scheduled status
// reports in Slack: where a report was posted (its slack_send_message
// result), the replies in its thread (the Slack MCP server's
// slack_read_thread text) and whom a reply is for.
package feedback

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
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
	// Boundaries are reserved by the unescaped wire format. Recognize even
	// malformed boundaries so message text cannot hide an ambiguous structure.
	boundary      = regexp.MustCompile(`(?m)^(?:=== THREAD|--- Reply)[^\n]*(?:\n|$)`)
	replyHeader   = regexp.MustCompile(`^--- Reply (\d+) of (\d+) ---$`)
	repliesHeader = regexp.MustCompile(`^=== THREAD REPLIES \((\d+) total\) ===$`)
	metadata      = regexp.MustCompile(`^From: [^\n]*\((\w+)\)\nTime: [^\n]+\nMessage TS: (\d+\.\d+)(?:\n|$)`)
	// trailer is what the server and the connector append to a message's
	// text: its reactions, and the "Sent using Claude" attribution of a
	// message posted through the claude.ai connector.
	trailer = regexp.MustCompile(`(?:\n(?:Reactions: .*|\*Sent using\* <@\w+\|[^>\n]*>))+\s*$`)
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
	text := v.Messages
	idx := boundary.FindAllStringIndex(text, -1)
	if len(idx) < 2 || idx[0][0] != 0 || strings.TrimSuffix(text[idx[0][0]:idx[0][1]], "\n") != "=== THREAD PARENT MESSAGE ===" {
		return Thread{}, fmt.Errorf("the thread has no leading parent message")
	}
	section := repliesHeader.FindStringSubmatch(strings.TrimSuffix(text[idx[1][0]:idx[1][1]], "\n"))
	if section == nil {
		return Thread{}, fmt.Errorf("the thread has no replies section after its parent")
	}
	count, err := strconv.Atoi(section[1])
	if err != nil || count != len(idx)-2 {
		return Thread{}, fmt.Errorf("the thread's reply count does not match its boundaries")
	}
	parent, err := parseMessage(text[idx[0][1]:idx[1][0]])
	if err != nil {
		return Thread{}, err
	}
	resultThread := Thread{Parent: parent}
	if count == 0 && strings.TrimSpace(text[idx[1][1]:]) != "" {
		return Thread{}, fmt.Errorf("the empty replies section contains text")
	}
	for i := 2; i < len(idx); i++ {
		if i == 2 && strings.TrimSpace(text[idx[1][1]:idx[i][0]]) != "" {
			return Thread{}, fmt.Errorf("the replies section contains unexpected text")
		}
		h := replyHeader.FindStringSubmatch(strings.TrimSuffix(text[idx[i][0]:idx[i][1]], "\n"))
		if h == nil || h[1] != strconv.Itoa(i-1) || h[2] != strconv.Itoa(count) {
			return Thread{}, fmt.Errorf("the thread's reply sequence is inconsistent")
		}
		end := len(text)
		if i+1 < len(idx) {
			end = idx[i+1][0]
		}
		message, err := parseMessage(text[idx[i][1]:end])
		if err != nil {
			return Thread{}, err
		}
		resultThread.Replies = append(resultThread.Replies, message)
	}
	return resultThread, nil
}

// parseMessage reads only the leading metadata, then the text without trailers.
func parseMessage(s string) (Message, error) {
	m := metadata.FindStringSubmatchIndex(s)
	if m == nil {
		return Message{}, fmt.Errorf("a message of the thread has invalid leading metadata: %q", firstLine(s))
	}
	return Message{User: s[m[2]:m[3]], TS: s[m[4]:m[5]], Text: strings.TrimSpace(trailer.ReplaceAllString(strings.TrimSpace(s[m[1]:]), ""))}, nil
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
