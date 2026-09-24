// Package web holds the pages served to the desktop browser and the phone.
package web

import "embed"

// FS holds desktop.html, phone.html and the static/ assets they load.
//
//go:embed desktop.html phone.html static
var FS embed.FS
