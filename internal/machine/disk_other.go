//go:build !unix

package machine

import "errors"

// Disk is a filesystem's usage in MiB.
type Disk struct {
	Path    string `json:"path"`
	UsedMiB int    `json:"usedMiB"`
	FreeMiB int    `json:"freeMiB"`
	// TotalMiB is the filesystem's size.
	TotalMiB int `json:"totalMiB"`
}

// ReadDisk is not implemented off Unix.
func ReadDisk(path string) (Disk, error) {
	return Disk{Path: path}, errors.New("disk usage needs a Unix system")
}
