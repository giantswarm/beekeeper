//go:build !unix

package free

import "os"

// fileUID matches the caller off Unix, where os.Getuid is -1 too.
func fileUID(os.FileInfo) int { return -1 }
