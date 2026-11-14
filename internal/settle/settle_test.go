package settle_test

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/settle"
)

// Something that finishes while the probes are asked one after another (work moving from a probe not
// yet asked to one already asked) makes that scan worthless: Wait scans again.
func TestWaitRescansWhenWorkFinishesDuringAScan(t *testing.T) {
	t.Parallel()
	tr := settle.New()
	var calls atomic.Int32
	tr.AddProbe("p", func() (bool, string) {
		if calls.Add(1) == 1 {
			tr.Poke() // a unit finished while this scan ran
		}
		return true, ""
	})
	if err := tr.Wait(context.Background(), time.Second); err != nil {
		t.Fatal(err)
	}
	if n := calls.Load(); n < 3 {
		t.Fatalf("probe asked %d times: Wait trusted a scan during which work finished", n)
	}
}

func TestWaitWaitsForBegunWork(t *testing.T) {
	t.Parallel()
	tr := settle.New()
	done := tr.Begin("unit")
	if err := tr.Wait(context.Background(), 5*time.Millisecond); err == nil {
		t.Fatal("settled with a unit in progress")
	}
	go func() {
		time.Sleep(time.Millisecond)
		done()
	}()
	if err := tr.Wait(context.Background(), time.Second); err != nil {
		t.Fatal(err)
	}
}
