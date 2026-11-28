// Package view embeds the self-contained dashboard page so the collector can
// serve it without a second service, static host, or runtime file dependency.
package view

import _ "embed"

// HTML is the complete dashboard document.
//
//go:embed index.html
var HTML string
