// Package watch is the watch: the Watcher and its loops, the poll of the
// sessions and the desk, the machine sample, the standby watch's relays,
// successions and recoveries, fired timers, usage, the reporter's turns,
// the doctor's and the disk's chores, and the central feed and mailbox.
//
// The command package holds the watch command and builds the Watcher, and
// is its Host. What this package still needs of the command crosses three
// seams, each named in one file:
//
//   - host.go: Host, the app's configuration, state, clock and output and
//     the app's own steps; and Cmd, the command package's functions.
//   - vocabulary.go: the command's names the watch writes and reads, kept
//     equal by the command's TestWatchVocabulary.
//   - the forwarders that say "runs in the command": steps whose bodies
//     need the command's own types stay there as functions over the
//     Watcher.
//
// A seam entry goes when what it names has moved to a package this one
// can import.
package watch
