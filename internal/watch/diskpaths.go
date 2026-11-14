package watch

type DiskPath struct {
	Path    string `json:"path"`
	Bytes   int64  `json:"bytes"`
	Growth  *int64 `json:"growthBytes,omitempty"`
	Partial bool   `json:"partial,omitempty"`
}
