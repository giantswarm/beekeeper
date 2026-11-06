package github

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

// memoryIssue is an issue body with GitHub's blind replace.
type memoryIssue struct {
	mu     sync.Mutex
	body   string
	reads  int
	writes []string
	// beforeRead changes the body from outside at a given read.
	beforeRead func(read int, body string) string
}

func (m *memoryIssue) issueBody() IssueBody {
	return IssueBody{
		Read: func(context.Context) (string, error) {
			m.mu.Lock()
			defer m.mu.Unlock()
			m.reads++
			if m.beforeRead != nil {
				m.body = m.beforeRead(m.reads, m.body)
			}
			return m.body, nil
		},
		Write: func(_ context.Context, body string) error {
			m.mu.Lock()
			defer m.mu.Unlock()
			m.body = body
			m.writes = append(m.writes, body)
			return nil
		},
		Sleep: func(time.Duration) {},
	}
}

func TestAddQueueRowKeepsTheRowsForm(t *testing.T) {
	for _, tc := range []struct{ name, body, want string }{
		{"empty", "", "1. new\n"},
		{"prose", "Queue:\n", "Queue:\n\n1. new\n"},
		{"list", "Queue:\n1. a\n2. b\n\nnotes\n", "Queue:\n1. a\n2. b\n3. new\n\nnotes\n"},
		{"table", "| # | what |\n|---|---|\n| 4 | a |\n| 9 | b |\nend\n", "| # | what |\n|---|---|\n| 4 | a |\n| 9 | b |\n| 10 | new |\nend\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := &memoryIssue{body: tc.body}
			if _, err := AddQueueRow(context.Background(), m.issueBody(), "new"); err != nil {
				t.Fatal(err)
			}
			if m.body != tc.want {
				t.Fatalf("body %q, want %q", m.body, tc.want)
			}
		})
	}
}

// A change between the read and the write is never overwritten: the round
// starts over and the new row takes the next number.
func TestAddQueueRowRetriesAChangedBody(t *testing.T) {
	m := &memoryIssue{body: "1. a\n"}
	m.beforeRead = func(read int, body string) string {
		if read == 2 {
			return body + "2. theirs\n"
		}
		return body
	}
	n, err := AddQueueRow(context.Background(), m.issueBody(), "mine")
	if err != nil {
		t.Fatal(err)
	}
	if want := "1. a\n2. theirs\n3. mine\n"; n != 3 || m.body != want {
		t.Fatalf("row %d, body %q, want %q", n, m.body, want)
	}
	if len(m.writes) != 1 {
		t.Fatalf("writes %q, want only the one after the fresh read", m.writes)
	}
}

// A writer that replaced the body right after ours took the row away: the
// read after the write sees that and lands it again.
func TestAddQueueRowProvesTheWriteLanded(t *testing.T) {
	m := &memoryIssue{body: "1. a\n"}
	m.beforeRead = func(read int, body string) string {
		if read == 3 {
			return "1. a\n2. theirs\n"
		}
		return body
	}
	n, err := AddQueueRow(context.Background(), m.issueBody(), "mine")
	if err != nil {
		t.Fatal(err)
	}
	if want := "1. a\n2. theirs\n3. mine\n"; n != 3 || m.body != want {
		t.Fatalf("row %d, body %q, want %q", n, m.body, want)
	}
}

func TestAddQueueRowAddsARowOnce(t *testing.T) {
	m := &memoryIssue{body: "1. a\n2. mine\n"}
	n, err := AddQueueRow(context.Background(), m.issueBody(), "mine")
	if err != nil || n != 2 || len(m.writes) != 0 {
		t.Fatalf("row %d, err %v, writes %q", n, err, m.writes)
	}
}

func TestAddQueueRowGivesUpOnABodyThatKeepsChanging(t *testing.T) {
	m := &memoryIssue{body: "1. a\n"}
	m.beforeRead = func(read int, body string) string { return fmt.Sprintf("%s%d. other\n", body, read+1) }
	if _, err := AddQueueRow(context.Background(), m.issueBody(), "mine"); !errors.Is(err, ErrQueueContended) {
		t.Fatalf("err %v", err)
	}
	if strings.Contains(m.body, "mine") {
		t.Fatalf("wrote over a changing body: %q", m.body)
	}
}

func TestAddQueueRowRefusesAMultiLineRow(t *testing.T) {
	m := &memoryIssue{}
	if _, err := AddQueueRow(context.Background(), m.issueBody(), "a\nb"); err == nil {
		t.Fatal("accepted a row of two lines")
	}
}
