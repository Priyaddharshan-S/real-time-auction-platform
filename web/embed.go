// Package web embeds the frontend so the whole app is one binary.
package web

import "embed"

//go:embed *.html *.css *.js
var FS embed.FS
