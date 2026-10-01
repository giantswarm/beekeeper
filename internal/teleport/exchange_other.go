//go:build !linux

package teleport

import "os"

// exchange swaps the directories a and b: two renames, b missing between
// them for an instant, where the system has no atomic exchange.
func exchange(a, b string) error {
	tmp := a + ".swap"
	if err := os.Rename(b, tmp); err != nil {
		return err
	}
	if err := os.Rename(a, b); err != nil {
		_ = os.Rename(tmp, b)
		return err
	}
	return os.Rename(tmp, a)
}
