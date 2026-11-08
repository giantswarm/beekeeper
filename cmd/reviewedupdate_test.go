package cmd

import (
	"slices"
	"testing"

	"github.com/giantswarm/beekeeper/internal/superloop"
)

// mergeStateClean is GitHub's merge state of an up-to-date, mergeable branch.
const mergeStateClean = "CLEAN"

// The checks of the rig's reviewed head.
const behindBuild, behindUnit = "build", "unit"

// behindRig is a loop rig whose pull request, approved at loopHead, is
// behind its base. Record any unexpected branch update.
type behindRig struct {
	*loopRig
	updates []string
	pauses  int
}

func newBehindRig(t *testing.T) *behindRig {
	t.Helper()
	b := &behindRig{loopRig: newLoopRig(t)}
	r := b.loopRig
	r.pull.Base = mainBranch
	r.pull.MergeState = mergeStateBehind
	r.pull.Checks = []string{behindBuild, behindUnit}
	r.verdict("R1", loopHead, superloop.Approved, "")
	r.baseMerge = func(p superloop.Pull, approved string) (bool, error) {
		return approved == loopHead && p.Head == loopFixed, nil
	}
	merge := r.command
	r.command = func(argv []string) (string, int) {
		if slices.Contains(argv, "repos/"+loopMergeRepo+"/pulls/7/update-branch") {
			b.updates = append(b.updates, argv[len(argv)-1])
			r.pull.Head, r.pull.MergeState, r.pull.Green, r.pull.Checks = loopFixed, mergeStateClean, false, nil
			return "", 0
		}
		return merge(argv)
	}
	return b
}

// claim records the loop's exact-head merge claim of loopHead.
func (b *behindRig) claim(t *testing.T) {
	t.Helper()
	if err := b.a.store.Record(about(event(loopParty, loopClaimVerb, "[go %s@%s] %s", loopPR, loopHead, loopWorker), loopPR)); err != nil {
		t.Fatal(err)
	}
}

// merges are the gh pr merge commands the rig ran.
func (b *behindRig) merges() [][]string {
	var out [][]string
	for _, argv := range b.cmds {
		if len(argv) > 2 && argv[0] == "gh" && argv[1] == "pr" && argv[2] == superloop.KindMerge {
			out = append(out, argv)
		}
	}
	return out
}
