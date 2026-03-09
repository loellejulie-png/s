package web

import "embed"

// EmbeddedFiles contains all web static assets and templates
//
//go:embed index.html static
var EmbeddedFiles embed.FS
