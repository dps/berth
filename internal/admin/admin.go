// Package admin holds the admin page's static files (HTML, CSS, JS), served
// by berthd behind sign-in. No build step and no CDN: the files here are
// what the browser gets.
package admin

import "embed"

//go:embed ui
var UI embed.FS
