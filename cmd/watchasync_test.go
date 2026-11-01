package cmd

import (
	"context"
	"sync"
	"testing"
	"time"
)

func TestWatchWaitsForAsyncDelivery(t *testing.T) {
	w, _ := onceWatch(t, nil)
	started, release, delivered := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var once sync.Once
	finish := func() { once.Do(func() { close(release) }) }
	defer finish()
	w.stand.send = func(context.Context, string, string) error { close(started); <-release; return nil }
	if !w.sendAsync(context.Background(), "fixture-recipient", "resume", func(error) { close(delivered) }) {
		t.Fatal("delivery did not start")
	}
	<-started
	waited := make(chan struct{})
	go func() { w.stand.inflight.Wait(); close(waited) }()
	select {
	case <-waited:
		finish()
		<-delivered
		t.Fatal("watch shutdown completed while message delivery still ran")
	case <-time.After(20 * time.Millisecond):
	}
	finish()
	select {
	case <-waited:
	case <-time.After(time.Second):
		t.Fatal("completed message delivery held watch shutdown")
	}
}
