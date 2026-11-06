package github

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// queueRowTries bounds AddQueueRow's rounds, queueRowBackoff the wait between
// them.
const (
	queueRowTries   = 8
	queueRowBackoff = 200 * time.Millisecond
)

// ErrQueueContended is AddQueueRow giving up: the body kept changing.
var ErrQueueContended = errors.New("the issue body kept changing")

var (
	queueListRow  = regexp.MustCompile(`^\s*(\d+)\.\s`)
	queueTableRow = regexp.MustCompile(`^\s*\|\s*(\d+)\s*\|`)
)

// IssueBody is one shared issue body: Read returns it, Write replaces it.
type IssueBody struct {
	Read  func(context.Context) (string, error)
	Write func(context.Context, string) error
	Sleep func(time.Duration)
}

// AddQueueRow adds row to the numbered queue in the body and returns its
// number. GitHub has no conditional write of an issue body, so this is the
// closest compare-and-swap it allows: the body is read again right before
// the write, a change starts the round over, and a read after the write
// proves the row landed, as a writer outside the caller's lock can replace it.
// A row already in the body is not added twice: a lost reply retries safely.
func AddQueueRow(ctx context.Context, b IssueBody, row string) (int, error) {
	row = strings.TrimSpace(row)
	if row == "" || strings.ContainsAny(row, "\r\n") {
		return 0, errors.New("a queue row is one line of text")
	}
	for try := range queueRowTries {
		if try > 0 && b.Sleep != nil {
			b.Sleep(queueRowBackoff * time.Duration(try))
		}
		seen, err := b.Read(ctx)
		if err != nil {
			return 0, err
		}
		if n, ok := queueRowNumber(seen, row); ok {
			return n, nil
		}
		next, n := withQueueRow(seen, row)
		now, err := b.Read(ctx)
		if err != nil {
			return 0, err
		}
		if now != seen {
			continue
		}
		if err := b.Write(ctx, next); err != nil {
			return 0, err
		}
		landed, err := b.Read(ctx)
		if err != nil {
			return 0, err
		}
		if got, ok := queueRowNumber(landed, row); ok && got == n {
			return n, nil
		}
	}
	return 0, fmt.Errorf("%w over %d tries", ErrQueueContended, queueRowTries)
}

// queueRowNumber is the number of the row whose text is row.
func queueRowNumber(body, row string) (int, bool) {
	for line := range strings.Lines(body) {
		line = strings.TrimRight(line, "\r\n")
		n, ok := rowNumber(line)
		if !ok {
			continue
		}
		text := strings.TrimSpace(line[strings.IndexAny(line, ".|")+1:])
		if queueTableRow.MatchString(line) {
			_, text, _ = strings.Cut(strings.TrimSpace(line)[1:], "|")
			text = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(text), "|"))
		}
		if text == row {
			return n, true
		}
	}
	return 0, false
}

// rowNumber is a numbered list's or a table's row number on line.
func rowNumber(line string) (int, bool) {
	m := queueListRow.FindStringSubmatch(line)
	if m == nil {
		m = queueTableRow.FindStringSubmatch(line)
	}
	if m == nil {
		return 0, false
	}
	n, err := strconv.Atoi(m[1])
	return n, err == nil
}

// withQueueRow is body with row after its last numbered row, in that row's
// form and one above the highest number, and that number. A body without
// numbered rows gets a list of one at its end.
func withQueueRow(body, row string) (string, int) {
	lines := strings.Split(body, "\n")
	last, highest := -1, 0
	for i, line := range lines {
		if n, ok := rowNumber(strings.TrimRight(line, "\r")); ok {
			last, highest = i, max(highest, n)
		}
	}
	n := highest + 1
	if last < 0 {
		body = strings.TrimRight(body, "\n")
		if body != "" {
			body += "\n\n"
		}
		return fmt.Sprintf("%s%d. %s\n", body, n, row), n
	}
	line := fmt.Sprintf("%d. %s", n, row)
	if queueTableRow.MatchString(lines[last]) {
		line = fmt.Sprintf("| %d | %s |", n, row)
	}
	return strings.Join(append(lines[:last+1:last+1], append([]string{line}, lines[last+1:]...)...), "\n"), n
}
