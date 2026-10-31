package remotebuild

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/gofrs/flock"
	"github.com/google/uuid"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
)

// Command is the kubectl transport. Tests replace it without a live cluster.
type Command func(context.Context, io.Reader, io.Writer, ...string) error

type Runner struct {
	Config         Config
	StateDir       string
	Stdout, Stderr io.Writer
	Command        Command
}

func (r Runner) kubectl(ctx context.Context, in io.Reader, out io.Writer, args ...string) error {
	base := []string{"--kubeconfig", r.Config.Kubeconfig, "--context", r.Config.Context, "--namespace", r.Config.Namespace, "--request-timeout=15s"}
	// Placement/stream exec lifetime is controlled by context, not an HTTP timeout.
	if len(args) > 0 && (args[0] == "exec" || args[0] == "wait") {
		base[len(base)-1] = "--request-timeout=0"
	}
	args = append(base, args...)
	if r.Command != nil {
		return r.Command(ctx, in, out, args...)
	}
	c := exec.CommandContext(ctx, "kubectl", args...) //nolint:gosec // fixed executable and typed arguments; test commands use owned fixtures
	c.Stdin, c.Stdout, c.Stderr = in, out, io.Discard
	return c.Run()
}

// Try returns handled=false only before any command could have started remotely.
// Every such failure emits one line and leaves the ordinary local slot unchanged.
func (r Runner) Try(ctx context.Context, argv []string) (handled bool, code int) {
	if !r.Config.Enabled {
		return false, 0
	}
	tool := Toolchain(argv)
	if tool == "" {
		return false, 0
	}
	if r.Stdout == nil {
		r.Stdout = os.Stdout
	}
	if r.Stderr == nil {
		r.Stderr = os.Stderr
	}
	fallback := func(reason string) (bool, int) {
		if ctx.Err() != nil {
			return true, 130
		}
		_, _ = fmt.Fprintf(r.Stderr, "beekeeper run: remote unavailable (%s); using local slot\n", reason)
		return false, 0
	}
	if err := r.Config.Validate(); err != nil {
		return fallback("invalid configuration")
	}
	r.Config = r.Config.Defaults()
	image := r.Config.Images[tool]
	if image == "" {
		return fallback("no image for toolchain")
	}
	if os.Getenv("GOFLAGS") != "" || os.Getenv("RUSTFLAGS") != "" || os.Getenv("CARGO_ENCODED_RUSTFLAGS") != "" {
		return fallback("caller toolchain flags require local environment")
	}
	rootBytes, err := exec.CommandContext(ctx, "git", "rev-parse", "--show-toplevel").Output() //nolint:gosec // fixed executable and typed arguments; test commands use owned fixtures
	if err != nil {
		return fallback("not a git worktree")
	}
	root := strings.TrimSpace(string(rootBytes))
	cwd, err := os.Getwd()
	if err != nil {
		return fallback("working directory unavailable")
	}
	sub, err := filepath.Rel(root, cwd)
	if err != nil || sub == ".." || strings.HasPrefix(sub, "../") {
		return fallback("working directory outside worktree")
	}
	host, err := os.Hostname()
	if err != nil {
		return fallback("host identity unavailable")
	}
	sum := sha256.Sum256([]byte(host + "\x00" + root))
	name := "bkb-" + hex.EncodeToString(sum[:12])
	dir := filepath.Join(r.StateDir, "remote-build")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fallback("cannot create local run journal")
	}
	lock := flock.New(filepath.Join(dir, name+".lock"))
	locked, err := lock.TryLock()
	if err != nil || !locked {
		return fallback("worktree already has a remote run")
	}
	defer func() { _ = lock.Unlock() }()
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()
	setup, cancel := context.WithTimeout(ctx, r.Config.PlacementTimeout)
	defer cancel()
	pod, err := r.ensurePod(setup, name, image)
	if err != nil {
		if ctx.Err() != nil {
			return true, 130
		}
		return fallback(err.Error())
	}
	src, err := archive(setup, root, dir)
	if err != nil {
		return fallback("cannot snapshot worktree")
	}
	defer func() { _ = src.Close(); _ = os.Remove(src.Name()) }()
	id := uuid.NewString()
	cleanup := func(script string) error {
		cleanupCtx, done := context.WithTimeout(context.Background(), 15*time.Second)
		defer done()
		return r.exec(cleanupCtx, pod, nil, io.Discard, shellBash, "-c", script, "--", id)
	}
	if err := r.exec(setup, pod, src, io.Discard, shellBash, "-c", upload, "--", id); err != nil {
		_ = cleanup(interrupt)
		return fallback("source upload failed or worktree busy")
	}
	// A launch reply may be lost after the command starts; never retry locally.
	defer func() {
		if ctx.Err() != nil {
			if err := cleanup(interrupt); err != nil {
				_, _ = fmt.Fprintln(r.Stderr, "beekeeper run: remote interrupt could not be confirmed; inspect builder Job")
			}
		}
	}()
	launchCtx, launchCancel := context.WithTimeout(ctx, 15*time.Second)
	defer launchCancel()
	if err := r.exec(launchCtx, pod, nil, io.Discard, shellBash, "-c", start, "--", id, sub, shellCommand(argv), wrapper); err != nil {
		// A lost launch reply is recoverable: follow waits for the run's journal.
		_, _ = fmt.Fprintln(r.Stderr, "beekeeper run: reconnecting to remote run journal")
	}
	count := &countWriter{w: r.Stdout}
	for attempt := 0; attempt < 4; attempt++ {
		err = r.exec(ctx, pod, nil, count, shellBash, "-c", follow, "--", id, strconv.FormatInt(count.n, 10))
		if ctx.Err() != nil {
			return true, 130
		}
		if err == nil {
			var status bytes.Buffer
			if r.exec(ctx, pod, nil, &status, "cat", "/work/runs/"+id+"/rc") == nil {
				rc, e := strconv.Atoi(strings.TrimSpace(status.String()))
				if e == nil && rc >= 0 && rc <= 255 {
					_ = cleanup(finish)
					return true, rc
				}
			}
		}
	}
	_ = cleanup(interrupt)
	_, _ = fmt.Fprintln(r.Stderr, "beekeeper run: remote journal unavailable; command was not retried locally")
	return true, 125
}

