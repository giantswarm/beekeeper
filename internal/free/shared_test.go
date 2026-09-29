package free

import (
	"bytes"
	"fmt"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestSharedOneScanForConcurrentCallers(t *testing.T) {
	s := Shared{Dir: t.TempDir(), Key: "k", MaxAge: 30 * time.Second, Timeout: 10 * time.Second}
	var scans atomic.Int32
	scan := func(w io.Writer) error {
		scans.Add(1)
		time.Sleep(300 * time.Millisecond)
		_, _ = fmt.Fprintln(w, "kind\tlab\t900\t12:00")
		return nil
	}
	outs := make([]string, 10)
	var wg sync.WaitGroup
	for i := range outs {
		wg.Go(func() {
			var b bytes.Buffer
			if err := s.Print(&b, scan); err != nil {
				t.Error(err)
			}
			outs[i] = b.String()
		})
	}
	wg.Wait()
	if n := scans.Load(); n != 1 {
		t.Fatalf("scans = %d, want 1", n)
	}
	for i, o := range outs {
		if !strings.HasSuffix(o, "kind\tlab\t900\t12:00\n") {
			t.Errorf("caller %d got %q", i, o)
		}
	}
}

func TestSharedReusesAFreshResult(t *testing.T) {
	now := time.Date(2026, 9, 29, 3, 0, 0, 0, time.UTC)
	s := Shared{Dir: t.TempDir(), Key: "k", MaxAge: 30 * time.Second, Timeout: 10 * time.Second, Now: func() time.Time { return now }}
	scans := 0
	scan := func(w io.Writer) error {
		scans++
		_, _ = fmt.Fprintf(w, "cli\t%d\t500\t60\t/work\n", scans)
		return nil
	}
	var first bytes.Buffer
	if err := s.Print(&first, scan); err != nil {
		t.Fatal(err)
	}
	if got := first.String(); got != "cli\t1\t500\t60\t/work\n" {
		t.Fatalf("first = %q", got)
	}

	now = now.Add(12 * time.Second)
	var second bytes.Buffer
	if err := s.Print(&second, scan); err != nil {
		t.Fatal(err)
	}
	if got, want := second.String(), "cached\t12\ncli\t1\t500\t60\t/work\n"; got != want {
		t.Fatalf("second = %q, want %q", got, want)
	}

	other := s
	other.Key = "other options"
	var third bytes.Buffer
	if err := other.Print(&third, scan); err != nil {
		t.Fatal(err)
	}
	if got := third.String(); got != "cli\t2\t500\t60\t/work\n" {
		t.Fatalf("other options reused the cache: %q", got)
	}

	now = now.Add(31 * time.Second)
	var fourth bytes.Buffer
	if err := other.Print(&fourth, scan); err != nil {
		t.Fatal(err)
	}
	if got := fourth.String(); got != "cli\t3\t500\t60\t/work\n" {
		t.Fatalf("stale result reused: %q", got)
	}
}

func TestSharedTimeoutPrintsWhatItHas(t *testing.T) {
	s := Shared{Dir: t.TempDir(), Key: "k", MaxAge: 30 * time.Second, Timeout: 200 * time.Millisecond}
	block := make(chan struct{})
	defer close(block)
	scan := func(w io.Writer) error {
		_, _ = fmt.Fprintln(w, "tab\t77\t300\t01:00\trenderer")
		_, _ = io.WriteString(w, "proc\t88\t") // a row still being written
		<-block
		return nil
	}
	start := time.Now()
	var b bytes.Buffer
	if err := s.Print(&b, scan); err != nil {
		t.Fatal(err)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("took %s past a 200ms timeout", d)
	}
	if got, want := b.String(), "tab\t77\t300\t01:00\trenderer\npartial\t0\n"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
	if _, _, ok := s.read(); ok {
		t.Error("a partial result was cached")
	}
}

func TestSharedWaiterTimesOutBehindARunningScan(t *testing.T) {
	dir := t.TempDir()
	block := make(chan struct{})
	started := make(chan struct{})
	holder := Shared{Dir: dir, Key: "k", MaxAge: 30 * time.Second, Timeout: 5 * time.Second}
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = holder.Print(io.Discard, func(io.Writer) error { close(started); <-block; return nil })
	}()
	<-started
	waiter := holder
	waiter.Timeout = 300 * time.Millisecond
	var b bytes.Buffer
	if err := waiter.Print(&b, func(io.Writer) error { t.Error("waiter scanned"); return nil }); err != nil {
		t.Fatal(err)
	}
	if got := b.String(); got != "partial\t0\n" {
		t.Fatalf("got %q", got)
	}
	close(block)
	<-done
}
