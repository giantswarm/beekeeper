package sandbox

import (
	"net"
	"testing"
	"time"
)

func TestSameUser(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	go func() {
		c, err := net.Dial("tcp", ln.Addr().String())
		if err == nil {
			time.Sleep(time.Second)
			_ = c.Close()
		}
	}()
	c, err := ln.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	if err := SameUser("/proc")(c); err != nil {
		t.Errorf("a connection of this user: %v", err)
	}
	if err := SameUser(t.TempDir())(c); err == nil {
		t.Error("no socket table: served")
	}
}
