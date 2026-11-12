//go:build scenario && linux

package harness

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
)

// The builder of a scenario is the desk's run.remote without a cluster:
//
//	/work/builder/bin         the image's tool directory: the system's shell
//	                          and coreutils, and the fake go and golangci-lint
//	                          the scenario names
//	/work/builder/kubeconfig  the kubeconfig the desk's run.remote names
//	/work/runs, /work/src, …  the pod's /work, where the desk's own scripts
//	                          keep a run's source, journal and exit code
//
// The fake kubectl keeps the Jobs the desk creates in the world and answers
// get, create, wait and delete from them. kubectl exec runs its command in
// place, under the Job's container environment and the image's PATH: the
// desk's upload, launch and follow scripts run as they are. A pod's /work is
// the sandbox's, so one builder runs at a time; deleting its Job empties it.

const opKube = "kube"

// builderBase is what every builder image carries (docs/remote-builds.md,
// "Builder image contract"): the first are required, the rest come along
// where the system has them.
var (
	builderRequired = []string{"bash", "tar", "flock", "setsid", "nohup", "dd", "stat", "cat", "mkdir", "rm", "mv", "touch", "sleep", "date"}
	builderOptional = []string{"sh", "env", "cp", "ln", "ls", "chmod", "head", "tail", "tr", "cut", "sed", "grep", "wc", "sort", "basename", "dirname", "mktemp", "true", "false", "timeout", "id"}
)

// podScratch is what a builder pod keeps under its /work.
var podScratch = []string{"runs", "src", "tmp", "tools", "busy", "active", "control.lock", "go-cache", "go-mod", "target", "cargo"}

// ProvisionBuilder builds the image's tool directory and returns the
// desk's run.remote.
func ProvisionBuilder(b *Builder, fake string) (map[string]any, error) {
	bin := filepath.Join(BuilderRoot, "bin")
	for _, dir := range []string{bin, filepath.Join(BuilderRoot, "home")} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, err
		}
	}
	for _, tool := range append(slices.Clone(builderRequired), builderOptional...) {
		p, err := exec.LookPath(tool)
		if err != nil {
			if slices.Contains(builderRequired, tool) {
				return nil, fmt.Errorf("the builder image needs %s, which this system lacks: %w", tool, err)
			}
			continue
		}
		if err := os.Symlink(p, filepath.Join(bin, tool)); err != nil {
			return nil, err
		}
	}
	for _, tool := range b.Tools {
		if err := os.Symlink(fake, filepath.Join(bin, tool)); err != nil {
			return nil, err
		}
	}
	kubeconfig := filepath.Join(BuilderRoot, "kubeconfig")
	if err := os.WriteFile(kubeconfig, []byte("# the scenario's builder cluster: the fake kubectl answers for it\n"), 0o600); err != nil {
		return nil, err
	}
	return map[string]any{
		"enabled": true, "kubeconfig": kubeconfig, "context": BuilderContext, "namespace": BuilderNamespace,
		"images": map[string]any{"go": BuilderImage},
	}, nil
}

// kubeArgs are kubectl's arguments without the flags that name the cluster.
func kubeArgs(args []string) []string {
	var out []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			return append(out, args[i:]...)
		}
		if slices.Contains([]string{"--kubeconfig", "--context", "--namespace", "-n"}, a) {
			i++
			continue
		}
		if strings.HasPrefix(a, "--kubeconfig=") || strings.HasPrefix(a, "--context=") || strings.HasPrefix(a, "--namespace=") || strings.HasPrefix(a, "--request-timeout") {
			continue
		}
		out = append(out, a)
	}
	return out
}

