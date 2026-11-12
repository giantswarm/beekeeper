package cmd

import (
	"context"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/giantswarm/beekeeper/internal/github"
	"github.com/giantswarm/beekeeper/internal/superloop"
)

var ReadRedJobLog = func(ctx context.Context, repo string, job int64) (string, error) {
	return github.JobLog(ctx, github.RunGH, repo, job)
}

// Attach bounded failure output at the handoff, once per planned red wake.
// Non-Actions checks and unavailable logs retain their original check links.
func AttachRedLogs(ctx context.Context, act superloop.Action) superloop.Action {
	ref, ok := Cmd.ParseRef(act.Ref)
	if !ok {
		return act
	}
	jobs := regexp.MustCompile(`https://github.com/` + regexp.QuoteMeta(ref.Repo) + `/actions/runs/[0-9]+/job/([0-9]+)`)
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	seen := map[int64]bool{}
	for _, match := range jobs.FindAllStringSubmatch(act.Message, -1) {
		job, err := strconv.ParseInt(match[1], 10, 64)
		if err != nil || seen[job] {
			continue
		}
		if len(seen) == 2 {
			break
		}
		seen[job] = true
		log, err := ReadRedJobLog(ctx, ref.Repo, job)
		if err != nil {
			act.Message += "\nCI failure output unavailable; read " + match[0]
			continue
		}
		const limit = 12000
		if len(log) > limit {
			log = "[earlier output truncated]\n" + log[len(log)-limit:]
		}
		act.Message += "\n\nCI failure output from " + match[0] + ":\n" + strings.TrimSpace(log)
	}
	return act
}
