package frontend

import "embed"

// Dist contains the compiled management interface.
//
//go:embed dist
var Dist embed.FS
