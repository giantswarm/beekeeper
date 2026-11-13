// Package gate is the merge and promotion gate: lane admission, the queue,
// overlap and pair checks, takeover and preemption of a running child,
// urgent cut-ins, child runs and release confirmation, and central lane
// turns.
//
// The command package holds the CLI commands, the detached child entrypoint
// and the lane views, and is its Host. What this package still needs of the
// command crosses two seams, each named in one file:
//
//   - host.go: Host, the app's configuration, state, clock and output and
//     the app's own steps; and Cmd, the command package's functions.
//   - vocabulary.go: the command's names and exit codes the gate writes and
//     reads, kept equal by the command's TestGateVocabulary.
//
// A seam entry goes when what it names has moved to a package this one
// can import.
package gate