// The fake kubectl: the verbs the desk's remote builds use.
func kubectl(c *client) int {
	args := kubeArgs(c.args)
	unknown := func() int {
		fmt.Fprintf(os.Stderr, "error: the scenario's kubectl does not model: kubectl %s\n", strings.Join(args, " "))
		return 1
	}
	if len(args) == 0 {
		return unknown()
	}
	verb, rest := args[0], args[1:]
	switch verb {
	case "get":
		if len(rest) == 0 {
			return unknown()
		}
		switch rest[0] {
		case "job":
			if len(rest) < 2 {
				return unknown()
			}
			return c.end(c.ask(request{Op: opKube, Verb: "get-job", Names: []string{rest[1]}}))
		case "pod":
			if len(rest) < 2 {
				return unknown()
			}
			return c.end(c.ask(request{Op: opKube, Verb: "get-pod", Names: []string{rest[1]}}))
		case "pods":
			var names []string
			for i, a := range rest {
				if a == "-l" && i+1 < len(rest) {
					if job, ok := strings.CutPrefix(rest[i+1], "job-name="); ok {
						names = []string{job}
					}
				}
			}
			return c.end(c.ask(request{Op: opKube, Verb: "get-pods", Names: names}))
		}
	case "create":
		raw, _ := io.ReadAll(io.LimitReader(os.Stdin, 16<<20))
		return c.end(c.ask(request{Op: opKube, Verb: "create", Stdin: string(raw)}))
	case "wait":
		for _, a := range rest {
			if job, ok := strings.CutPrefix(a, "job/"); ok {
				return c.end(c.ask(request{Op: opKube, Verb: "wait", Names: []string{job}}))
			}
		}
	case "delete":
		if len(rest) >= 2 && rest[0] == "job" {
			return c.end(c.ask(request{Op: opKube, Verb: "delete", Names: []string{rest[1]}}))
		}
	case "exec":
		i := slices.Index(rest, "--")
		if len(rest) == 0 || i < 1 || i+1 >= len(rest) {
			return unknown()
		}
		argv := rest[i+1:]
		rep, err := c.ask(request{Op: opKube, Verb: "exec", Names: []string{rest[0]}})
		if err != nil || rep.SSH == nil {
			return c.end(rep, err)
		}
		bin, err := lookPath(argv[0], rep.SSH.Env)
		if err != nil {
			// The container runtime's words for a command the image lacks.
			fmt.Fprintf(os.Stderr, "OCI runtime exec failed: exec failed: unable to start container process: exec: %q: executable file not found in $PATH: unknown\ncommand terminated with exit code 126\n", argv[0])
			return 126
		}
		if err := os.Chdir(rep.SSH.Home); err != nil {
			fmt.Fprintf(os.Stderr, "kubectl: %v\n", err)
			return 1
		}
		if err := syscall.Exec(bin, argv, rep.SSH.Env); err != nil {
			fmt.Fprintf(os.Stderr, "kubectl: %v\n", err)
		}
		return 1
	}
	return unknown()
}

// kubeJob is what the world reads of a Job the desk creates.
type kubeJob struct {
	Metadata struct {
		Name string `json:"name"`
	} `json:"metadata"`
	Spec struct {
		Template struct {
			Spec struct {
				Containers []struct {
					Env []struct {
						Name  string `json:"name"`
						Value string `json:"value"`
					} `json:"env"`
				} `json:"containers"`
			} `json:"spec"`
		} `json:"template"`
	} `json:"spec"`
}

func podName(job string) string { return job + "-pod" }

func podJSON(job string) map[string]any {
	return map[string]any{
		"apiVersion": "v1", "kind": "Pod",
		"metadata": map[string]any{"name": podName(job), "namespace": BuilderNamespace, "labels": map[string]any{"job-name": job}},
		"status":   map[string]any{"phase": "Running"},
	}
}

