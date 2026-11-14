package cmd

import (
	"fmt"
)

// gibStr is a byte count in GiB with one decimal.
func gibStr(b int64) string { return fmt.Sprintf("%.1f GiB", float64(b)/(1<<30)) }
