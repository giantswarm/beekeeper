// Package capacity is what a start is judged by: the views beekeeper
// capacity prints and writes as JSON, the usage data model they are derived
// from, and the math from the roster's counts, the usage readings and the
// machine's headroom to a verdict and a choice.
//
// The command package counts the roster, reads usage and headroom, and
// holds the commands and the start gate. What this package still needs of
// the command crosses two seams, each named in one file:
//
//   - command.go: Cmd, the command package's text helpers the views' lines
//     are written with.
//   - vocabulary.go: the command's names the views write and compare,
//     kept equal by the command's TestCapacityVocabulary.
//
// A seam entry goes when what it names has moved to a package this one
// can import.
package capacity