// kube answers the fake kubectl from the Jobs the desk created.
func (w *World) kube(req request) reply {
	if w.opts.Builder == nil {
		return reply{Exit: 1, Message: "error: the scenario has no given.builder: no cluster answers kubectl"}
	}
	name := firstOf(req.Names)
	notFound := func(kind, name string) reply {
		return reply{Exit: 1, Message: fmt.Sprintf("Error from server (NotFound): %s %q not found", kind, name)}
	}
	marshal := func(v any) reply {
		raw, err := json.Marshal(v)
		if err != nil {
			return reply{Error: err.Error()}
		}
		return reply{Message: string(raw)}
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	switch req.Verb {
	case "get-job":
		// --ignore-not-found: nothing, and exit 0.
		return reply{Message: w.jobs[name]}
	case "get-pods":
		items := []any{}
		for _, job := range w.jobOrder {
			if name == "" || name == job {
				items = append(items, podJSON(job))
			}
		}
		return marshal(map[string]any{"apiVersion": "v1", "kind": "PodList", "items": items})
	case "get-pod":
		for _, job := range w.jobOrder {
			if podName(job) == name {
				return marshal(podJSON(job))
			}
		}
		return notFound("pods", name)
	case "create":
		var job kubeJob
		if err := json.Unmarshal([]byte(req.Stdin), &job); err != nil || job.Metadata.Name == "" {
			return reply{Exit: 1, Message: "error: the scenario's kubectl creates a Job from standard input"}
		}
		if _, ok := w.jobs[job.Metadata.Name]; ok {
			return reply{Exit: 1, Message: fmt.Sprintf("Error from server (AlreadyExists): jobs.batch %q already exists", job.Metadata.Name)}
		}
		// What the Job's watchdog does as its pod starts.
		for _, dir := range []string{"runs", "src", "tmp"} {
			if err := os.MkdirAll(filepath.Join("/work", dir), 0o700); err != nil {
				return reply{Error: err.Error()}
			}
		}
		if err := os.WriteFile("/work/active", nil, 0o600); err != nil {
			return reply{Error: err.Error()}
		}
		if w.jobs == nil {
			w.jobs = map[string]string{}
		}
		w.jobs[job.Metadata.Name] = req.Stdin
		w.jobOrder = append(w.jobOrder, job.Metadata.Name)
		return reply{Message: fmt.Sprintf("job.batch/%s created", job.Metadata.Name)}
	case "wait":
		if _, ok := w.jobs[name]; !ok {
			return notFound("jobs.batch", name)
		}
		return reply{Message: fmt.Sprintf("job.batch/%s condition met", name)}
	case "delete":
		if _, ok := w.jobs[name]; !ok {
			return reply{}
		}
		delete(w.jobs, name)
		w.jobOrder = slices.DeleteFunc(w.jobOrder, func(j string) bool { return j == name })
		// The pod and its emptyDir go with the Job.
		for _, entry := range podScratch {
			_ = os.RemoveAll(filepath.Join("/work", entry))
		}
		return reply{Message: fmt.Sprintf("job.batch %q deleted", name)}
	case "exec":
		for _, jobName := range w.jobOrder {
			if podName(jobName) != name {
				continue
			}
			var job kubeJob
			_ = json.Unmarshal([]byte(w.jobs[jobName]), &job)
			home := filepath.Join(BuilderRoot, "home")
			env := []string{"PATH=" + filepath.Join(BuilderRoot, "bin"), "HOME=" + home, "LANG=C.UTF-8", builderEnv + "=1"}
			if cs := job.Spec.Template.Spec.Containers; len(cs) > 0 {
				for _, e := range cs[0].Env {
					env = append(env, e.Name+"="+e.Value)
				}
			}
			return reply{SSH: &sshRoute{Root: BuilderRoot, Home: home, Env: env}}
		}
		return notFound("pods", name)
	}
	return reply{Error: "unknown kubectl verb " + req.Verb}
}

// inBuilder says this process runs in the builder's pod.
func inBuilder() bool { return os.Getenv(builderEnv) != "" }

// The image's fake go. go install puts the fake of the tool it names into
// GOBIN, as the real one puts the tool there; every build, vet and test
// passes unless a given call of the scenario answers it.
func builderGo(c *client) int {
	if len(c.args) >= 2 && c.args[0] == "install" {
		pkg, _, _ := strings.Cut(c.args[len(c.args)-1], "@")
		tool := path.Base(pkg)
		gobin := os.Getenv("GOBIN")
		self, err := os.Executable()
		if gobin == "" || err != nil || !slices.Contains(BuilderTools, tool) {
			fmt.Fprintf(os.Stderr, "go: the scenario's go installs only %s, into GOBIN\n", strings.Join(BuilderTools, " and "))
			return 1
		}
		if err := os.MkdirAll(gobin, 0o700); err != nil {
			fmt.Fprintf(os.Stderr, "go: %v\n", err)
			return 1
		}
		link := filepath.Join(gobin, tool)
		_ = os.Remove(link)
		if err := os.Symlink(self, link); err != nil {
			fmt.Fprintf(os.Stderr, "go: %v\n", err)
			return 1
		}
		return 0
	}
	if len(c.args) == 1 && c.args[0] == "version" {
		fmt.Printf("go version go%s linux/amd64\n", NewGo)
	}
	return 0
}

// The image's, or an installed, fake golangci-lint: no findings.
func builderLint(_ *client) int {
	fmt.Println("0 issues.")
	return 0
}
