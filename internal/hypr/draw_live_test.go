//go:build hyprlive

package hypr

// The live proof that a window on the off-screen output keeps drawing for a
// whole turn: a scratch-profile Chrome of its own class sits on the output
// while its page navigates now and then, and the page reports how many
// animation frames it got each half second. Run it as
//
//	BEEKEEPER_LIVE_DIR=<scratch dir> go test -tags hyprlive -run LiveDraws -v ./internal/hypr
//
// BEEKEEPER_LIVE_TURNS (default 10) turns of BEEKEEPER_LIVE_TURN (default
// 40s) each; it writes the frame timeline to the scratch dir.

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// drawPage reports its animation frames to /f every 500ms and navigates to
// the next page every 8s.
const drawPage = `<!doctype html><title>beekeeper draw proof %[1]d</title>
<body style="font:48px sans-serif">page %[1]d <span id=t></span><script>
let n = 0; (function f() { n++; requestAnimationFrame(f) })();
setInterval(() => { if (location.hash == '#paint') document.getElementById('t').textContent = n;
  fetch('/f?p=%[1]d&n=' + n + '&v=' + document.visibilityState) }, 500);
setTimeout(() => { location.href = '/p?n=%[2]d' }, 8000);
</script>`

// hiddenNumber is the workspace the scratch Chrome waits on between turns.
const hiddenNumber = "77"

type frameSample struct {
	at   time.Time
	page int
	n    int
	vis  string
}

func TestLiveDrawsOnTheOutput(t *testing.T) {
	dir := os.Getenv("BEEKEEPER_LIVE_DIR")
	if !Running() || dir == "" {
		t.Skip("needs Hyprland and BEEKEEPER_LIVE_DIR")
	}
	turns, turn := 10, 40*time.Second
	if v, err := strconv.Atoi(os.Getenv("BEEKEEPER_LIVE_TURNS")); err == nil {
		turns = v
	}
	if v, err := time.ParseDuration(os.Getenv("BEEKEEPER_LIVE_TURN")); err == nil {
		turn = v
	}
	ctx := context.Background()
	classes := browserClasses
	browserClasses = []string{liveClass}
	t.Cleanup(func() { browserClasses = classes })

	var (
		mu      sync.Mutex
		samples []frameSample
	)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/p", func(w http.ResponseWriter, r *http.Request) {
		n, _ := strconv.Atoi(r.URL.Query().Get("n"))
		_, _ = fmt.Fprintf(w, drawPage, n, n+1)
	})
	mux.HandleFunc("/f", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		p, _ := strconv.Atoi(q.Get("p"))
		n, _ := strconv.Atoi(q.Get("n"))
		mu.Lock()
		samples = append(samples, frameSample{time.Now(), p, n, q.Get("v")})
		mu.Unlock()
	})
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	url := "http://" + ln.Addr().String() + "/p?n=0"

	chrome := []string{"google-chrome-stable", "--user-data-dir=" + filepath.Join(dir, "profile"), "--class=" + liveClass,
		"--no-first-run", "--no-default-browser-check", "--password-store=basic", url}
	// A numbered workspace no monitor shows: a named one could take the
	// number the output's workspace had, which the compositor shows on the
	// output again when it is created.
	launch := fmt.Sprintf("hl.exec_cmd(%q, { workspace = %q, no_initial_focus = true })", strings.Join(chrome, " "), hiddenNumber+" silent")
	if err := run(ctx, Hyprctl, "eval", launch); err != nil {
		t.Fatal(err)
	}
	main := waitWindows(t, ctx, 1)[0]
	before := snapshot(t, ctx)
	t.Cleanup(func() {
		if p, err := os.FindProcess(main.Pid); err == nil {
			_ = p.Signal(syscall.SIGTERM)
		}
	})

	var log []string
	failed := 0
	for i := range turns {
		s, err := Show(ctx, Hyprctl, 2*turn)
		if err != nil || s == nil || !s.Moved {
			t.Fatalf("turn %d: Show = %+v, %v", i, s, err)
		}
		start := time.Now()
		during := snapshot(t, ctx)
		time.Sleep(turn)
		if err := s.Restore(ctx, Hyprctl); err != nil {
			t.Fatalf("turn %d: Restore = %v", i, err)
		}
		after := snapshot(t, ctx)
		if !during.same(before) || !after.same(before) {
			t.Errorf("turn %d: the person's view changed: during %+v, after %+v, before %+v", i, during, after, before)
		}
		// A stall is a second of the turn, past its first, in which the page
		// got no animation frame while it said it was visible.
		mu.Lock()
		var in []frameSample
		for _, f := range samples {
			if f.at.After(start) && f.at.Before(start.Add(turn)) {
				in = append(in, f)
			}
		}
		mu.Unlock()
		stalls := 0
		for j := 1; j < len(in); j++ {
			a, b := in[j-1], in[j]
			if a.page == b.page && b.n == a.n && b.vis == "visible" && b.at.Sub(start) > 2*time.Second {
				stalls++
			}
		}
		line := fmt.Sprintf("turn %d: %d samples, %d stalled half-seconds", i, len(in), stalls)
		if stalls > 0 || len(in) == 0 {
			failed++
			line += " NOT DRAWN"
		}
		log = append(log, line)
		t.Log(line)
		time.Sleep(3 * time.Second) // the window back on its hidden workspace between turns
	}
	mu.Lock()
	var tl []string
	for _, f := range samples {
		tl = append(tl, fmt.Sprintf("%s page %d frames %d %s", f.at.Format("15:04:05.000"), f.page, f.n, f.vis))
	}
	mu.Unlock()
	_ = os.WriteFile(filepath.Join(dir, "draw-timeline.txt"), []byte(strings.Join(append(log, tl...), "\n")+"\n"), 0o600) //nolint:gosec // the proof's scratch dir
	if failed > 0 {
		t.Errorf("%d of %d turns not drawn throughout", failed, turns)
	}
}
