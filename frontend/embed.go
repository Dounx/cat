// Package frontend embeds the built web UI (run `pnpm build` in this directory).
package frontend

import "embed"

//go:embed all:dist
var Dist embed.FS
