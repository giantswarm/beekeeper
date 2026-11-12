package loop

import (
	"time"
)

// HostUpdateRetry paces a held host's or a busy host's next update attempt.
const HostUpdateRetry = time.Hour
