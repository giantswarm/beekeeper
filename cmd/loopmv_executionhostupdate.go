package cmd

import (
	"time"
)

// hostUpdateRetry paces a held host's or a busy host's next update attempt.
const hostUpdateRetry = time.Hour
