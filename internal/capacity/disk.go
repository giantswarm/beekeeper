package capacity

// WatchDiskReading is the watch's latest reading of the disk the desk
// works on.
type WatchDiskReading struct {
	Low bool
	// Filling says / would run full within watch.diskFillWithin at its
	// recent rate. It is a forecast: it triggers an early disk step and
	// is said in the watch, but it never holds starts on its own — only
	// Low (free space under the floor) does.
	Filling bool
	Writers string
	FreeMiB int
	MinMiB  int
}
