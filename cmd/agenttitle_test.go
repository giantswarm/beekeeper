package cmd

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/state"
)

// A session whose desktop record lost its name is asked, through its
// desktop CLI's socket, to set its own title, and keepTitle waits for the
// desktop to record it; a record that kept the name is left alone.
func TestRetitle(t *testing.T) {
	const name = "test: title after turn"
	sock := func(context.Context) string { return "/run/user/1000/cc-socks/42.sock" }
	for _, c := range []struct {
		desc     string
		titles   []string // the record's title, read by read
		socket   func(context.Context) string
		sendErr  error
		wantSent bool
		want     string // a substring of the line or the error
		wantErr  bool
	}{
		{desc: "kept", titles: []string{name}, socket: sock, want: "keeps its title"},
		{desc: "lost, then retitled", titles: []string{"", "", name}, socket: sock, wantSent: true, want: "the session retitled itself"},
		{desc: "replaced, then retitled", titles: []string{"klaus-lab", name}, socket: sock, wantSent: true, want: `"klaus-lab" instead of`},
		{desc: "no desktop CLI", titles: []string{""}, socket: func(context.Context) string { return "" }, want: "runs no CLI", wantErr: true},
		{desc: "send fails", titles: []string{""}, socket: sock, sendErr: errors.New("not sent"), wantSent: true, want: "not sent", wantErr: true},
		{desc: "never recorded", titles: []string{""}, socket: sock, wantSent: true, want: "did not retitle itself", wantErr: true},
	} {
		t.Run(c.desc, func(t *testing.T) {
			var reads atomic.Int32
			title := func() string {
				i := int(reads.Add(1)) - 1
				return c.titles[min(i, len(c.titles)-1)]
			}
			var to, msg string
			send := func(_ context.Context, t, m string) error {
				to, msg = t, m
				return c.sendErr
			}
			line, err := retitle(context.Background(), name, title, c.socket, send, 3*time.Second)
			got := line
			if err != nil {
				got = err.Error()
			}
			if (err != nil) != c.wantErr || !strings.Contains(got, c.want) {
				t.Errorf("retitle = %q, %v; want %q, error %v", line, err, c.want, c.wantErr)
			}
			if sent := to != ""; sent != c.wantSent {
				t.Fatalf("sent = %v, want %v", sent, c.wantSent)
			}
			if c.wantSent && (to != "uds:/run/user/1000/cc-socks/42.sock" || !strings.Contains(msg, "set_session_title") || !strings.Contains(msg, `"`+name+`"`)) {
				t.Errorf("sent %q to %q", msg, to)
			}
		})
	}
}

func TestPeerSocket(t *testing.T) {
	if got := peerSocket("/run/user/1000", 878093); got != "/run/user/1000/cc-socks/878093.sock" {
		t.Errorf("peerSocket = %q", got)
	}
}

// A reopen the desktop did not take ends the unit successfully and leaves
// its reason in the event log.
func TestAMissedReopenFailsNoUnit(t *testing.T) {
	store, err := state.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	a := &app{store: store, out: &out}
	if err := a.reopenMissed("#1 task", errors.New("the desktop recorded no title")); err != nil {
		t.Fatalf("a missed reopen fails its unit: %v", err)
	}
	evs, err := store.Events(0, func(e state.Event) bool { return e.Verb == "agent.reopen" })
	if err != nil || len(evs) != 1 || !strings.Contains(evs[0].Detail, "no title") || evs[0].By.Name != "#1 task" {
		t.Errorf("events %+v, %v", evs, err)
	}
	if !strings.Contains(out.String(), "reopen: missed") {
		t.Errorf("printed %q", out.String())
	}
}