func (r Runner) exec(ctx context.Context, pod string, in io.Reader, out io.Writer, args ...string) error {
	flags := []string{"exec", pod, "-c", "builder"}
	if in != nil {
		flags = append(flags, "-i")
	}
	flags = append(flags, "--")
	return r.kubectl(ctx, in, out, append(flags, args...)...)
}

func (r Runner) ensurePod(ctx context.Context, name, image string) (podName string, resultErr error) {
	var b bytes.Buffer
	if err := r.kubectl(ctx, nil, &b, "get", "job", name, "--ignore-not-found", "-o", "json"); err != nil {
		return "", errors.New("cluster unreachable")
	}
	if b.Len() > 0 {
		var j batchv1.Job
		if json.Unmarshal(b.Bytes(), &j) != nil {
			return "", errors.New("invalid builder response")
		}
		if j.Labels["app.kubernetes.io/name"] != appLabel || len(j.Spec.Template.Spec.Containers) != 1 || j.Spec.Template.Spec.Containers[0].Image != image || j.Annotations["beekeeper.dev/builder-spec"] != Job(r.Config, name, image).Annotations["beekeeper.dev/builder-spec"] {
			return "", errors.New("existing builder differs; remove idle Job before changing configuration")
		}
		if j.Status.Succeeded > 0 || j.Status.Failed > 0 || j.DeletionTimestamp != nil {
			return "", errors.New("builder retiring; waiting for TTL cleanup")
		}
	} else {
		b.Reset()
		if err := r.kubectl(ctx, nil, &b, "get", "pods", "-o", "json"); err != nil {
			return "", errors.New("cluster unreachable")
		}
		var pods corev1.PodList
		if json.Unmarshal(b.Bytes(), &pods) != nil {
			return "", errors.New("invalid capacity response")
		}
		active := 0
		for _, p := range pods.Items {
			if p.Status.Phase != corev1.PodSucceeded && p.Status.Phase != corev1.PodFailed {
				active++
			}
		}
		if active >= r.Config.MaxPods {
			return "", errors.New("cluster full")
		}
		raw, err := json.Marshal(Job(r.Config, name, image))
		if err != nil {
			return "", err
		}
		if err = r.kubectl(ctx, bytes.NewReader(raw), io.Discard, "create", "-f", "-"); err != nil {
			return "", errors.New("builder admission failed (quota or concurrent creation)")
		}
		// Only remove a Job created by this attempt if it cannot place in time.
		defer func() {
			if resultErr != nil {
				clean, cancel := context.WithTimeout(context.Background(), 15*time.Second)
				defer cancel()
				_ = r.kubectl(clean, nil, io.Discard, "delete", "job", name, "--wait=false")
			}
		}()
	}
	wait := "--timeout=" + r.Config.PlacementTimeout.String()
	if err := r.kubectl(ctx, nil, io.Discard, "wait", "--for=jsonpath={.status.ready}=1", "job/"+name, wait); err != nil {
		return "", errors.New("builder not ready within placement bound")
	}
	b.Reset()
	if err := r.kubectl(ctx, nil, &b, "get", "pods", "-l", "job-name="+name, "-o", "json"); err != nil {
		return "", errors.New("builder pod unavailable")
	}
	var pods corev1.PodList
	if json.Unmarshal(b.Bytes(), &pods) != nil {
		return "", errors.New("invalid builder response")
	}
	for _, p := range pods.Items {
		if p.DeletionTimestamp == nil && p.Status.Phase == corev1.PodRunning {
			return p.Name, nil
		}
	}
	return "", errors.New("builder pod unavailable")
}

type countWriter struct {
	w io.Writer
	n int64
}

func (w *countWriter) Write(p []byte) (int, error) {
	n, err := w.w.Write(p)
	w.n += int64(n)
	return n, err
}
