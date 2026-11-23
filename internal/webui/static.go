package webui

import "embed"

// Files contains the production Vue bundle.
//
//go:embed dist
var Files embed.FS
