package notify

import (
	"bufio"
	"context"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/godbus/dbus/v5"
)

// notifications is the notification service on a private bus: it records
// the summaries it was sent.
type notifications struct {
	mu  sync.Mutex
	got []string
}

func (n *notifications) Notify(_ string, _ uint32, _, summary, _ string, _ []string, _ map[string]dbus.Variant, _ int32) (uint32, *dbus.Error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.got = append(n.got, summary)
	return uint32(len(n.got)), nil
}

func (n *notifications) summaries() []string {
	n.mu.Lock()
	defer n.mu.Unlock()
	return append([]string(nil), n.got...)
}

// privateBus starts a bus of the test's own with the notification service
// on it and returns its address; skipped without dbus-daemon.
func privateBus(t *testing.T) (string, *notifications) {
	t.Helper()
	bin, err := exec.LookPath("dbus-daemon")
	if err != nil {
		t.Skip("no dbus-daemon")
	}
	c := exec.Command(bin, "--session", "--nofork", "--print-address", "--address=unix:path="+filepath.Join(t.TempDir(), "bus")) //nolint:gosec // the test's own bus
	out, err := c.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Start(); err != nil {
		t.Skipf("dbus-daemon: %v", err)
	}
	t.Cleanup(func() { _ = c.Process.Kill(); _ = c.Wait() })
	addr, err := bufio.NewReader(out).ReadString('\n')
	if err != nil {
		t.Fatalf("dbus-daemon printed no address: %v", err)
	}
	addr = strings.TrimSpace(addr)
	srv, err := dbus.Connect(addr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = srv.Close() })
	n := &notifications{}
	if err := srv.Export(n, object, service); err != nil {
		t.Fatal(err)
	}
	if r, err := srv.RequestName(service, dbus.NameFlagDoNotQueue); err != nil || r != dbus.RequestNameReplyPrimaryOwner {
		t.Fatalf("owning %s: %v, %v", service, r, err)
	}
	return addr, n
}

// The connection outlives the send that opened it: the next notification,
// after the first send's context ended, reaches the desktop over it; one the
// bus closed underneath is replaced within the send.
func TestDesktopKeepsItsConnection(t *testing.T) {
	addr, n := privateBus(t)
	t.Setenv("DBUS_SESSION_BUS_ADDRESS", addr)
	d := &Desktop{}
	t.Cleanup(func() { _ = d.Close() })
	send := func(summary string) {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), sendTimeout)
		defer cancel()
		if _, err := d.Send(ctx, Message{Summary: summary, Urgency: Normal}); err != nil {
			t.Fatalf("sending %q: %v", summary, err)
		}
	}
	send("first")
	first := d.conn
	// godbus closes a connection whose context ended in a goroutine of its own.
	time.Sleep(100 * time.Millisecond)
	send("second")
	if d.conn != first {
		t.Error("the second send opened a new connection: the first one was closed with its send")
	}
	_ = d.conn.Close()
	send("third")
	if got := n.summaries(); strings.Join(got, ",") != "first,second,third" {
		t.Errorf("the desktop got %q, want first, second and third", got)
	}
}
