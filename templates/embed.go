// Package templates holds the plain-text files Berth renders: Dockerfiles
// for detected stacks and launchd plists. They are readable (and copyable)
// on purpose; `berth dockerfile <app>` prints the rendered result.
package templates

import "embed"

//go:embed *.Dockerfile *.plist
var FS embed.FS
