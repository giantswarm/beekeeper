package proc

import "os"

// What the external tests (package proc_test) reach of this package: those
// that import a package which itself imports this one, such as diskprune,
// which a test of this package cannot import without a cycle.
var (
	UserScanAt       = userScanAt
	UserReferencesAt = userReferencesAt
	ReferenceFiles   = referenceFiles
	ReferencesAt     = referencesAt
	HeldAt           = held
	MappingFixture   = mappingFixture
)

// The process table's root and the files of a fixture process.
const (
	ProcRoot        = procRoot
	FixtureMappings = fixtureMappings
	FixtureStatus   = fixtureStatus
)

// ReferenceFilesReading is referenceFiles with readDir in place of its
// directory reads.
func ReferenceFilesReading(readDir func(string) ([]os.DirEntry, error)) referenceIO {
	files := referenceFiles()
	files.readDir = readDir
	return files
}
