// Package loop is the supervisor loop: the pass that reads the desk's events
// and pull requests, plans with superloop and takes each transition once,
// the ordered streams its actions run in, the refill that starts the next
// board items, and the alarms on what stalls.
//
// The command package runs it (beekeeper supervisor loop) and is its Host.
// What the loop still needs of the command crosses three seams, each named
// in one file:
//
//   - host.go: Host, the app's configuration, state, clock and output and
//     the app's own steps; and Cmd, the command package's functions.
//   - vocabulary.go: the command's names, flags, verbs and exit codes the
//     loop writes and reads, kept equal by the command's TestLoopVocabulary.
//   - the forwarders that say "runs in the command": steps whose bodies
//     need the command's own types (capacity views, timers, the roster's
//     chores) stay there as functions over the Loop.
//
// A seam entry goes when what it names has moved to a package the loop can
// import.
package loop
