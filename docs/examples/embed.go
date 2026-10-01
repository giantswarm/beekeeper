// Package examples carries the documented example configuration, from which
// beekeeper install writes a starter config.
package examples

import _ "embed"

// Config is config.yaml, a complete desk's configuration.
//
//go:embed config.yaml
var Config string
